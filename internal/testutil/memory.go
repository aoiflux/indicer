package testutil

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// AllocBelow runs fn and fails if the heap it allocated in total exceeds
// limit bytes. It measures cumulative allocation, so it bounds work done, not
// only what is live at the end.
func AllocBelow(tb testing.TB, limit uint64, fn func()) {
	tb.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > limit {
		tb.Fatalf("allocated %d bytes, limit %d", got, limit)
	}
}

// PeakHeap runs fn while sampling the live heap and returns the largest
// value seen, in bytes. Sampling is every millisecond, so short spikes can be
// missed; use it for bounds with headroom, not exact figures.
func PeakHeap(fn func()) uint64 {
	var (
		mu   sync.Mutex
		peak uint64
		done = make(chan struct{})
		wg   sync.WaitGroup
	)
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		mu.Lock()
		if m.HeapAlloc > peak {
			peak = m.HeapAlloc
		}
		mu.Unlock()
	}
	runtime.GC()
	wg.Go(func() {
		t := time.NewTicker(time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sample()
			}
		}
	})
	fn()
	sample()
	close(done)
	wg.Wait()
	return peak
}
