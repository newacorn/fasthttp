package fasthttp

import (
	"github.com/newacorn/goutils/unsafefn"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

var maxWhen = time.Hour * 365 * 24 * 10
var defaultClientCleaner = DurationCleaner{
	stopChan:    make(chan struct{}),
	t:           time.NewTimer(maxWhen),
	minDuration: time.Millisecond * 500,
}

func registerCleanableItem(client CleanableItem, duration time.Duration) {
	defaultClientCleaner.Register(client, duration)
}
func stopDurationCleaner() {
	defaultClientCleaner.Stop()
}

type DurationCleaner struct {
	items      []*CleanItem
	mu         sync.Mutex
	zeroWhen   atomic.Int64
	cleanStart atomic.Uint32
	numItems   atomic.Uint32
	stop       atomic.Bool
	stopChan   chan struct{}
	t          *time.Timer
	// The minimum runtime interval for the same CleanableItem
	minDuration time.Duration
}

type CleanableItem interface {
	Clean() (duration time.Duration, stop bool)
}

type CleanItem struct {
	client CleanableItem
	when   int64
}

func (cc *DurationCleaner) Stop() {
	if cc.stop.Load() {
		return
	}
	cc.mu.Lock()
	cc.stop.Store(true)
	cc.stopChan <- struct{}{}
	cc.mu.Unlock()
}

func (cc *DurationCleaner) Register(client CleanableItem, duration time.Duration) {
	if client == nil {
		return
	}
	var item *CleanItem
	when := unsafefn.NanoTime() + int64(duration)
	cc.mu.Lock()
	l := len(cc.items)
	if cap(cc.items) > l {
		cc.items = cc.items[:l+1]
		item = cc.items[l]
		if item == nil {
			item = &CleanItem{client: client, when: when}
			cc.items[l] = item
		} else {
			item.client = client
			item.when = when
		}
	}
	if item == nil {
		item = &CleanItem{client: client, when: when}
		cc.items = append(cc.items, item)
	}
	doAdd(cc, item)
	if cc.cleanStart.Load() == 0 {
		go func() {
			cc.startClen()
		}()
		cc.cleanStart.Store(1)
	}
	cc.mu.Unlock()
}

func (cc *DurationCleaner) startClen() {
	for {
		select {
		case <-cc.t.C:
			cc.mu.Lock()
			now := unsafefn.NanoTime()
			pollUntil, _ := checkItems(cc, now)
			if pollUntil > 0 {
				du := pollUntil - now
				if du <= 0 {
					panic("never happened")
				}
				cc.t.Reset(time.Duration(du))
			}
			cc.mu.Unlock()
		case <-cc.stopChan:
			return
		}
	}
}

func badHeap() {
	panic("client data corruption")
}

func siftUp(items []*CleanItem, i int) int {
	if i >= len(items) {
		badHeap()
	}
	when := items[i].when
	if when <= 0 {
		badHeap()
	}
	tmp := items[i]
	for i > 0 {
		p := (i - 1) / 4 // parent
		if when >= items[p].when {
			break
		}
		items[i] = items[p]
		i = p
	}
	if tmp != items[i] {
		items[i] = tmp
	}
	return i
}

func siftDown(items []*CleanItem, i int) {
	n := len(items)
	if i >= n {
		badHeap()
	}
	when := items[i].when
	if when <= 0 {
		badHeap()
	}
	tmp := items[i]
	for {
		c := i*4 + 1 // left child
		c3 := c + 2  // mid child
		if c >= n {
			break
		}
		w := items[c].when
		if c+1 < n && items[c+1].when < w {
			w = items[c+1].when
			c++
		}
		if c3 < n {
			w3 := items[c3].when
			if c3+1 < n && items[c3+1].when < w3 {
				w3 = items[c3+1].when
				c3++
			}
			if w3 < w {
				w = w3
				c = c3
			}
		}
		if w >= when {
			break
		}
		items[i] = items[c]
		i = c
	}
	if tmp != items[i] {
		items[i] = tmp
	}
}

func runOne(cc *DurationCleaner, item *CleanItem, now int64) {
	duration, stop := item.client.Clean()
	if duration < cc.minDuration {
		duration = cc.minDuration
	}
	item.when = int64(duration) + unsafefn.NanoTime()
	if stop {
		doDel0(cc)
		return
	}
	siftDown(cc.items, 0)
}

func run(cc *DurationCleaner, now int64) int64 {
	t := cc.items[0]
	if t.when > now {
		return t.when
	}
	runOne(cc, t, now)
	return 0
}

func doDel0(cc *DurationCleaner) {
	last := len(cc.items) - 1
	if last > 0 {
		cc.items[0], cc.items[last] = cc.items[last], cc.items[0]
	}
	cc.items[last].client = nil
	cc.items = cc.items[:last]
	if last > 0 {
		siftDown(cc.items, 0)
	}
	update0When(cc)
	cc.numItems.Add(math.MaxUint32)
}

func update0When(pp *DurationCleaner) {
	if len(pp.items) == 0 {
		pp.zeroWhen.Store(0)
	} else {
		if pp.zeroWhen.Load() != pp.items[0].when {
			du := pp.items[0].when - unsafefn.NanoTime()
			pp.zeroWhen.Store(pp.items[0].when)
			if du < 0 {
				du = 0
			}
			pp.t.Reset(time.Duration(du))
		}
	}
}

func checkItems(cc *DurationCleaner, now int64) (pollUntil int64, ran bool) {
	next := cc.zeroWhen.Load()
	if now < next {
		return next, false
	}
	if len(cc.items) > 0 {
		for len(cc.items) > 0 {
			if tw := run(cc, now); tw != 0 {
				if tw > 0 {
					pollUntil = tw
				}
				break
			}
			ran = true
		}
	}
	return
}

func doAdd(cc *DurationCleaner, c *CleanItem) {
	cc.numItems.Add(1)
	siftUp(cc.items, len(cc.items)-1)
	if c == cc.items[0] {
		update0When(cc)
	}
	return
}
