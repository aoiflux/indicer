package ingest

import (
	"runtime"
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/store"
)

// TestAutoJobs: a machine left to choose runs at least one evidence, and
// never more than its CPUs or maxAutoJobs.
func TestAutoJobs(t *testing.T) {
	t.Parallel()
	if n := AutoJobs(); n < 1 || n > maxAutoJobs || n > max(1, runtime.NumCPU()) {
		t.Fatalf("AutoJobs() = %d on %d CPUs, want 1 to %d", n, runtime.NumCPU(), maxAutoJobs)
	}
}

// TestInnerWorkers: one at a time keeps the configured width; several share
// it out, never below minSharedWorkers; a width the user set is kept.
func TestInnerWorkers(t *testing.T) {
	t.Parallel()
	e := engine.For(openDB(t))
	width, _ := e.Tuning().StorePipeline(e.Container())
	for _, jobs := range []int{0, 1} {
		if w := InnerWorkers(e, jobs); w != 0 {
			t.Errorf("InnerWorkers(%d) = %d, want 0: the configured width", jobs, w)
		}
	}
	for jobs := 2; jobs <= MaxJobs; jobs++ {
		w := InnerWorkers(e, jobs)
		if w < minSharedWorkers || w*jobs > max(width, minSharedWorkers*jobs) {
			t.Fatalf("InnerWorkers(%d) = %d of a width of %d", jobs, w, width)
		}
	}

	tun := testdb.Tuning()
	tun.StoreWorkers = 8
	set := engine.For(openDBWith(t, testdb.Options{ChunkKiB: 64, Tuning: &tun}))
	if w := InnerWorkers(set, 4); w != 0 {
		t.Fatalf("InnerWorkers(4) with --store-workers set = %d, want 0", w)
	}
}

// TestLaneMemory: an evidence holds what its passes hold, each at the width
// it is given beside the others.
func TestLaneMemory(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	size := 8 * dbmeta.ChunkSize(db)
	if got, want := LaneMemory(db, size, 1, 1), store.PassMemory(db, size, 0); got != want {
		t.Errorf("one pass, one at a time = %d, want %d: a pass of the configured width", got, want)
	}
	for _, jobs := range []int{1, 4} {
		if one, two := LaneMemory(db, size, jobs, 1), LaneMemory(db, size, jobs, 2); two != 2*one {
			t.Errorf("jobs %d: two passes hold %d, want twice one (%d)", jobs, two, one)
		}
	}
	if shared, alone := LaneMemory(db, size, 4, 1), LaneMemory(db, size, 1, 1); shared > alone {
		t.Errorf("a pass beside three others holds %d, more than alone (%d)", shared, alone)
	}
}
