package store

import (
	"bytes"
	"testing"

	"indicer/lib/dbio"
	"indicer/lib/structs"
)

func TestRecoverIncompleteIngestsMarksPendingAsFailed(t *testing.T) {
	db := openTestDB(t)

	pendingInput := newTestInputFile(db, "pending-recovery.dd", bytes.Repeat([]byte{6}, 32))
	pending := structs.NewEvidenceFile(pendingInput.GetName(), pendingInput.GetStartIndex(), pendingInput.GetSize(), pendingInput.GetInternalObjects(), "dd")
	if err := dbio.SetFile(pendingInput.GetID(), pending, db); err != nil {
		t.Fatalf("seed pending evidence: %v", err)
	}

	report, err := RecoverIncompleteIngests(db)
	if err != nil {
		t.Fatalf("RecoverIncompleteIngests: %v", err)
	}
	if report.IncompleteFound != 1 {
		t.Fatalf("expected 1 incomplete record, got %d", report.IncompleteFound)
	}
	if report.MarkedFailed != 1 {
		t.Fatalf("expected 1 marked failed, got %d", report.MarkedFailed)
	}
	if report.CompletedFlushed != 0 {
		t.Fatalf("expected 0 auto-completed, got %d", report.CompletedFlushed)
	}

	persisted, err := dbio.GetEvidenceFile(pendingInput.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if !persisted.Failed {
		t.Fatal("expected pending record to be marked failed")
	}
}

func TestRecoverIncompleteIngestsAutoCompletesFlushed(t *testing.T) {
	db := openTestDB(t)

	flushedInput := seedFlushedEvidenceWithChunk(t, db, "flushed-recovery.dd", 7)

	report, err := RecoverIncompleteIngests(db)
	if err != nil {
		t.Fatalf("RecoverIncompleteIngests: %v", err)
	}
	if report.IncompleteFound != 1 {
		t.Fatalf("expected 1 incomplete record, got %d", report.IncompleteFound)
	}
	if report.MarkedFailed != 0 {
		t.Fatalf("expected 0 marked failed for flushed state, got %d", report.MarkedFailed)
	}
	if report.CompletedFlushed != 1 {
		t.Fatalf("expected 1 auto-completed, got %d", report.CompletedFlushed)
	}

	persisted, err := dbio.GetEvidenceFile(flushedInput.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if persisted.Failed {
		t.Fatal("expected flushed record NOT to be marked failed")
	}
	if !persisted.Completed {
		t.Fatal("expected flushed record to be marked completed")
	}
	if persisted.IngestState != structs.IngestStateCompleted {
		t.Fatalf("expected IngestStateCompleted, got %q", persisted.IngestState)
	}
}

func TestRecoverIncompleteIngestsMarksInvalidFlushedAsFailed(t *testing.T) {
	db := openTestDB(t)

	flushedInput := newTestInputFile(db, "flushed-invalid-recovery.dd", bytes.Repeat([]byte{0x71}, 32))
	flushed := structs.NewEvidenceFile(flushedInput.GetName(), flushedInput.GetStartIndex(), flushedInput.GetSize(), flushedInput.GetInternalObjects(), "dd")
	flushed.IngestState = structs.IngestStateFlushed
	if err := dbio.SetFile(flushedInput.GetID(), flushed, db); err != nil {
		t.Fatalf("seed invalid flushed evidence: %v", err)
	}

	report, err := RecoverIncompleteIngests(db)
	if err != nil {
		t.Fatalf("RecoverIncompleteIngests: %v", err)
	}
	if report.IncompleteFound != 1 {
		t.Fatalf("expected 1 incomplete record, got %d", report.IncompleteFound)
	}
	if report.CompletedFlushed != 0 {
		t.Fatalf("expected 0 auto-completed records, got %d", report.CompletedFlushed)
	}
	if report.MarkedFailed != 1 {
		t.Fatalf("expected invalid flushed record to be marked failed, got %d", report.MarkedFailed)
	}

	persisted, err := dbio.GetEvidenceFile(flushedInput.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if !persisted.Failed {
		t.Fatal("expected invalid flushed record to be marked failed")
	}
	if persisted.Completed {
		t.Fatal("expected invalid flushed record to remain incomplete")
	}
}

func TestRecoverIncompleteIngestsSkipsCompletedAndAlreadyFailed(t *testing.T) {
	db := openTestDB(t)

	completedInput := newTestInputFile(db, "completed-recovery.dd", bytes.Repeat([]byte{8}, 32))
	completed := structs.NewEvidenceFile(completedInput.GetName(), completedInput.GetStartIndex(), completedInput.GetSize(), completedInput.GetInternalObjects(), "dd")
	completed.Completed = true
	completed.IngestState = structs.IngestStateCompleted
	if err := dbio.SetFile(completedInput.GetID(), completed, db); err != nil {
		t.Fatalf("seed completed evidence: %v", err)
	}

	failedInput := newTestInputFile(db, "failed-recovery.dd", bytes.Repeat([]byte{9}, 32))
	failed := structs.NewEvidenceFile(failedInput.GetName(), failedInput.GetStartIndex(), failedInput.GetSize(), failedInput.GetInternalObjects(), "dd")
	failed.Failed = true
	if err := dbio.SetFile(failedInput.GetID(), failed, db); err != nil {
		t.Fatalf("seed failed evidence: %v", err)
	}

	report, err := RecoverIncompleteIngests(db)
	if err != nil {
		t.Fatalf("RecoverIncompleteIngests: %v", err)
	}
	if report.IncompleteFound != 0 {
		t.Fatalf("expected 0 incomplete records, got %d", report.IncompleteFound)
	}
	if report.MarkedFailed != 0 {
		t.Fatalf("expected 0 marked failed records, got %d", report.MarkedFailed)
	}
}
