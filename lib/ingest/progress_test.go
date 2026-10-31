package ingest

import (
	"testing"

	"indicer/internal/progress"
	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/store"
	"indicer/lib/structs"
)

func phases(r *progress.Reporter) map[string]progress.View {
	out := map[string]progress.View{}
	for _, v := range r.Snapshot(0).Phases {
		out[v.Name] = v
	}
	return out
}

// TestIngestReportsItsPhases: a store of a disk image plans store, index
// and full-text, and each ends done; the index counts the files it
// indexed and full-text counts the documents it wrote.
func TestIngestReportsItsPhases(t *testing.T) {
	db := openDB(t)
	r := progress.New(t.Context(), nil)
	ctx := progress.With(t.Context(), r)
	res, err := Run(ctx, db, Request{Path: fixture(t, "fat/fat16"), Decode: true, FTS: true})
	if err != nil || res.Outcome != store.CommitStored || res.Index == nil {
		t.Fatalf("ingest: %+v, %v", res, err)
	}
	ps := phases(r)
	for _, name := range []string{"store", "index", "full-text"} {
		if v, ok := ps[name]; !ok || v.State != progress.OK {
			t.Errorf("phase %s: %+v (found %v)", name, v, ok)
		}
	}
	if got, want := ps["index"].Done, int64(res.Index.Coverage.FilesIndexed); got != want || got == 0 {
		t.Errorf("index counted %d files, the index holds %d", got, want)
	}
	if v := ps["full-text"]; v.Total != v.Done || v.Done == 0 {
		t.Errorf("full-text %d of %d", v.Done, v.Total)
	}
	if v := ps["store"]; v.Done != res.Size {
		t.Errorf("store read %d of %d bytes", v.Done, res.Size)
	}
}

func TestIngestSkippedIndexAndIdentify(t *testing.T) {
	db := openDB(t)
	prev := cnst.StoreHashStrategy
	cnst.StoreHashStrategy = cnst.SyncHashStrategy
	t.Cleanup(func() { cnst.StoreHashStrategy = prev })

	r := progress.New(t.Context(), nil)
	ctx := progress.With(t.Context(), r)
	path := testutil.WriteRandomFile(t, t.TempDir(), "blob.bin", 3, 256<<10)
	res, err := Run(ctx, db, Request{Path: path, Decode: true, NoIndex: true})
	if err != nil || res.Outcome != store.CommitStored {
		t.Fatalf("ingest: %+v, %v", res, err)
	}
	ps := phases(r)
	if v := ps["identify"]; v.State != progress.OK || v.Done != res.Size {
		t.Errorf("identify %+v, want the content hashed once", v)
	}
	if v := ps["index"]; v.State != progress.Skipped || v.Reason == "" {
		t.Errorf("index %+v, want skipped with a reason", v)
	}
	if res.Index == nil || res.Index.State != structs.IndexSkipped {
		t.Errorf("index info %+v", res.Index)
	}
}

func TestIngestQuietReportsNothing(t *testing.T) {
	db := openDB(t)
	r := progress.New(t.Context(), nil)
	path := testutil.WriteRandomFile(t, t.TempDir(), "q.bin", 4, 64<<10)
	if _, err := Run(progress.With(t.Context(), r), db, Request{Path: path, Decode: true, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if snap := r.Snapshot(0); len(snap.Phases) != 0 {
		t.Fatalf("a quiet ingest reported %+v", snap.Phases)
	}
}
