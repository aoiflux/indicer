package ingest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"indicer/internal/progress"
	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/index"
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
	if v := ps["identify"]; v.Cost != identifyCost {
		t.Errorf("identify costs %v, want %v", v.Cost, identifyCost)
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

// planned lists the phases r was told of before they started, in order.
func planned(r *progress.Reporter) string {
	var names []string
	for _, v := range r.Snapshot(0).Phases {
		if v.Planned {
			names = append(names, v.Name)
		}
	}
	return fmt.Sprint(names)
}

// TestIngestPlansItsSteps: an ingest lists its steps before the first
// starts, identify only under the sync strategy, and the index and
// full-text say which file they are on.
func TestIngestPlansItsSteps(t *testing.T) {
	for strategy, want := range map[string]string{
		cnst.SyncHashStrategy:  "[identify store index full-text]",
		cnst.AsyncHashStrategy: "[store index full-text]",
	} {
		t.Run(strategy, func(t *testing.T) {
			db := openDB(t)
			prev := cnst.StoreHashStrategy
			cnst.StoreHashStrategy = strategy
			t.Cleanup(func() { cnst.StoreHashStrategy = prev })
			r := progress.New(t.Context(), nil)
			if _, err := Run(progress.With(t.Context(), r), db, Request{Path: fixture(t, "fat/fat16"), Decode: true, FTS: true}); err != nil {
				t.Fatal(err)
			}
			if got := planned(r); got != want {
				t.Errorf("planned %s, want %s", got, want)
			}
			ps := phases(r)
			if _, ok := ps["identify"]; ok != (strategy == cnst.SyncHashStrategy) {
				t.Errorf("identify ran: %v", ok)
			}
			if ps["index"].Item == "" || ps["full-text"].Item == "" {
				t.Errorf("no current file: index %q, full-text %q", ps["index"].Item, ps["full-text"].Item)
			}
		})
	}
}

// TestIngestAliasDropsItsPlan: content already stored ends after identify,
// leaving none of the planned steps behind as "not run".
func TestIngestAliasDropsItsPlan(t *testing.T) {
	db := openDB(t)
	prev := cnst.StoreHashStrategy
	cnst.StoreHashStrategy = cnst.SyncHashStrategy
	t.Cleanup(func() { cnst.StoreHashStrategy = prev })
	path := testutil.WriteRandomFile(t, t.TempDir(), "a.bin", 5, 256<<10)
	if _, err := Run(t.Context(), db, Request{Path: path, Decode: true, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	r := progress.New(t.Context(), nil)
	res, err := Run(progress.With(t.Context(), r), db, Request{Path: path, Decode: true})
	if err != nil || res.Outcome != store.CommitAliased {
		t.Fatalf("second ingest: %+v, %v", res, err)
	}
	r.Finish(nil)
	if vs := r.Snapshot(0).Phases; len(vs) != 1 || vs[0].Name != "identify" || vs[0].State != progress.OK {
		t.Fatalf("phases %+v", vs)
	}
}

// TestIngestFailedIndexSkipsFeatures: the features read the index, so when
// it fails they show as not run, and why.
func TestIngestFailedIndexSkipsFeatures(t *testing.T) {
	db := openDB(t)
	prev := runIndex
	t.Cleanup(func() { runIndex = prev })
	runIndex = func(context.Context, index.Options) (index.Result, error) {
		return index.Result{}, errors.New("simulated index failure")
	}
	r := progress.New(t.Context(), nil)
	if _, err := Run(progress.With(t.Context(), r), db, Request{Path: fixture(t, "fat/fat12"), Decode: true, FTS: true, Enrich: true}); err == nil {
		t.Fatal("ingest succeeded; want an incomplete index")
	}
	ps := phases(r)
	for _, name := range []string{"full-text", "entropy", "graph"} {
		if v := ps[name]; v.State != progress.Skipped || v.Reason != "index failed" {
			t.Errorf("%s: %+v", name, v)
		}
	}
}

func TestReindexPlansItsSteps(t *testing.T) {
	db := openDB(t)
	res, err := Run(t.Context(), db, Request{Path: fixture(t, "fat/fat16"), Decode: true, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	r := progress.New(t.Context(), nil)
	if _, err := Reindex(progress.With(t.Context(), r), db, res.EvidenceID, ReindexOptions{FTS: true}); err != nil {
		t.Fatal(err)
	}
	if got := planned(r); got != "[delete old index index full-text]" {
		t.Errorf("planned %s", got)
	}
}
