package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/internal/workpool"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/ingest"
)

// withJobsBudget gives evidence run at once budget bytes between them.
func withJobsBudget(t *testing.T, budget int64) {
	t.Helper()
	prev := jobsBudget
	t.Cleanup(func() { jobsBudget = prev })
	jobsBudget = func() int64 { return budget }
}

// recordStores replaces storeOneFile with one that stores nothing and
// records the worker cap each evidence was given, by file name.
func recordStores(t *testing.T) map[string]int {
	t.Helper()
	var mu sync.Mutex
	got := map[string]int{}
	prev := storeOneFile
	t.Cleanup(func() { storeOneFile = prev })
	storeOneFile = func(_ context.Context, _ io.Writer, _ *ingest.Session, path string, sopts StoreOptions) (ingest.Bytes, error) {
		mu.Lock()
		defer mu.Unlock()
		got[filepath.Base(path)] = sopts.maxWorkers
		return ingest.Bytes{}, nil
	}
	return got
}

// TestFitJobs: the worst case is the items that hold the most, heavy ones
// aside, running together; jobs come down until it fits, never below one.
func TestFitJobs(t *testing.T) {
	t.Parallel()
	items := []workpool.Item{{}, {Heavy: true}, {}, {}}
	lanes := []int64{10, 1000, 30, 20}
	lane := func(i, _ int) int64 { return lanes[i] }
	for _, c := range []struct {
		jobs      int
		budget    int64
		fit       int
		need      int64
		situation string
	}{
		{4, 60, 4, 60, "three light items fit, the heavy one runs alone"},
		{4, 59, 2, 60, "the two largest fit"},
		{4, 49, 1, 60, "only one at a time fits"},
		{3, 29, 1, 60, "one at a time, even over the budget"},
		{2, 1 << 40, 2, 50, "the two largest are the worst case"},
	} {
		fit, need := fitJobs(c.jobs, items, lane, c.budget)
		if fit != c.fit || need != c.need {
			t.Errorf("%s: fitJobs(%d, budget %d) = %d, %d; want %d, %d", c.situation, c.jobs, c.budget, fit, need, c.fit, c.need)
		}
	}
}

// twoChunkFolder writes n distinct files of two chunks each: each holds the
// same memory however many run beside it.
func twoChunkFolder(t *testing.T, db *testdb.DB, n int) (string, int64) {
	t.Helper()
	size := 2 * dbmeta.ChunkSize(db.DB)
	in := filepath.Join(t.TempDir(), "in")
	for i := range n {
		testutil.WriteRandomFile(t, in, fmt.Sprintf("f%d.bin", i), uint64(400+i), int(size))
	}
	return in, size
}

// TestStoreFolderJobsFitMemory: --store-jobs the user gave that do not fit
// in memory are refused before anything is stored; jobs chosen for this
// machine come down to what fits, and each evidence gets their share.
func TestStoreFolderJobsFitMemory(t *testing.T) {
	d := testdb.Open(t, testdb.Default)
	in, size := twoChunkFolder(t, d, 4)
	// Two fit; three do not. Each holds its pass and one for an object.
	withJobsBudget(t, 2*ingest.LaneMemory(d.DB, size, 2, 2))
	got := recordStores(t)
	sess := ingest.NewSession(d.DB)
	defer sess.Close()

	err := storeFolder(t.Context(), io.Discard, sess, in, StoreOptions{Decode: true, Quiet: true, Jobs: 4})
	var usage *lifecycle.UsageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "--store-jobs 4") {
		t.Fatalf("--store-jobs 4 over the budget: %v, want a usage error naming it", err)
	}
	if len(got) != 0 {
		t.Fatalf("refused, but stored %v", got)
	}

	if err := storeFolder(t.Context(), io.Discard, sess, in, StoreOptions{Decode: true, Quiet: true, Jobs: 4, AutoJobs: true}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("stored %v, want all four", got)
	}
	for name, workers := range got {
		if workers != ingest.InnerWorkers(engine.For(d.DB), 2) {
			t.Errorf("%s ran with %d workers, want %d: the share of two at once", name, workers, ingest.InnerWorkers(engine.For(d.DB), 2))
		}
	}
}

// TestHeavyEvidenceRunsWithEveryWorker: beside others, an evidence's chunk
// passes get a share of the workers; a heavy one runs alone, with them all.
func TestHeavyEvidenceRunsWithEveryWorker(t *testing.T) {
	withJobsBudget(t, 1<<50)
	d := testdb.Open(t, testdb.Default)
	if ingest.InnerWorkers(engine.For(d.DB), 4) == 0 {
		t.Fatal("ingest.InnerWorkers(4) = 0: --store-workers is set")
	}
	in, _ := twoChunkFolder(t, d, 3)
	f, err := os.Create(filepath.Join(in, "heavy.bin"))
	if err != nil {
		t.Fatal(err)
	}
	// Sparse where the file system allows: only its size matters.
	if err := errors.Join(f.Truncate(ingest.HeavySize), f.Close()); err != nil {
		t.Fatal(err)
	}
	got := recordStores(t)
	sess := ingest.NewSession(d.DB)
	defer sess.Close()
	if err := storeFolder(t.Context(), io.Discard, sess, in, StoreOptions{Decode: true, Quiet: true, Jobs: 4}); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"f0.bin": ingest.InnerWorkers(engine.For(d.DB), 4), "f1.bin": ingest.InnerWorkers(engine.For(d.DB), 4), "f2.bin": ingest.InnerWorkers(engine.For(d.DB), 4), "heavy.bin": 0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("worker caps %v, want %v", got, want)
	}
}
