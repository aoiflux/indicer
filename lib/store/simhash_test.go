package store

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"
)

// TestSimhashWriterBoundsInFlight is the STO-13 regression. enqueue copied
// the chunk and started a goroutine before taking a worker slot, so while
// the workers were busy every further chunk piled up in memory: the copy
// grew with the evidence, not with the worker count.
func TestSimhashWriterBoundsInFlight(t *testing.T) {
	db := openTestDB(t)
	release := make(chan struct{})
	prev := chunkSimHash
	chunkSimHash = func(data []byte) uint64 {
		<-release
		return prev(data)
	}
	t.Cleanup(func() { chunkSimHash = prev })

	const limit, chunks = 2, 20
	w := newSimhashAsyncWriter(db, limit)
	var accepted atomic.Int32
	go func() {
		for i := range chunks {
			w.enqueue(bytes.Repeat([]byte{byte(i)}, 4096), bytes.Repeat([]byte{byte(i), 1}, 16))
			accepted.Add(1)
		}
	}()

	// With the workers stalled, enqueue must stop accepting chunks once
	// every slot is taken.
	time.Sleep(200 * time.Millisecond)
	if n := accepted.Load(); n > limit {
		t.Fatalf("%d chunks accepted while %d workers were stalled; enqueue does not apply backpressure", n, limit)
	}

	close(release)
	deadline := time.After(30 * time.Second)
	for accepted.Load() < chunks {
		select {
		case <-deadline:
			t.Fatal("enqueue did not resume after the workers finished")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := w.wait(); err != nil {
		t.Fatal(err)
	}
}
