// Package workpool runs a list of items on a fixed number of goroutines,
// for work whose items are independent except where the caller says they
// are not, and whose results must read as if the items ran one by one.
//
//   - Items start in index order. An item waits for the earlier items of its
//     chain (equal Key), so content that may be the same is never worked on
//     twice at once.
//   - A Heavy item runs alone: it starts when nothing else runs and done has
//     seen every item before it, and nothing starts while it runs. Until it
//     starts, no later item does, so it is never starved.
//   - done is called in index order, one call at a time, as soon as every
//     item before has been done: a caller printing there prints what a run
//     of one goroutine would.
//   - When ctx ends, no item starts. Run waits for the running ones, calls
//     done for every item that ran, in index order, and returns ctx's cause.
//   - A panic in an item stops the run the same way, and Run returns it as a
//     *safe.PanicError.
//   - With RunHeld, the caller can hold back every item but the first not yet
//     finished, which always may start, so the run never stalls.
//
// Run starts exactly jobs goroutines, however many items there are, and
// says how they spent the run (Stats).
package workpool

import (
	"context"
	"runtime/trace"
	"sync"
	"time"

	"indicer/internal/safe"
)

// Item describes one item to the scheduler.
type Item struct {
	// Key chains items: those with the same non-empty key run one after
	// another, in index order. "" chains with nothing.
	Key string
	// Heavy runs the item alone.
	Heavy bool
}

// Stats is how the goroutines of a run spent it, one entry each.
type Stats struct {
	// Wall is how long the run took.
	Wall time.Duration
	// Busy is the time each goroutine spent in items.
	Busy []time.Duration
	// Held is the time each goroutine waited with items left to start, all
	// held back: by the earlier items of their chains, by a heavy item,
	// which waits for the running ones and runs alone, or by RunHeld's hold.
	Held []time.Duration
}

// Utilization is the share of the goroutines' time spent in items, from 0 to
// 1; 0 for a run that took no time.
func (s Stats) Utilization() float64 {
	if s.Wall <= 0 || len(s.Busy) == 0 {
		return 0
	}
	var busy time.Duration
	for _, b := range s.Busy {
		busy += b
	}
	return float64(busy) / (float64(s.Wall) * float64(len(s.Busy)))
}

// Run runs do for each of items, on at most jobs goroutines (at least 1),
// and calls done for each that ran, as the package describes.
func Run(ctx context.Context, jobs int, items []Item, do func(ctx context.Context, i int) error, done func(i int, err error)) (Stats, error) {
	return RunHeld(ctx, jobs, items, nil, do, done)
}

// RunHeld is Run, but while hold reports true no item starts except the
// first not yet finished: a caller whose finished items keep something for
// the earlier ones bounds it so. hold is asked before an item starts, and
// again once an item finishes and once done returns; nil never holds.
func RunHeld(ctx context.Context, jobs int, items []Item, hold func() bool, do func(ctx context.Context, i int) error, done func(i int, err error)) (Stats, error) {
	return run(ctx, jobs, items, hold, nil, do, done)
}

// run is RunHeld. holding, when not nil, is called under the scheduler's
// lock each time a goroutine starts to wait for an item; tests pass it to
// know one waits.
func run(ctx context.Context, jobs int, items []Item, hold func() bool, holding func(), do func(ctx context.Context, i int) error, done func(i int, err error)) (Stats, error) {
	jobs = max(jobs, 1)
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	s := newScheduler(items, jobs)
	s.hold = hold
	s.holding = holding
	busy := make([]time.Duration, jobs)
	start := time.Now()
	var wg sync.WaitGroup
	for g := range jobs {
		wg.Go(func() {
			for {
				i, ok := s.next(runCtx, g)
				if !ok {
					return
				}
				began := time.Now()
				err := safe.Call(safe.Site{Component: "workpool", Op: "item"}, func() error { return do(runCtx, i) })
				busy[g] += time.Since(began)
				if safe.IsPanic(err) {
					stop(err)
				}
				s.finish(i, err, done)
			}
		})
	}
	wg.Wait()
	st := Stats{Wall: time.Since(start), Busy: busy, Held: s.held}
	s.flush(done)
	return st, context.Cause(runCtx)
}

// state is where one item is.
type state uint8

const (
	waiting state = iota
	running
	finished
	reported
)

