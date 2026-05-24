package store

import (
	"bytes"
	"testing"

	"indicer/lib/dbio"
	"indicer/lib/structs"
)

func TestInspectEvidenceRepairsDoesNotMutateWithoutFix(t *testing.T) {
	db := openTestDB(t)
	infile := newTestInputFile(db, "pending.dd", bytes.Repeat([]byte{4}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed evidence file: %v", err)
	}

	report, err := InspectEvidenceRepairs(db, false)
	if err != nil {
		t.Fatalf("InspectEvidenceRepairs: %v", err)
	}
	if report.TotalProblematic != 1 {
		t.Fatalf("expected 1 problematic evidence, got %d", report.TotalProblematic)
	}
	if report.PendingCount != 1 {
		t.Fatalf("expected 1 pending evidence, got %d", report.PendingCount)
	}
	if report.RepairedCount != 0 {
		t.Fatalf("expected 0 repaired evidence, got %d", report.RepairedCount)
	}
	if got := report.Evidence[0].Action; got != "pending" {
		t.Fatalf("expected action pending, got %q", got)
	}

	persisted, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if persisted.Failed {
		t.Fatal("expected inspect-only run to leave failed flag unset")
	}
}

func TestInspectEvidenceRepairsFixMarksPendingEvidenceFailed(t *testing.T) {
	db := openTestDB(t)
	infile := newTestInputFile(db, "repair.dd", bytes.Repeat([]byte{5}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed evidence file: %v", err)
	}

	report, err := InspectEvidenceRepairs(db, true)
	if err != nil {
		t.Fatalf("InspectEvidenceRepairs: %v", err)
	}
	if report.RepairedCount != 1 {
		t.Fatalf("expected 1 repaired evidence, got %d", report.RepairedCount)
	}
	if report.PendingCount != 0 {
		t.Fatalf("expected 0 pending evidence after repair, got %d", report.PendingCount)
	}
	if report.FailedCount != 1 {
		t.Fatalf("expected 1 failed evidence after repair, got %d", report.FailedCount)
	}
	if got := report.Evidence[0].Action; got != "marked_failed" {
		t.Fatalf("expected action marked_failed, got %q", got)
	}

	persisted, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if !persisted.Failed {
		t.Fatal("expected repair run to mark evidence as failed")
	}
}

func TestInspectEvidenceRepairsFixAutoCompletesFlushed(t *testing.T) {
	db := openTestDB(t)
	infile := seedFlushedEvidenceWithChunk(t, db, "flushed-repair.dd", 0xAA)

	report, err := InspectEvidenceRepairs(db, true)
	if err != nil {
		t.Fatalf("InspectEvidenceRepairs: %v", err)
	}
	if report.AutoCompletedCount != 1 {
		t.Fatalf("expected 1 auto-completed, got %d", report.AutoCompletedCount)
	}
	if report.RepairedCount != 0 {
		t.Fatalf("expected 0 repaired (marked_failed), got %d", report.RepairedCount)
	}
	if got := report.Evidence[0].Action; got != "auto_completed" {
		t.Fatalf("expected action auto_completed, got %q", got)
	}

	persisted, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if persisted.Failed {
		t.Fatal("expected flushed evidence NOT to be marked failed")
	}
	if !persisted.Completed {
		t.Fatal("expected flushed evidence to be marked completed")
	}
	if persisted.IngestState != structs.IngestStateCompleted {
		t.Fatalf("expected IngestStateCompleted, got %q", persisted.IngestState)
	}
}

func TestInspectEvidenceRepairsFixMarksInvalidFlushedFailed(t *testing.T) {
	db := openTestDB(t)
	infile := newTestInputFile(db, "flushed-invalid-repair.dd", bytes.Repeat([]byte{0xAB}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")
	evidenceFile.IngestState = structs.IngestStateFlushed
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed invalid flushed evidence: %v", err)
	}

	report, err := InspectEvidenceRepairs(db, true)
	if err != nil {
		t.Fatalf("InspectEvidenceRepairs: %v", err)
	}
	if report.AutoCompletedCount != 0 {
		t.Fatalf("expected 0 auto-completed records, got %d", report.AutoCompletedCount)
	}
	if report.RepairedCount != 1 {
		t.Fatalf("expected 1 repaired record, got %d", report.RepairedCount)
	}
	if report.FailedCount != 1 {
		t.Fatalf("expected 1 failed record, got %d", report.FailedCount)
	}
	if got := report.Evidence[0].Action; got != "flushed_invalid_marked_failed" {
		t.Fatalf("expected action flushed_invalid_marked_failed, got %q", got)
	}

	persisted, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if !persisted.Failed {
		t.Fatal("expected invalid flushed evidence to be marked failed")
	}
	if persisted.Completed {
		t.Fatal("expected invalid flushed evidence to remain incomplete")
	}
}
