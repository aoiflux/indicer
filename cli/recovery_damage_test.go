package cli

import (
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// TestRecoveryFailsFlushedEvidenceWithDamagedMaterial is a validation v2
// regression. Recovery checked only that each chunk had metadata, so a
// FLUSHED ingest whose blobs a crash or a full disk left short, or whose
// container lost its tail, was marked COMPLETED and could never be restored.
func TestRecoveryFailsFlushedEvidenceWithDamagedMaterial(t *testing.T) {
	t.Parallel()
	testdb.ForEachMode(t, func(t *testing.T, m testdb.Mode) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "db")
		src := testutil.WriteRandomFile(t, dir, "evidence.bin", 12, 3*matrixChunkKB*1024+5)
		if err := StoreData(t.Context(), optsForMode(dbPath, matrixChunkKB, m), src, false, true, false, false); err != nil {
			t.Fatalf("store: %v", err)
		}
		blobs, err := filepath.Glob(filepath.Join(dbPath, cnst.BLOBSDIR, "*"+cnst.BLOBEXT))
		if err != nil || len(blobs) == 0 {
			t.Fatalf("no blob files: %v", err)
		}
		for _, b := range blobs {
			fi, err := os.Stat(b)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(b, fi.Size()/2); err != nil {
				t.Fatal(err)
			}
		}

		h, err := openTestDB(dbPath, []byte(m.Password))
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		eid, err := dbio.GuessFileType(plainHashB64(t, src), h.DB)
		if err != nil {
			t.Fatal(err)
		}
		if err := dbio.UpdateEvidence(h.DB, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
			e.Completed = false
			e.IngestState = structs.IngestStateFlushed
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		if _, err := store.RecoverIncompleteIngests(t.Context(), h.DB); err != nil {
			t.Fatalf("recovery: %v", err)
		}
		got, err := dbio.GetEvidenceFile(eid, h.DB)
		if err != nil {
			t.Fatal(err)
		}
		if got.Completed || !got.Failed {
			t.Fatalf("recovery over truncated blobs: completed=%v failed=%v, want failed", got.Completed, got.Failed)
		}
	})
}
