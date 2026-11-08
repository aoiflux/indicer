package workpool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHoldLetsOnlyTheFirstUnfinishedStart: while hold reports true, nothing
// starts beside the first item not yet finished, and that one always may,
// so the run ends; done still sees every item, in order.
func TestHoldLetsOnlyTheFirstUnfinishedStart(t *testing.T) {
	t.Parallel()
	waits := make(chan struct{})
	var once sync.Once
	holding := func() { once.Do(func() { close(waits) }) }

	items := make([]Item, 8)
	var busy, most atomic.Int32
	another := make(chan struct{})
	var anotherOnce sync.Once
	do := func(_ context.Context, i int) error {
		n := busy.Add(1)
		defer busy.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		if i > 0 {
			anotherOnce.Do(func() { close(another) })
			return nil
		}
		// Until another goroutine is held back, or starts an item.
		select {
		case <-waits:
		case <-another:
		case <-time.After(10 * time.Second):
			t.Error("no goroutine waited for an item")
		}
		return nil
	}
	rec := newRecorder()
	if _, err := run(t.Context(), 4, items, func() bool { return true }, holding, do, rec.done); err != nil {
		t.Fatal(err)
	}
	if m := most.Load(); m != 1 {
		t.Fatalf("%d items ran at once while held, want 1", m)
	}
	if len(rec.order) != len(items) {
		t.Fatalf("done saw %v, want all %d items", rec.order, len(items))
	}
	for i, got := range rec.order {
		if got != i {
			t.Fatalf("done saw %v, want index order", rec.order)
		}
	}
}
