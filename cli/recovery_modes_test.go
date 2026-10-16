package cli

import (
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/store"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// TestRecoveryCompletesFlushedEvidenceInEveryMode is an ING-10 regression.
// A crash between FLUSHED and COMPLETED leaves evidence whose chunks are all
// stored; recovery must complete it. Validation looked chunks up only in
// badger, where hierarchical mode keeps none (they are in the block index),
// so recovery marked every such ingest failed in that mode.
func TestRecoveryCompletesFlushedEvidenceInEveryMode(t *testing.T) {
	testdb.ForEachMode(t, func(t *testing.T, m testdb.Mode) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "db")
		opts := optsForMode(dbPath, matrixChunkKB, m)
		src := testutil.WriteRandomFile(t, dir, "evidence.bin", 11, 3*matrixChunkKB*1024+5)
		if err := StoreData(t.Context(), opts, src, false, true, false, false); err != nil {
			t.Fatalf("store: %v", err)
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
		// Undo the COMPLETED marker, as a crash just before it would.
		if err := dbio.UpdateEvidence(h.DB, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
			e.Completed = false
			e.IngestState = structs.IngestStateFlushed
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		report, err := store.RecoverIncompleteIngests(h.DB)
		if err != nil {
			t.Fatalf("recovery: %v", err)
		}
		got, err := dbio.GetEvidenceFile(eid, h.DB)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Completed || got.Failed || report.CompletedFlushed != 1 {
			t.Fatalf("recovery left complete material %+v (completed=%v failed=%v)", report, got.Completed, got.Failed)
		}
	})
}
