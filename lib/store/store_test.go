package store

import (
	"bytes"
	"errors"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
	"github.com/klauspost/compress/zstd"
)

func TestMarkEvidenceFileFailedSetsFailedFlag(t *testing.T) {
	db := openTestDB(t)

	infile := newTestInputFile(db, "failed-retry.dd", bytes.Repeat([]byte{1}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed evidence file: %v", err)
	}

	if err := MarkEvidenceFileFailed(infile.GetID(), db); err != nil {
		t.Fatalf("MarkEvidenceFileFailed: %v", err)
	}

	updated, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if !updated.Failed {
		t.Fatal("expected failed flag to be set")
	}
	if updated.Completed {
		t.Fatal("expected completed flag to remain false")
	}
}

func TestMarkEvidenceFileFailedUnlessFlushedMarksPending(t *testing.T) {
	db := openTestDB(t)

	infile := newTestInputFile(db, "pending-fail.dd", bytes.Repeat([]byte{0x11}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")
	evidenceFile.IngestState = structs.IngestStatePending
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed evidence file: %v", err)
	}

	if err := MarkEvidenceFileFailedUnlessFlushed(infile.GetID(), db); err != nil {
		t.Fatalf("MarkEvidenceFileFailedUnlessFlushed: %v", err)
	}

	updated, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if !updated.Failed {
		t.Fatal("expected pending ingest to be marked failed")
	}
}

func TestMarkEvidenceFileFailedUnlessFlushedSkipsFlushed(t *testing.T) {
	db := openTestDB(t)

	infile := newTestInputFile(db, "flushed-preserve.dd", bytes.Repeat([]byte{0x22}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")
	evidenceFile.IngestState = structs.IngestStateFlushed
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed evidence file: %v", err)
	}

	if err := MarkEvidenceFileFailedUnlessFlushed(infile.GetID(), db); err != nil {
		t.Fatalf("MarkEvidenceFileFailedUnlessFlushed: %v", err)
	}

	updated, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if updated.Failed {
		t.Fatal("expected flushed ingest to remain recoverable (not failed)")
	}
	if updated.IngestState != structs.IngestStateFlushed {
		t.Fatalf("expected ingest state to remain flushed, got %q", updated.IngestState)
	}
}

func TestEvidenceFilePreflightClearsFailedFlagOnRetry(t *testing.T) {
	db := openTestDB(t)

	infile := newTestInputFile(db, "retry.dd", bytes.Repeat([]byte{2}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")
	evidenceFile.Failed = true
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed evidence file: %v", err)
	}

	updated, err := evidenceFilePreflight(infile)
	if err != nil {
		t.Fatalf("evidenceFilePreflight: %v", err)
	}
	if updated.Failed {
		t.Fatal("expected retry preflight to clear failed flag")
	}

	persisted, err := dbio.GetEvidenceFile(infile.GetID(), db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if persisted.Failed {
		t.Fatal("expected persisted failed flag to be cleared")
	}
}

func TestProcessRevRelSkippedWhenDisabled(t *testing.T) {
	db := openTestDB(t)

	prev := cnst.ENABLEREVREL
	cnst.ENABLEREVREL = false
	t.Cleanup(func() {
		cnst.ENABLEREVREL = prev
	})

	index := int64(0)
	fhash := []byte("file-disabled")
	chash := []byte("chunk-disabled")

	batch := db.NewWriteBatch()
	if err := processRevRel(index, fhash, chash, batch, nil); err != nil {
		batch.Cancel()
		t.Fatalf("processRevRel disabled: %v", err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatalf("batch.Flush disabled: %v", err)
	}

	revKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, index)
	_, err := dbio.GetReverseRelationNode(revKey, db)
	if !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("expected badger.ErrKeyNotFound when reverse relations disabled, got %v", err)
	}
}

func TestProcessRevRelWritesWhenEnabled(t *testing.T) {
	db := openTestDB(t)

	prev := cnst.ENABLEREVREL
	cnst.ENABLEREVREL = true
	t.Cleanup(func() {
		cnst.ENABLEREVREL = prev
	})

	index := int64(0)
	fhash := []byte("file-enabled")
	chash := []byte("chunk-enabled")

	batch := db.NewWriteBatch()
	if err := processRevRel(index, fhash, chash, batch, nil); err != nil {
		batch.Cancel()
		t.Fatalf("processRevRel enabled: %v", err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatalf("batch.Flush enabled: %v", err)
	}

	revKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, index)
	revMap, err := dbio.GetReverseRelationNode(revKey, db)
	if err != nil {
		t.Fatalf("GetReverseRelationNode enabled: %v", err)
	}
	if _, ok := revMap[string(fhash)]; !ok {
		t.Fatalf("expected reverse relation member %q when enabled", string(fhash))
	}
}

func openTestDB(t testing.TB) *badger.DB {
	t.Helper()
	prevQuick := cnst.QUICKOPT
	cnst.QUICKOPT = true
	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			t.Fatalf("zstd.NewReader: %v", err)
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
		if err != nil {
			t.Fatalf("zstd.NewWriter: %v", err)
		}
		cnst.ENCODER = encoder
	}
	t.Cleanup(func() {
		cnst.QUICKOPT = prevQuick
	})

	db, err := dbio.ConnectDB(t.TempDir(), bytes.Repeat([]byte{0}, 32))
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}
	})
	return db
}

func newTestInputFile(db *badger.DB, name string, hash []byte) structs.InputFile {
	var mapped mmap.MMap
	return structs.NewInputFile(db, nil, mapped, name, cnst.EviFileNamespace, hash, 128, 0)
}
