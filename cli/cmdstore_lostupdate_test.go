package cli

import (
	"encoding/base64"
	"path/filepath"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// fixtureNamed returns the parser fixture called name, decompressed.
func fixtureNamed(t *testing.T, name string) (fsfixture.Manifest, string) {
	t.Helper()
	t.Cleanup(fsfixture.Cleanup)
	for _, m := range fsfixture.All(t) {
		if m.Name == name {
			return m, fsfixture.Open(t, m)
		}
	}
	t.Fatalf("no fixture %s", name)
	return fsfixture.Manifest{}, ""
}

// onlyEvidence returns the one evidence record of the database at dbPath.
func onlyEvidence(t *testing.T, dbPath string) ([]byte, structs.EvidenceFile) {
	t.Helper()
	h, err := openTestDB(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	var ids [][]byte
	var recs []structs.EvidenceFile
	err = h.DB.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			id := dbio.EvidenceRawID(it.Item().KeyCopy(nil))
			rec, err := dbio.GetEvidenceTxn(txn, id)
			if err != nil {
				return err
			}
			ids, recs = append(ids, id), append(recs, rec)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("%d evidence records, want 1", len(recs))
	}
	return ids[0], recs[0]
}

// TestIngestKeepsPartitionLinks is the successor of the ING-8 regression
// (the COMPLETED transition wrote back a stale copy over the partition map
// the indexer persisted meanwhile). The index pass now links each partition
// from the evidence while the chunk pass stores it and the evidence is
// flushed and committed; every one of those writes is a read-modify-write of
// the current record, so no partition is lost, whichever hash strategy runs.
func TestIngestKeepsPartitionLinks(t *testing.T) {
	m, image := fixtureNamed(t, "part/gpt_multi")
	for _, strategy := range []string{cnst.SyncHashStrategy, cnst.AsyncHashStrategy, cnst.HochoHashStrategy} {
		t.Run(strategy, func(t *testing.T) {
			tun := cnst.DefaultTuning()
			tun.HashStrategy = strategy
			dbPath := filepath.Join(t.TempDir(), "db")
			opts := optsFor(dbPath, matrixChunkKB, nil)
			opts.Tuning = &tun
			if err := StoreData(t.Context(), opts, image, false, false, false, false); err != nil {
				t.Fatalf("store: %v", err)
			}
			eid, rec := onlyEvidence(t, dbPath)
			if !rec.Completed || rec.IngestState != structs.IngestStateCompleted {
				t.Fatalf("not completed: %v %q", rec.Completed, rec.IngestState)
			}
			if rec.Index == nil || rec.Index.State != structs.IndexComplete || rec.Index.Coverage.Partitions == 0 {
				t.Fatalf("index info %+v", rec.Index)
			}
			if len(rec.InternalObjects) != rec.Index.Coverage.Partitions {
				t.Fatalf("evidence links %d partitions, the index pass wrote %d", len(rec.InternalObjects), rec.Index.Coverage.Partitions)
			}
			want := rec.PlainHash
			if strategy == cnst.HochoHashStrategy {
				want = rec.ContentRoot
			}
			if rec.PlainHash == "" || rec.ContentRoot == "" || rec.FileHash != want {
				t.Fatalf("hashes: plain %q root %q file %q", rec.PlainHash, rec.ContentRoot, rec.FileHash)
			}
			if rec.Size != m.Size || rec.ContentModel != structs.ContentModelV2 {
				t.Fatalf("record size %d (want %d), model %q", rec.Size, m.Size, rec.ContentModel)
			}
			// Every linked partition has its record, with its hash set.
			h, err := openTestDB(dbPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			for pidB64 := range rec.InternalObjects {
				pid, _ := base64.StdEncoding.DecodeString(pidB64)
				prec, err := dbio.GetPartitionFile(pid, h.DB)
				if err != nil {
					t.Fatalf("partition %s: %v", pidB64, err)
				}
				if ev, err := dbio.RecordEvidenceID(prec.IndexedFile); err != nil || string(ev) != string(eid) {
					t.Errorf("partition %s names evidence %x, %v", pidB64, ev, err)
				}
				if prec.FileHash == "" {
					t.Errorf("partition %s at %d has no hash", pidB64, prec.Start)
				}
			}
		})
	}
}