type scheduler struct {
	items []Item
	mu    sync.Mutex
	cond  *sync.Cond
	state []state
	errs  []error
	// prev is the index of the item before i in its chain, or -1.
	prev []int
	// first is the lowest index not yet started; reported, the lowest not
	// yet given to done.
	first, reportedTo int
	busy              int // items running
	heavy             bool
	stopped           bool
	// unfinished is the lowest index not yet finished, the one item hold
	// never holds back.
	unfinished int
	hold       func() bool
	holding    func() // see run; nil for none
	// held is how long each goroutine waited for an item to start.
	held []time.Duration
}

func newScheduler(items []Item, jobs int) *scheduler {
	s := &scheduler{items: items, state: make([]state, len(items)), errs: make([]error, len(items)), prev: make([]int, len(items)), held: make([]time.Duration, jobs)}
	s.cond = sync.NewCond(&s.mu)
	last := map[string]int{}
	for i, it := range items {
		s.prev[i] = -1
		if it.Key == "" {
			continue
		}
		if p, ok := last[it.Key]; ok {
			s.prev[i] = p
		}
		last[it.Key] = i
	}
	return s
}

// next blocks until an item may start, and returns it to goroutine g; ok is
// false when none will: every item started, or ctx ended.
func (s *scheduler) next(ctx context.Context, g int) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		// Wake every waiter when ctx ends, so they can leave.
		stopWatch := context.AfterFunc(ctx, func() {
			s.mu.Lock()
			s.stopped = true
			s.mu.Unlock()
			s.cond.Broadcast()
		})
		defer stopWatch()
	}
	for {
		if s.stopped || ctx.Err() != nil {
			s.stopped = true
			return 0, false
		}
		if i, ok := s.ready(); ok {
			s.state[i] = running
			s.busy++
			if s.items[i].Heavy {
				s.heavy = true
			}
			for s.first < len(s.items) && s.state[s.first] != waiting {
				s.first++
			}
			return i, true
		}
		if s.first >= len(s.items) {
			return 0, false
		}
		if s.holding != nil {
			s.holding()
		}
		waited := time.Now()
		region := trace.StartRegion(ctx, "held")
		s.cond.Wait()
		region.End()
		s.held[g] += time.Since(waited)
	}
}

// ready finds the lowest waiting item that may start now.
func (s *scheduler) ready() (int, bool) {
	if s.heavy {
		return 0, false
	}
	held := s.hold != nil && s.hold()
	for i := s.first; i < len(s.items); i++ {
		if s.state[i] != waiting {
			continue
		}
		if held && i != s.unfinished {
			return 0, false
		}
		if s.items[i].Heavy {
			// Alone, and nothing after it starts first. It waits too for done
			// to have seen every item before it, so it runs with nothing of
			// theirs left to settle (a heavy item's pass then never takes a
			// shared ledger's lock).
			return i, s.busy == 0 && s.chainFree(i) && s.reportedTo >= i
		}
		if s.chainFree(i) {
			return i, true
		}
	}
	return 0, false
}

// chainFree reports whether the item before i in its chain is done.
func (s *scheduler) chainFree(i int) bool {
	p := s.prev[i]
	return p < 0 || s.state[p] >= finished
}

// finish records item i's end and gives done every item now in order.
func (s *scheduler) finish(i int, err error, done func(int, error)) {
	s.mu.Lock()
	s.state[i], s.errs[i] = finished, err
	s.busy--
	for s.unfinished < len(s.items) && s.state[s.unfinished] >= finished {
		s.unfinished++
	}
	if s.items[i].Heavy {
		s.heavy = false
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	s.report(done, false)
}

// flush gives done what finished after the run stopped, skipping the items
// that never started.
func (s *scheduler) flush(done func(int, error)) { s.report(done, true) }

// report calls done for each finished item from reportedTo on, in order,
// one caller at a time; with all, past items that never started.
func (s *scheduler) report(done func(int, error), all bool) {
	for {
		s.mu.Lock()
		for all && s.reportedTo < len(s.items) && s.state[s.reportedTo] == waiting {
			s.reportedTo++
		}
		if s.reportedTo >= len(s.items) || s.state[s.reportedTo] != finished {
			s.mu.Unlock()
			return
		}
		i := s.reportedTo
		s.state[i] = reported
		err := s.errs[i]
		// reportedTo moves only after done returns, so calls never overlap
		// and stay in order.
		s.mu.Unlock()
		done(i, err)
		s.mu.Lock()
		s.reportedTo++
		s.mu.Unlock()
		// A heavy item may wait for this report, and done may have let go of
		// what held the others back.
		s.cond.Broadcast()
	}
}
