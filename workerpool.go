package fasthttp

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"
)

// workerPool serves incoming connections via a pool of workers
// in FILO order, i.e. the most recently stopped worker will serve the next
// incoming connection.
//
// Such a scheme keeps CPU caches hot (in theory).
// 每个worker在单独的一个协程中执行关联一个 workerChan，使用for循环阻塞在
// workerChan.ch 上。
//
// 当 Server.Serve 从net.Listener中获取新的net.Conn时
// 会创建一个新的worker或者从ready队列中取旧的worker，将net.Conn
// 发送到worker关联的 workerChan。
type workerPool struct {
	// Function for serving server connections.
	// It must leave c unclosed.
	//
	// 设置为Server.serveConn
	WorkerFunc ServeHandler
	// 设置为Server.getConcurrency的返回值。
	MaxWorkersCount int
	// 设置为 Server.LogAllErrors字段
	LogAllErrors bool
	// 设置为 Server.MaxIdleWorkerDuration
	// 默认为10s
	MaxIdleWorkerDuration time.Duration
	// 设置为Server.logger()的返回值
	Logger Logger

	// 保护ready字段的读写
	lock sync.Mutex
	// 当前正在服务于net.Conn的workerChan实例的数量。
	workersCount int
	// 当从net.Listener中接收连接时遇到错误会将此字段设置为true。
	// 其表示fastHttp服务应当停止。
	//
	// workerFunc 的for循环中，在服务完一个net.Conn后，会调用
	// release 方法将workerChan放回ready列表中，在release方法中，
	// 如果检查此字段为true会返回false，release返回false导致上面的for循环退出。
	// 从而会让关联的worker协程退出。
	mustStop bool

	// 空闲workerChan列表
	ready []*workerChan

	// 从net.Listener中获取连接时，如果返回错误则会调用 Stop 方法
	// 其会关闭此通道。
	//
	// 此通道的关闭用于通知 Start 方法启动的用于清除worker(超过 MaxIdleWorkerDuration)的协程退出。
	stopCh chan struct{}
	// workerChan 缓冲池
	workerChanPool sync.Pool

	// 设置为 Server.setState
	// 其内部调用 Server.ConnState 字段设置的钩子函数。
	connState func(net.Conn, ConnState)
}

type workerChan struct {
	lastUseTime time.Time
	ch          chan net.Conn
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

// Stop 当从ne.Listener中获取连接遇到错误时，会调用此方法。
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

func (wp *workerPool) clean(scratch *[]*workerChan) {
	maxIdleWorkerDuration := wp.getMaxIdleWorkerDuration()

	// Clean least recently used workers if they didn't serve connections
	// for more than maxIdleWorkerDuration.
	criticalTime := time.Now().Add(-maxIdleWorkerDuration)

	wp.lock.Lock()
	ready := wp.ready
	n := len(ready)

	// Use binary-search algorithm to find out the index of the least recently worker which can be cleaned up.
	l, r := 0, n-1
	for l <= r {
		mid := (l + r) / 2
		if criticalTime.After(wp.ready[mid].lastUseTime) {
			l = mid + 1
		} else {
			r = mid - 1
		}
	}
	i := r
	if i == -1 {
		wp.lock.Unlock()
		return
	}

	*scratch = append((*scratch)[:0], ready[:i+1]...)
	m := copy(ready, ready[i+1:])
	for i = m; i < n; i++ {
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

		// [[Server.serverConn]]
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

	wp.lock.Lock()
	wp.workersCount--
	wp.lock.Unlock()
}
