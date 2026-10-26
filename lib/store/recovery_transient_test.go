package store

import (
	"context"
	"errors"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// TestValidateFlushedClassifiesMissingMaterial: a relation or chunk that is
// not in the database is ErrMaterialMissing, the one outcome that justifies
// failing an ingest (ING-10).
func TestValidateFlushedClassifiesMissingMaterial(t *testing.T) {
	db := openTestDB(t)
	infile := seedFlushedEvidenceWithChunk(t, db, "missing.dd", 3)
	e, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFlushedEvidenceMaterialized(t.Context(), infile.GetID(), e, db); err != nil {
		t.Fatalf("complete material: %v", err)
	}

	rel := util.AppendToBytesSlice(cnst.RelationNamespace, infile.GetID(), cnst.DataSeperator, int64(0))
	chash, err := dbio.GetNode(rel, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Delete(util.AppendToBytesSlice(cnst.ChonkNamespace, chash))
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFlushedEvidenceMaterialized(t.Context(), infile.GetID(), e, db); !errors.Is(err, ErrMaterialMissing) {
		t.Fatalf("missing chunk: %v, want ErrMaterialMissing", err)
	}
	if err := db.Update(func(txn *badger.Txn) error { return txn.Delete(rel) }); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFlushedEvidenceMaterialized(t.Context(), infile.GetID(), e, db); !errors.Is(err, ErrMaterialMissing) {
		t.Fatalf("missing relation: %v, want ErrMaterialMissing", err)
	}
}

// TestRecoveryKeepsFlushedEvidenceOnTransientError is an ING-10 regression:
// recovery failed a FLUSHED ingest on any validation error, so a read error
// that would have passed on the next open lost a recoverable ingest.
func TestRecoveryKeepsFlushedEvidenceOnTransientError(t *testing.T) {
	db := openTestDB(t)
	infile := seedFlushedEvidenceWithChunk(t, db, "transient.dd", 4)

	prev := validateFlushed
	validateFlushed = func(context.Context, []byte, structs.EvidenceFile, *badger.DB) error { return errors.New("read error") }
	t.Cleanup(func() { validateFlushed = prev })

	if _, err := RecoverIncompleteIngests(t.Context(), db); err == nil {
		t.Fatal("recovery hid a validation read error")
	}
	got, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got.Failed || got.IngestState != structs.IngestStateFlushed {
		t.Fatalf("a read error failed the ingest: failed=%v state=%q", got.Failed, got.IngestState)
	}
}

// TestRepairInspectsEvidenceRecordsOnly is an ING-10 regression: repair
// decoded every key in the database as an evidence record, so any value that
// happened to decode as one was reported, and with --fix acted on.
func TestRepairInspectsEvidenceRecordsOnly(t *testing.T) {
	db := openTestDB(t)
	lookalike := structs.NewEvidenceFile("not-evidence", 0, 1, nil, "raw", "")
	data, err := msgpack.Marshal(lookalike)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte("X|||not-an-evidence-key"), data)
	}); err != nil {
		t.Fatal(err)
	}

	report, err := InspectEvidenceRepairs(t.Context(), db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Evidence) != 0 {
		t.Fatalf("repair reported a non-evidence key as evidence: %+v", report.Evidence)
	}
}
