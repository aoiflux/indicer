package workpool

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"indicer/internal/safe"
)

// recorder checks what a run did: done's order, and which items ran at once.
type recorder struct {
	mu      sync.Mutex
	order   []int
	running map[int]bool
	maxBusy int
	clash   string
}

func newRecorder() *recorder { return &recorder{running: map[int]bool{}} }

func (r *recorder) enter(i int, items []Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for j := range r.running {
		switch {
		case items[i].Heavy || items[j].Heavy:
			r.clash = fmt.Sprintf("heavy item ran beside another: %d and %d", i, j)
		case items[i].Key != "" && items[i].Key == items[j].Key:
			r.clash = fmt.Sprintf("items %d and %d of chain %q ran at once", i, j, items[i].Key)
		}
	}
	r.running[i] = true
	r.maxBusy = max(r.maxBusy, len(r.running))
}

func (r *recorder) leave(i int) {
	r.mu.Lock()
	delete(r.running, i)
	r.mu.Unlock()
}

func (r *recorder) done(i int, _ error) {
	r.mu.Lock()
	r.order = append(r.order, i)
	r.mu.Unlock()
}

// TestOrderChainsAndHeavyItems: whatever order items finish in, done sees
// them in index order; chained items never overlap, nor a heavy item with
// anything; and no more than jobs run at once.
func TestOrderChainsAndHeavyItems(t *testing.T) {
	items := make([]Item, 200)
	for i := range items {
		switch {
		case i%37 == 5:
			items[i].Heavy = true
		case i%5 == 0:
			items[i].Key = fmt.Sprint("chain", i%3)
		}
	}
	r := newRecorder()
	rnd := rand.New(rand.NewPCG(1, 2))
	delays := make([]time.Duration, len(items))
	for i := range delays {
		delays[i] = time.Duration(rnd.IntN(2000)) * time.Microsecond
	}
	_, err := Run(t.Context(), 4, items, func(_ context.Context, i int) error {
		r.enter(i, items)
		time.Sleep(delays[i])
		r.leave(i)
		return nil
	}, r.done)
	if err != nil {
		t.Fatal(err)
	}
	if r.clash != "" {
		t.Fatal(r.clash)
	}
	if len(r.order) != len(items) {
		t.Fatalf("done called %d times, want %d", len(r.order), len(items))
	}
	for i, got := range r.order {
		if got != i {
			t.Fatalf("done call %d was item %d: not in index order", i, got)
		}
	}
	if r.maxBusy > 4 || r.maxBusy < 2 {
		t.Fatalf("at most %d items ran at once, want 2 to 4", r.maxBusy)
	}
}

// TestHeavyItemIsNotStarved: a heavy item waits only for those already
// running; items after it wait for it.
func TestHeavyItemIsNotStarved(t *testing.T) {
	items := make([]Item, 50)
	items[3].Heavy = true
	var started []int
	var mu sync.Mutex
	_, err := Run(t.Context(), 4, items, func(_ context.Context, i int) error {
		mu.Lock()
		started = append(started, i)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		return nil
	}, func(int, error) {})
	if err != nil {
		t.Fatal(err)
	}
	for k, i := range started {
		if i == 3 {
			for _, j := range started[:k] {
				if j > 3 {
					t.Fatalf("item %d started before the heavy item 3: %v", j, started)
				}
			}
		}
	}
}

