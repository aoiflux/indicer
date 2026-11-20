//go:build faultinject

package store

import (
	"bytes"
	"errors"
	"testing"

	"indicer/internal/faultinject"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
)

// TestMergeCommitFaultLeavesRecoverable: a merge commit that stops inside its
// transaction writes no ledger entry and leaves the attempt FLUSHED, so the
// next recovery commits it and writes the entry from the record.
func TestMergeCommitFaultLeavesRecoverable(t *testing.T) {
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(40, 3*4096+11)
	size := int64(len(content))
	m := testOrigin(testPackage, newUUID(t), content)
	eid := flushMerged(t, db.DB, m, "/src/a.dd", content)

	restore, err := faultinject.Set(faultinject.IngestInCommit + "=error")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	if _, err := CommitEvidence(db.DB, eid); !errors.Is(err, faultinject.ErrInjected) {
		t.Fatalf("commit: %v, want the injected fault", err)
	}
	if n := countPrefix(t, db.DB, []byte(cnst.EviMergeNamespace)); n != 0 {
		t.Fatalf("%d ledger keys after the failed commit", n)
	}
	if e := evidenceRecord(t, db.DB, eid); e.IngestState != structs.IngestStateFlushed || e.Merged == nil {
		t.Fatalf("record after the failed commit: state %q, merged %+v", e.IngestState, e.Merged)
	}
	if !keyExists(t, db.DB, dbio.MergeAttemptKey(m, size)) {
		t.Fatal("the failed commit removed the merge attempt key")
	}

	restore()
	report, err := RecoverIncompleteIngests(t.Context(), db.DB)
	if err != nil || report.CompletedFlushed != 1 {
		t.Fatalf("recovery: %+v, %v", report, err)
	}
	if entry, present, err := MergedTarget(db.DB, m, size); err != nil || !present || !bytes.Equal(entry.Target, eid) {
		t.Fatalf("MergedTarget: %+v present %v, %v", entry, present, err)
	}
	if keyExists(t, db.DB, dbio.MergeAttemptKey(m, size)) {
		t.Fatal("recovery left the merge attempt key")
	}
}
