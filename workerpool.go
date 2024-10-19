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
	// Function for serving server connections.
	// It must leave c unclosed.
	WorkerFunc ServeHandler

	MaxWorkersCount int

	LogAllErrors bool
	mustStop     bool

	MaxIdleWorkerDuration time.Duration
	cleanThresholdFunc    CleanThresholdFunc

	Logger Logger

	lock         sync.Mutex
	workersCount int

	ready []*workerChan

	stopCh chan struct{}

	workerChanPool sync.Pool

	connState   func(net.Conn, ConnState)
	open        *int32
	concurrency *uint32
}

type workerChan struct {
	lastUseTime time.Time
	ch          chan net.Conn
}

// CleanThresholdFunc t1, t2, t3 and t4 are 1/8 1/4 1/2 3/4 workerChan's lastUseTime respectively.
// cleanNum is the number of workerChan to clean. zero represents no op.
type CleanThresholdFunc func(t1, t2, t3, t4 *time.Time, readyLen, maxWorkersCount int, maxIdleWorkerDuration time.Duration) (cleanNum int)

var DefaultCleanThresholdFunc = func(t1, t2, t3, t4 *time.Time, readyLen, maxWorkersCount int, maxIdleWorkerDuration time.Duration) (cleanNum int) {
	if readyLen <= 2>>10 {
		return
	}

	now := time.Now()
	if readyLen <= maxWorkersCount>>2 {
		if now.Sub(*t3) > maxIdleWorkerDuration {
			return readyLen >> 2
		}
		if now.Sub(*t4) > maxIdleWorkerDuration {
			return readyLen >> 1
		}
		return
	}

	if readyLen <= maxWorkersCount>>1 {
		if now.Sub(*t2) > maxIdleWorkerDuration {
			return readyLen >> 2
		}
		if now.Sub(*t3) > maxIdleWorkerDuration {
			return readyLen >> 1
		}
		return
	}

	if readyLen <= maxWorkersCount {
		if now.Sub(*t3) > maxIdleWorkerDuration {
			return readyLen >> 1
		}
		return
	}

	return 0
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
		var scratch []*workerChan
		for {
			wp.clean(&scratch)
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
	wp.lock.Lock()
	ready := wp.ready
	for i := range ready {
		ready[i].ch <- nil
		ready[i] = nil
	}
	wp.ready = ready[:0]
	wp.mustStop = true
	wp.lock.Unlock()
}

func (wp *workerPool) getMaxIdleWorkerDuration() time.Duration {
	if wp.MaxIdleWorkerDuration <= 0 {
		return 10 * time.Second
	}
	return wp.MaxIdleWorkerDuration
}

// clean specific number of workerChan in ready list, according cleanThresholdFunc method return value.
func (wp *workerPool) clean(scratch *[]*workerChan) {
	wp.lock.Lock()
	ready := wp.ready
	n := len(ready)
	cleanNum := 0
	if n > 0 {
		cleanNum = wp.cleanThresholdFunc(&ready[n>>3].lastUseTime, &ready[n>>2].lastUseTime, &ready[n>>1].lastUseTime, &ready[n*3/4].lastUseTime, n, wp.MaxWorkersCount, wp.MaxIdleWorkerDuration)
	}
	if cleanNum == 0 {
		wp.lock.Unlock()
		return
	}
	if cap(*scratch) < cleanNum {
		*scratch = make([]*workerChan, cleanNum)
	} else {
		*scratch = (*scratch)[:cleanNum]
	}
	copy(*scratch, ready)
	m := copy(ready, ready[cleanNum:])
	for i := m; i < n; i++ {
		ready[i] = nil
	}
	wp.ready = ready[:m]
	wp.lock.Unlock()

	// Notify obsolete workers to stop.
	// This notification must be outside the wp.lock, since ch.ch
	// may be blocking and may consume a lot of time if many workers
	// are located on non-local CPUs.
	tmp := *scratch
	for i := range tmp {
		tmp[i].ch <- nil
		tmp[i] = nil
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
	var ch *workerChan
	createWorker := false

	wp.lock.Lock()
	ready := wp.ready
	n := len(ready) - 1
	if n < 0 {
		if wp.workersCount < wp.MaxWorkersCount {
			createWorker = true
			wp.workersCount++
		}
	} else {
		ch = ready[n]
		ready[n] = nil
		wp.ready = ready[:n]
	}
	wp.lock.Unlock()

	if ch == nil {
		if !createWorker {
			return nil
		}
		vch := wp.workerChanPool.Get()
		ch = vch.(*workerChan)
		go func() {
			wp.workerFunc(ch)
			wp.workerChanPool.Put(vch)
		}()
	}
	return ch
}

func (wp *workerPool) release(ch *workerChan) bool {
	ch.lastUseTime = time.Now()
	wp.lock.Lock()
	if wp.mustStop {
		wp.lock.Unlock()
		return false
	}
	wp.ready = append(wp.ready, ch)
	wp.lock.Unlock()
	return true
}

func (wp *workerPool) workerFunc(ch *workerChan) {
	var c net.Conn

	var err error
	for c = range ch.ch {
		if c == nil {
			break
		}
		// here c's status is really StateNew.
		atomic.AddUint32(wp.concurrency, 1)
		wp.connState(c, StateNew)
		//log.Println(c.RemoteAddr(), "->", c.LocalAddr())
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
		atomic.AddInt32(wp.open, -1)
		atomic.AddUint32(wp.concurrency, ^uint32(0))
		if err == errHijacked {
			wp.connState(c, StateHijacked)
		} else {
			//goland:noinspection GoUnhandledErrorResult
			c.Close()
			wp.connState(c, StateClosed)
		}
		if !wp.release(ch) {
			break
		}
	}

	wp.lock.Lock()
	wp.workersCount--
	wp.lock.Unlock()
}