// TestHeavyStartsAfterEarlierReported: a heavy item starts only once done
// has returned for every item before it, so what done settles for them is
// settled before the heavy one runs, however slow done is.
func TestHeavyStartsAfterEarlierReported(t *testing.T) {
	items := make([]Item, 6)
	items[3].Heavy = true
	var reported atomic.Int32
	var early atomic.Int32
	_, err := Run(t.Context(), 4, items, func(_ context.Context, i int) error {
		if i == 3 {
			if n := reported.Load(); n != 3 {
				early.Store(n + 1)
			}
		}
		return nil
	}, func(i int, _ error) {
		if i < 3 {
			// Slow, so a heavy item woken by the last finish would start
			// while done still runs.
			time.Sleep(20 * time.Millisecond)
		}
		reported.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := early.Load(); n != 0 {
		t.Fatalf("heavy item 3 started after done returned for %d items, want 3", n-1)
	}
}

// TestErrorsDoNotStopOthers: an item's error goes to done and the rest run.
func TestErrorsDoNotStopOthers(t *testing.T) {
	items := make([]Item, 20)
	errs := make([]error, len(items))
	_, err := Run(t.Context(), 3, items, func(_ context.Context, i int) error {
		if i%4 == 1 {
			return fmt.Errorf("item %d failed", i)
		}
		return nil
	}, func(i int, err error) { errs[i] = err })
	if err != nil {
		t.Fatalf("Run = %v, want nil: item errors are the items'", err)
	}
	for i, e := range errs {
		if (i%4 == 1) != (e != nil) {
			t.Fatalf("item %d: error %v", i, e)
		}
	}
}

// TestCancelStopsStarting: after ctx ends, nothing starts; running items
// finish, done hears of every item that ran, in order, and Run returns the
// cause.
func TestCancelStopsStarting(t *testing.T) {
	cause := errors.New("stop now")
	ctx, cancel := context.WithCancelCause(t.Context())
	items := make([]Item, 100)
	var ran atomic.Int64
	var reported []int
	_, err := Run(ctx, 4, items, func(ctx context.Context, i int) error {
		ran.Add(1)
		if i == 2 {
			cancel(cause)
		}
		<-ctx.Done()
		return ctx.Err()
	}, func(i int, _ error) { reported = append(reported, i) })
	if !errors.Is(err, cause) {
		t.Fatalf("Run = %v, want %v", err, cause)
	}
	// The items running when item 2 cancelled: at most the four workers'.
	if n := ran.Load(); n > 4 || int(n) != len(reported) {
		t.Fatalf("%d items ran, %d reported; want at most 4, all reported", n, len(reported))
	}
	for k := 1; k < len(reported); k++ {
		if reported[k] <= reported[k-1] {
			t.Fatalf("reported out of order: %v", reported)
		}
	}
}

// TestPanicStopsTheRun: a panic in an item stops the run, as a cancel does,
// and Run returns it.
func TestPanicStopsTheRun(t *testing.T) {
	prev := safe.SetStrict(false)
	t.Cleanup(func() { safe.SetStrict(prev) })
	items := make([]Item, 50)
	var ran atomic.Int64
	_, err := Run(t.Context(), 2, items, func(ctx context.Context, i int) error {
		ran.Add(1)
		if i == 5 {
			panic("boom")
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Millisecond):
		}
		return nil
	}, func(int, error) {})
	if !safe.IsPanic(err) {
		t.Fatalf("Run = %v, want the panic", err)
	}
	if ran.Load() >= int64(len(items)) {
		t.Fatal("every item ran after a panic")
	}
}

// TestGoroutinesAreBounded: Run starts jobs goroutines, not one per item.
func TestGoroutinesAreBounded(t *testing.T) {
	items := make([]Item, 10000)
	before := runtime.NumGoroutine()
	var peak atomic.Int64
	_, err := Run(t.Context(), 4, items, func(context.Context, int) error {
		if n := int64(runtime.NumGoroutine() - before); n > peak.Load() {
			peak.Store(n)
		}
		return nil
	}, func(int, error) {})
	if err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p > 8 {
		t.Fatalf("%d goroutines above the start, want about 4", p)
	}
}

// TestStatsSayHowTheRunWasSpent: Busy counts each goroutine's time in items,
// and Held the time one waited while the items left were held back, here by
// the earlier item of their chain.
func TestStatsSayHowTheRunWasSpent(t *testing.T) {
	waits := make(chan struct{})
	var once sync.Once
	holding = func() { once.Do(func() { close(waits) }) }
	t.Cleanup(func() { holding = func() {} })
	const hold = 20 * time.Millisecond
	st, err := Run(t.Context(), 2, []Item{{Key: "a"}, {Key: "a"}}, func(_ context.Context, i int) error {
		if i == 0 {
			// The other goroutine waits for this item, its chain's first.
			<-waits
			time.Sleep(hold)
		}
		return nil
	}, func(int, error) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Busy) != 2 || len(st.Held) != 2 {
		t.Fatalf("stats for %d and %d goroutines, want 2", len(st.Busy), len(st.Held))
	}
	if busy, held := st.Busy[0]+st.Busy[1], st.Held[0]+st.Held[1]; busy < hold || held < hold || st.Wall < hold {
		t.Fatalf("busy %v, held %v in %v; want each at least %v", busy, held, st.Wall, hold)
	}
	if u := st.Utilization(); u <= 0 || u > 1 {
		t.Fatalf("utilization %v, want in (0, 1]", u)
	}

	full := Stats{Wall: 2 * time.Second, Busy: []time.Duration{time.Second, 3 * time.Second}}
	if u := full.Utilization(); u != 1 {
		t.Fatalf("4 s busy of 2 goroutines in 2 s: utilization %v, want 1", u)
	}
	if u := (Stats{Busy: []time.Duration{time.Second}}).Utilization(); u != 0 {
		t.Fatalf("a run that took no time: utilization %v, want 0", u)
	}
}
