//go:build faultinject

package store

import (
	"bytes"
	"errors"
	"testing"

	"indicer/internal/faultinject"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// TestInterruptedDuplicateCleanupIsFinished: a duplicate whose cleanup stopped
// part way (its index gone, its relations and record still there, as a crash
// leaves them) is removed by the next recovery, and the evidence it
// duplicates stays whole.
func TestInterruptedDuplicateCleanupIsFinished(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	content := bytes.Repeat([]byte("duplicate "), 2000)
	first := flushedEvidence(t, db.DB, "/case/x.img", content)
	if _, err := CommitEvidence(db.DB, first); err != nil {
		t.Fatal(err)
	}
	dup := flushedEvidence(t, db.DB, "/case/y.img", content)
	if res, err := CommitEvidence(db.DB, dup); err != nil || res.Outcome != CommitAliased {
		t.Fatalf("commit: %+v, %v", res, err)
	}

	restore, err := faultinject.Set(faultinject.IngestDuplicateCleanup + "=error")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	if err := CleanupDuplicate(t.Context(), db.DB, dup); !errors.Is(err, faultinject.ErrInjected) {
		t.Fatalf("cleanup: %v, want the injected fault", err)
	}
	if n := countPrefix(t, db.DB, dbio.RelationPrefix(dup)); n == 0 {
		t.Fatal("the cleanup removed the relations before the fault point")
	}

	restore()
	report, err := RecoverIncompleteIngests(t.Context(), db.DB)
	if err != nil || report.DuplicatesCleaned != 1 {
		t.Fatalf("recovery: %+v, %v", report, err)
	}
	if _, err := dbio.GetEvidenceFile(dup, db.DB); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("duplicate record: %v", err)
	}
	if n := countPrefix(t, db.DB, dbio.RelationPrefix(dup)); n != 0 {
		t.Fatalf("the duplicate kept %d relations", n)
	}
	e, err := dbio.GetEvidenceFile(first, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateEvidence(t.Context(), db.DB, first, e, ValidateContent); err != nil {
		t.Fatalf("the winner: %v", err)
	}
}
