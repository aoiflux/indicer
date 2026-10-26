package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func setIndexState(t *testing.T, db *badger.DB, eid []byte, state structs.IndexState) {
	t.Helper()
	err := dbio.UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Index = &structs.IndexInfo{State: state, Started: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRecoveryCommitsFlushedAttempts: a FLUSHED attempt is committed the way
// an ingest commits it (W6), so the second of two attempts holding the same
// content becomes an alias and its records go; its index pass, which the
// crash cut short, is marked interrupted. Recovery used to mark a FLUSHED
// evidence COMPLETED without entering its identity, so the same content
// could be stored twice.
func TestRecoveryCommitsFlushedAttempts(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	content := bytes.Repeat([]byte("recover me "), 3000)
	first := flushedEvidence(t, db.DB, "/case/a.img", content)
	second := flushedEvidence(t, db.DB, "/case/copy-of-a.img", content)
	setIndexState(t, db.DB, first, structs.IndexRunning)
	setIndexState(t, db.DB, second, structs.IndexRunning)

	report, err := RecoverIncompleteIngests(t.Context(), db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if report.CompletedFlushed != 1 || report.Aliased != 1 || report.IndexInterrupted != 1 {
		t.Fatalf("report %+v, want one completed (index interrupted) and one aliased", report)
	}
	winner, loser := first, second
	if e, _ := dbio.GetEvidenceFile(first, db.DB); !e.Completed {
		winner, loser = second, first
	}
	e, err := dbio.GetEvidenceFile(winner, db.DB)
	if err != nil || !e.Completed || e.Index == nil || e.Index.State != structs.IndexInterrupted {
		t.Fatalf("winner: %+v, %v", e.Index, err)
	}
	if w, err := StoredByIdentity(db.DB, e.Size, e.ContentRoot, e.PlainHash); err != nil || !bytes.Equal(w, winner) {
		t.Fatalf("the recovered evidence is not entered under its identity: %x, %v", w, err)
	}
	if _, err := dbio.GetEvidenceFile(loser, db.DB); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("the duplicate attempt's record is still there: %v", err)
	}
	if n := countPrefix(t, db.DB, dbio.RelationPrefix(loser)); n != 0 {
		t.Fatalf("the duplicate attempt kept %d relations", n)
	}
	if aliases, _ := dbio.Aliases(db.DB, winner); len(aliases) != 1 {
		t.Fatalf("%d aliases, want 1", len(aliases))
	}
	// Recovering again changes nothing.
	again, err := RecoverIncompleteIngests(t.Context(), db.DB)
	if err != nil || again.CompletedFlushed+again.Aliased+again.MarkedFailed+again.IndexInterrupted+again.DuplicatesCleaned != 0 {
		t.Fatalf("second recovery: %+v, %v", again, err)
	}
}

// TestRecoveryFinishesDuplicateCleanup: a duplicate whose removal a crash cut
// short is removed at the next writable open.
func TestRecoveryFinishesDuplicateCleanup(t *testing.T) {
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
	report, err := RecoverIncompleteIngests(t.Context(), db.DB)
	if err != nil || report.DuplicatesCleaned != 1 {
		t.Fatalf("report %+v, %v", report, err)
	}
	if _, err := dbio.GetEvidenceFile(dup, db.DB); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("duplicate record: %v", err)
	}
}

// TestRecoveryInterruptsIndexPass: an attempt that died before its chunks
// were stored is failed with a reason, and an index pass marked running on
// it or on a completed evidence is marked interrupted.
func TestRecoveryInterruptsIndexPass(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	content := bytes.Repeat([]byte("pending "), 2000)
	pending := flushedEvidence(t, db.DB, "/case/p.img", content)
	err := dbio.UpdateEvidence(db.DB, pending, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.IngestState = structs.IngestStatePending
		e.Index = &structs.IndexInfo{State: structs.IndexRunning}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	done := flushedEvidence(t, db.DB, "/case/d.img", bytes.Repeat([]byte("done "), 2000))
	if _, err := CommitEvidence(db.DB, done); err != nil {
		t.Fatal(err)
	}
	setIndexState(t, db.DB, done, structs.IndexRunning)

	report, err := RecoverIncompleteIngests(t.Context(), db.DB)
	if err != nil || report.MarkedFailed != 1 || report.IndexInterrupted != 1 {
		t.Fatalf("report %+v, %v", report, err)
	}
	p, _ := dbio.GetEvidenceFile(pending, db.DB)
	if !p.Failed || p.LastError == "" || p.Index.State != structs.IndexInterrupted {
		t.Fatalf("pending attempt: failed %v error %q index %+v", p.Failed, p.LastError, p.Index)
	}
	d, _ := dbio.GetEvidenceFile(done, db.DB)
	if !d.Completed || d.Index.State != structs.IndexInterrupted || d.Index.Finished == 0 {
		t.Fatalf("completed evidence: %+v", d.Index)
	}

	// repair reports and fixes the same way.
	rep, err := InspectEvidenceRepairs(t.Context(), db.DB, false)
	if err != nil || rep.TotalProblematic != 1 || rep.FailedCount != 1 {
		t.Fatalf("inspection after recovery: %+v, %v", rep, err)
	}
}

// TestRecoveryStopsWhenCancelled: recovery stops at a cancelled context (an
// examiner's Ctrl-C while a store opens the database) and leaves the attempt
// FLUSHED for the next writable open. It used to validate the attempt
// whatever the context said, so Ctrl-C waited for every chunk to be checked.
func TestRecoveryStopsWhenCancelled(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	eid := flushedEvidence(t, db.DB, "/case/a.img", bytes.Repeat([]byte("cancel me "), 3000))
	ctx, cancel := context.WithCancelCause(t.Context())
	stop := errors.New("examiner pressed Ctrl-C")
	cancel(stop)
	if _, err := RecoverIncompleteIngests(ctx, db.DB); !errors.Is(err, stop) {
		t.Fatalf("recovery: %v, want the cancel cause", err)
	}
	e, err := dbio.GetEvidenceFile(eid, db.DB)
	if err != nil || e.IngestState != structs.IngestStateFlushed || e.Completed || e.Failed {
		t.Fatalf("attempt after a cancelled recovery: %+v, %v", e.IngestState, err)
	}
	if _, err := RecoverEvidence(ctx, db.DB, eid, e); !errors.Is(err, stop) {
		t.Fatalf("RecoverEvidence: %v, want the cancel cause", err)
	}
}
