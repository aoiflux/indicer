package ingest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/index"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// indexKeys lists every key an index pass writes, with the records' values
// that must not change (indexed records decoded, so a map's order does not
// matter).
func indexKeys(t *testing.T, db *badger.DB) []string {
	t.Helper()
	var keys []string
	for _, ns := range []string{cnst.PartiFileNamespace, cnst.IdxFileNamespace, cnst.PartitionMemberNamespace,
		cnst.ObjectNamespace, cnst.ExtentPageNamespace, cnst.PartitionReportNamespace, cnst.HashLookupMemberNamespace} {
		err := db.View(func(txn *badger.Txn) error {
			opts := badger.DefaultIteratorOptions
			opts.Prefix = []byte(ns)
			opts.PrefetchValues = false
			it := txn.NewIterator(opts)
			defer it.Close()
			for it.Rewind(); it.Valid(); it.Next() {
				keys = append(keys, string(it.Item().KeyCopy(nil)))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func indexedHashes(t *testing.T, db *badger.DB, eid []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := index.ForEachIndexed(context.Background(), db, eid, func(key []byte, rec structs.IndexedFile) error {
		out[string(key)] = rec.Path + "=" + rec.FileHash
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestReindexRebuildsTheSameIndex (I7): reindexing from the stored chunks,
// or from the source file, writes exactly the keys the first ingest wrote,
// with the same records; a file that does not hold the evidence is refused.
func TestReindexRebuildsTheSameIndex(t *testing.T) {
	for _, name := range []string{"ntfs/ntfs_compressed", "part/gpt_multi", "ewf/fat12_multiseg"} {
		t.Run(name, func(t *testing.T) {
			db := openDB(t)
			path := fixture(t, name)
			res, err := Run(t.Context(), db, Request{Path: path, Decode: true, Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			eid := res.EvidenceID
			keys, hashes := indexKeys(t, db), indexedHashes(t, db, eid)
			if len(hashes) == 0 {
				t.Fatal("the first ingest indexed nothing")
			}
			for _, source := range []string{"", path} {
				r, err := Reindex(t.Context(), db, eid, ReindexOptions{Source: source})
				if err != nil {
					t.Fatalf("reindex (source %q): %v", source, err)
				}
				if r.Index == nil || r.Index.State != structs.IndexComplete {
					t.Fatalf("index after reindex: %+v", r.Index)
				}
				if got := indexKeys(t, db); !slices.Equal(got, keys) {
					t.Fatalf("reindex (source %q) wrote %d keys, the ingest %d", source, len(got), len(keys))
				}
				got := indexedHashes(t, db, eid)
				for k, v := range hashes {
					if got[k] != v {
						t.Fatalf("reindex (source %q): %q became %q", source, v, got[k])
					}
				}
			}
			other := testutil.WriteRandomFile(t, t.TempDir(), "other.bin", 3, int(res.Size))
			if _, err := Reindex(t.Context(), db, eid, ReindexOptions{Source: other}); !errors.Is(err, ErrSourceMismatch) {
				t.Fatalf("reindex from another file: %v, want ErrSourceMismatch", err)
			}
		})
	}
}

// TestReindexAfterAFailedIndex: an index pass that failed at ingest is
// rebuilt by a reindex, from the stored chunks.
func TestReindexAfterAFailedIndex(t *testing.T) {
	db := openDB(t)
	path := fixture(t, "fat/fat16")
	prev := runIndex
	t.Cleanup(func() { runIndex = prev })
	runIndex = func(ctx context.Context, o index.Options) (index.Result, error) {
		res, _ := prev(ctx, o)
		return res, errors.New("simulated index failure")
	}
	res, err := Run(t.Context(), db, Request{Path: path, Decode: true, Quiet: true})
	var incomplete *IndexIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("ingest: %v", err)
	}
	runIndex = prev
	r, err := Reindex(t.Context(), db, res.EvidenceID, ReindexOptions{})
	if err != nil || r.Index.State != structs.IndexComplete || r.Index.Coverage.FilesIndexed == 0 {
		t.Fatalf("reindex: %v, %+v", err, r.Index)
	}
	e, _ := dbio.GetEvidenceFile(res.EvidenceID, db)
	if e.Index.State != structs.IndexComplete {
		t.Fatalf("recorded index state %s", e.Index.State)
	}
}

// TestReindexRefuses: evidence that is not stored, holds a container DUES
// cannot decode, or has a quarantined index is not reindexed.
func TestReindexRefuses(t *testing.T) {
	db := openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "x.bin", 9, 9000)
	res, err := Run(t.Context(), db, Request{Path: path, Decode: true, Quiet: true, NoIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	err = dbio.UpdateEvidence(db, res.EvidenceID, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Index = &structs.IndexInfo{State: structs.IndexInvalid, Reason: "quarantined"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(t.Context(), db, res.EvidenceID, ReindexOptions{}); !errors.Is(err, ErrNotReindexable) {
		t.Fatalf("reindex of a quarantined index: %v", err)
	}
}
