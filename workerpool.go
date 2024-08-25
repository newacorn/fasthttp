package fasthttp

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// workerPool serves incoming connections via a pool of workers
// in FILO order, i.e. the most recently stopped worker will serve the next
// incoming connection.
//
// Such a scheme keeps CPU caches hot (in theory).
type workerPool struct {
	workerChanPool sync.Pool

	Logger Logger

	// Function for serving server connections.
	// It must leave c unclosed.
	ready      workerChanStack
	WorkerFunc ServeHandler

	stopCh chan struct{}

	connState func(net.Conn, ConnState)

	MaxWorkersCount int

	MaxIdleWorkerDuration time.Duration

	workersCount int32

	mustStop atomic.Bool

	LogAllErrors bool
}

type workerChan struct {
	next *workerChan

	ch chan net.Conn

	lastUseTime int64
}

type workerChanStack struct {
	head atomic.Pointer[workerChan]
}

func (s *workerChanStack) push(ch *workerChan) {
	for {
		oldHead := s.head.Load()
		ch.next = oldHead
		if s.head.CompareAndSwap(oldHead, ch) {
			break
		}
	}
}

func (s *workerChanStack) pop() *workerChan {
	for {
		oldHead := s.head.Load()
		if oldHead == nil {
			return nil
		}

		if s.head.CompareAndSwap(oldHead, oldHead.next) {
			return oldHead
		}
	}
}

func (wp *workerPool) Start() {
	if wp.stopCh != nil {
		return
	}
	wp.stopCh = make(chan struct{})
	stopCh := wp.stopCh
	wp.workerChanPool.New = func() any {
		return &workerChan{
			ch: make(chan net.Conn, workerChanCap),
		}
	}
	go func() {
		for {
			wp.clean()
			select {
			case <-stopCh:
				return
			default:
				time.Sleep(wp.getMaxIdleWorkerDuration())
			}
		}
	}()
}

func (wp *workerPool) Stop() {
	if wp.stopCh == nil {
		return
	}
	close(wp.stopCh)
	wp.stopCh = nil

	// Stop all the workers waiting for incoming connections.
	// Do not wait for busy workers - they will stop after
	// serving the connection and noticing wp.mustStop = true.

	for {
		ch := wp.ready.pop()
		if ch == nil {
			break
		}
		ch.ch <- nil
	}
	wp.mustStop.Store(true)
}

func (wp *workerPool) getMaxIdleWorkerDuration() time.Duration {
	if wp.MaxIdleWorkerDuration <= 0 {
		return 10 * time.Second
	}
	return wp.MaxIdleWorkerDuration
}

func (wp *workerPool) clean() {
	maxIdleWorkerDuration := wp.getMaxIdleWorkerDuration()
	criticalTime := time.Now().Add(-maxIdleWorkerDuration).UnixNano()

	for {
		current := wp.ready.head.Load()
		if current == nil || atomic.LoadInt64(&current.lastUseTime) >= criticalTime {
			break
		}

		next := current.next
		if wp.ready.head.CompareAndSwap(current, next) {
			current.ch <- nil
		}
	}
}

func (wp *workerPool) Serve(c net.Conn) bool {
	ch := wp.getCh()
	if ch == nil {
		return false
	}
	ch.ch <- c
	return true
}

var workerChanCap = func() int {
	// Use blocking workerChan if GOMAXPROCS=1.
	// This immediately switches Serve to WorkerFunc, which results
	// in higher performance (under go1.5 at least).
	if runtime.GOMAXPROCS(0) == 1 {
		return 0
	}

	// Use non-blocking workerChan if GOMAXPROCS>1,
	// since otherwise the Serve caller (Acceptor) may lag accepting
	// new connections if WorkerFunc is CPU-bound.
	return 1
}()

func (wp *workerPool) getCh() *workerChan {
	for {
		ch := wp.ready.pop()
		if ch != nil {
			return ch
		}

		currentWorkers := atomic.LoadInt32(&wp.workersCount)
		if currentWorkers < int32(wp.MaxWorkersCount) {
			if atomic.CompareAndSwapInt32(&wp.workersCount, currentWorkers, currentWorkers+1) {
				ch = wp.workerChanPool.Get().(*workerChan)
				go func() {
					wp.workerFunc(ch)
					wp.workerChanPool.Put(ch)
				}()
				return ch
			}
		} else {
			break
		}
	}
	return nil
}

func (wp *workerPool) release(ch *workerChan) bool {
	atomic.StoreInt64(&ch.lastUseTime, time.Now().UnixNano())
	if wp.mustStop.Load() {
		return false
	}
	wp.ready.push(ch)
	return true
}

func (wp *workerPool) workerFunc(ch *workerChan) {
	var c net.Conn

	var err error
	for c = range ch.ch {
		if c == nil {
			break
		}

		if err = wp.WorkerFunc(c); err != nil && err != errHijacked {
			errStr := err.Error()
			if wp.LogAllErrors || !(strings.Contains(errStr, "broken pipe") ||
				strings.Contains(errStr, "reset by peer") ||
				strings.Contains(errStr, "request headers: small read buffer") ||
				strings.Contains(errStr, "unexpected EOF") ||
				strings.Contains(errStr, "i/o timeout") ||
				errors.Is(err, ErrBadTrailer)) {
				wp.Logger.Printf("error when serving connection %q<->%q: %v", c.LocalAddr(), c.RemoteAddr(), err)
			}
		}
		if err == errHijacked {
			wp.connState(c, StateHijacked)
		} else {
			_ = c.Close()
			wp.connState(c, StateClosed)
		}

		if !wp.release(ch) {
			break
		}
	}

	atomic.AddInt32(&wp.workersCount, -1)
}
