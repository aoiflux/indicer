package testutil

import (
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// NoLeakedGoroutines records the running goroutines and, when the test ends,
// fails it if new ones are still running after a grace period. Goroutines the
// runtime and testing package own are ignored, as are any whose stack
// contains one of the ignore substrings.
//
// It must not be used with t.Parallel: other tests' goroutines would count.
func NoLeakedGoroutines(tb testing.TB, ignore ...string) {
	tb.Helper()
	before := goroutineIDs()
	tb.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		var leaked []string
		for {
			leaked = leaked[:0]
			for id, stack := range goroutineStacks() {
				if before[id] || benign(stack, ignore) {
					continue
				}
				leaked = append(leaked, stack)
			}
			if len(leaked) == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(leaked) > 0 {
			slices.Sort(leaked)
			tb.Errorf("%d goroutine(s) leaked:\n\n%s", len(leaked), strings.Join(leaked, "\n\n"))
		}
	})
}

var benignFrames = []string{
	"testing.(*T).Run",
	"testing.tRunner",
	"testing.(*M).",
	"runtime.goexit\n\truntime/asm",
	"os/signal.signal_recv",
	"os/signal.loop",
	"runtime.ensureSigM",
	"internal/poll.runtime_pollWait",
	"testing.runFuzzing",
}

func benign(stack string, extra []string) bool {
	// The goroutine running the cleanup itself.
	if strings.Contains(stack, "testutil.NoLeakedGoroutines.func1") {
		return true
	}
	first := stack
	if i := strings.Index(stack, "\n"); i >= 0 {
		first = stack[i+1:]
	}
	for _, f := range benignFrames {
		if strings.HasPrefix(strings.TrimSpace(first), f) {
			return true
		}
	}
	for _, s := range extra {
		if strings.Contains(stack, s) {
			return true
		}
	}
	return false
}

func goroutineIDs() map[string]bool {
	ids := map[string]bool{}
	for id := range goroutineStacks() {
		ids[id] = true
	}
	return ids
}

// goroutineStacks returns each goroutine's stack keyed by its "goroutine N"
// header.
func goroutineStacks() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[string]string{}
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		header, _, _ := strings.Cut(g, " [")
		if strings.HasPrefix(header, "goroutine ") {
			out[header] = g
		}
	}
	return out
}

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
