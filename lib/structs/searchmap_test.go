package structs

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestSeenChonkMapGetOrComputeComputesOnce(t *testing.T) {
	s := NewSeenChonkMap()
	key := []byte("same-key")

	var computeCalls int32
	computeFn := func() int {
		atomic.AddInt32(&computeCalls, 1)
		return 7
	}

	const workers = 64
	var wg sync.WaitGroup
	results := make([]int, workers)

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx] = s.GetOrCompute(key, computeFn)
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&computeCalls); got != 1 {
		t.Fatalf("expected compute to be called once, got %d", got)
	}
	for i, got := range results {
		if got != 7 {
			t.Fatalf("worker %d got %d, expected 7", i, got)
		}
	}
}
