package cnst

import (
	"runtime"
	"testing"
)

// startGlobals is the tuning of the process before any test changes it.
var startGlobals = Globals()

// TestDefaultTuningIsTheStart: a process given no flags runs with
// DefaultTuning.
func TestDefaultTuningIsTheStart(t *testing.T) {
	if got := DefaultTuning(); got != startGlobals {
		t.Fatalf("DefaultTuning() = %+v, the globals start as %+v", got, startGlobals)
	}
}

// TestGlobalsCopiesAsIs: the bridge copies every global as it is, without
// normalizing it: ingest refuses a hash strategy it does not know, and a
// test relies on that.
func TestGlobalsCopiesAsIs(t *testing.T) {
	saved := Globals()
	t.Cleanup(func() { setGlobals(saved) })
	want := Tuning{
		LowResource: true, CompressLevel: "fast", HashStrategy: "no-such-strategy",
		Pipeline: StorePipelineStaged, IOEngine: "stdlib", IOUringDepth: 7,
		StoreWorkers: 3, StoreQueue: 5, RestoreWorkers: 11, RestoreQueue: 13, RestoreBufferMB: 17,
	}
	setGlobals(want)
	if got := Globals(); got != want {
		t.Fatalf("Globals() = %+v, want %+v", got, want)
	}
}

func setGlobals(t Tuning) {
	MEMOPT, CompressLevel, StoreHashStrategy, StorePipelineMode = t.LowResource, t.CompressLevel, t.HashStrategy, t.Pipeline
	StoreIOEngine, StoreIOUringQueueDepth = t.IOEngine, t.IOUringDepth
	StoreWorkerCount, StoreTaskQueueDepth = t.StoreWorkers, t.StoreQueue
	RestoreWorkerCount, RestoreTaskQueueDepth, RestoreWriteBufferMB = t.RestoreWorkers, t.RestoreQueue, t.RestoreBufferMB
}

// TestPipelineOverrides: set workers and queues replace the machine's, and
// a queue is never shorter than the workers it feeds.
func TestPipelineOverrides(t *testing.T) {
	for _, container := range []bool{false, true} {
		w, q := DefaultStoreTuning(runtime.NumCPU(), false, container)
		if gw, gq := (Tuning{}).StorePipeline(container); gw != w || gq != q {
			t.Errorf("container %v: default store pipeline %d/%d, want %d/%d", container, gw, gq, w, q)
		}
		if gw, gq := (Tuning{StoreWorkers: 9, StoreQueue: 2}).StorePipeline(container); gw != 9 || gq != 9 {
			t.Errorf("container %v: store pipeline %d/%d, want 9/9", container, gw, gq)
		}
	}
	if w, q := (Tuning{RestoreWorkers: 3, RestoreQueue: 30}).RestorePipeline(); w != 3 || q != 30 {
		t.Errorf("restore pipeline %d/%d, want 3/30", w, q)
	}
}

// TestRestoreWriteBuffer: the default is 256 MiB, 32 MiB in low-resource
// mode, and a set size is capped at 256 MiB.
func TestRestoreWriteBuffer(t *testing.T) {
	for _, c := range []struct {
		t    Tuning
		want int
	}{
		{Tuning{}, 256 * 1024 * 1024},
		{Tuning{LowResource: true}, 32 * 1024 * 1024},
		{Tuning{RestoreBufferMB: 5}, 5 * 1024 * 1024},
		{Tuning{RestoreBufferMB: 300}, 256 * 1024 * 1024},
	} {
		if got := c.t.RestoreWriteBuffer(); got != c.want {
			t.Errorf("%+v: %d bytes, want %d", c.t, got, c.want)
		}
	}
}
