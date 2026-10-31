package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/progress"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/ingest"
	"indicer/lib/store"
)

// TestStoreFolderProgress: a folder store shows the folder's scan, one
// "evidence" phase counting the items, an item group for each with its
// store under it, and the value-log compaction.
func TestStoreFolderProgress(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "case")
	for i, name := range []string{"a.bin", "b.bin", "c.bin"} {
		testutil.WriteRandomFile(t, src, name, uint64(40+i), 2*matrixChunkKB*1024)
	}
	opts := optsFor(filepath.Join(dir, "db"), matrixChunkKB, nil)
	r := progress.New(t.Context(), nil)
	if err := StoreDataWithOptions(progress.With(t.Context(), r), opts, src, StoreOptions{NoIndex: true, Decode: true}); err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot(0)
	got := map[string]progress.View{}
	for _, v := range snap.Phases {
		got[v.Name] = v
	}
	if v := got["scan folder"]; v.State != progress.OK || v.Done != 3 {
		t.Errorf("scan folder %+v", v)
	}
	if v := got["evidence"]; v.State != progress.OK || v.Done != 3 || v.Total != 3 {
		t.Errorf("evidence %+v", v)
	}
	if v := got["compact"]; v.State != progress.OK {
		t.Errorf("compact %+v", v)
	}
	var items []string
	for _, e := range snap.Events {
		if e.Ended && len(e.Path) == 1 && e.Path[0] == "evidence" {
			items = append(items, e.Phase.Name+"="+stateName(e.Phase.State))
		}
	}
	if strings.Join(items, " ") != "a.bin=ok b.bin=ok c.bin=ok" {
		t.Errorf("item ends %v", items)
	}
}

func stateName(s progress.State) string {
	return map[progress.State]string{progress.OK: "ok", progress.Warning: "warning", progress.Failed: "failed", progress.Interrupted: "interrupted"}[s]
}

// TestInterruptHint: an interrupted store says how to finish it, matching
// what was left: an unfinished attempt resumes; stored evidence needs its
// index finished.
func TestInterruptHint(t *testing.T) {
	cases := []struct {
		res  ingest.Result
		want string
	}{
		{ingest.Result{Attempt: []byte("a")}, "run the same command again to resume"},
		{ingest.Result{Outcome: store.CommitStored, EvidenceID: []byte{1, 2, 3}}, "run `dues reindex AQID` to finish its index"},
		{ingest.Result{}, ""},
	}
	for _, c := range cases {
		r := progress.New(context.Background(), nil)
		interruptHint(progress.With(context.Background(), r), c.res)
		hints := r.Snapshot(0).Hints
		switch {
		case c.want == "" && len(hints) != 0:
			t.Errorf("%+v: hints %q, want none", c.res, hints)
		case c.want != "" && (len(hints) != 1 || !strings.Contains(hints[0], c.want)):
			t.Errorf("%+v: hints %q, want %q", c.res, hints, c.want)
		}
	}
}
