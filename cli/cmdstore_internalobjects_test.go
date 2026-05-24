package cli

import (
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
)

func TestPersistEvidenceInternalObjects(t *testing.T) {
	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			t.Fatalf("init zstd decoder: %v", err)
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedDefault)))
		if err != nil {
			t.Fatalf("init zstd encoder: %v", err)
		}
		cnst.ENCODER = encoder
	}

	db, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	defer db.Close()

	fileID := util.AppendToBytesSlice(cnst.EviFileNamespace, []byte("evidence-hash-001"))
	evidence := structs.NewEvidenceFile("sample.dd", 0, 4096, nil, "exfat", "")
	evidence.Completed = true
	evidence.IngestState = structs.IngestStateCompleted
	evidence.Name = "sample.dd"

	if err := dbio.SetFile(fileID, evidence, db); err != nil {
		t.Fatalf("dbio.SetFile seed evidence: %v", err)
	}

	internalObjects := map[string]structs.InternalOffset{
		"partition-a": {Start: 0, End: 1023},
		"partition-b": {Start: 1024, End: 4095},
	}

	if err := persistEvidenceInternalObjects(fileID, internalObjects, db); err != nil {
		t.Fatalf("persistEvidenceInternalObjects: %v", err)
	}

	persisted, err := dbio.GetEvidenceFile(fileID, db)
	if err != nil {
		t.Fatalf("dbio.GetEvidenceFile: %v", err)
	}

	if len(persisted.InternalObjects) != len(internalObjects) {
		t.Fatalf("internal object count = %d, want %d", len(persisted.InternalObjects), len(internalObjects))
	}
	for key, want := range internalObjects {
		got, ok := persisted.InternalObjects[key]
		if !ok {
			t.Fatalf("missing internal object %q", key)
		}
		if got != want {
			t.Fatalf("internal object %q = %+v, want %+v", key, got, want)
		}
	}
	if !persisted.Completed {
		t.Fatal("persisted evidence unexpectedly lost completed flag")
	}
	if persisted.IngestState != structs.IngestStateCompleted {
		t.Fatalf("ingest state = %q, want %q", persisted.IngestState, structs.IngestStateCompleted)
	}
	if persisted.Name != "sample.dd" {
		t.Fatal("persisted evidence unexpectedly lost name")
	}
}

func TestPersistEvidenceInternalObjectsPreservesFlushedTransition(t *testing.T) {
	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			t.Fatalf("init zstd decoder: %v", err)
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedDefault)))
		if err != nil {
			t.Fatalf("init zstd encoder: %v", err)
		}
		cnst.ENCODER = encoder
	}

	db, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	defer db.Close()

	fileID := util.AppendToBytesSlice(cnst.EviFileNamespace, []byte("evidence-hash-race"))
	evidence := structs.NewEvidenceFile("sample.dd", 0, 8192, nil, "exfat", "")
	evidence.IngestState = structs.IngestStatePending
	evidence.Completed = false
	evidence.Failed = false

	if err := dbio.SetFile(fileID, evidence, db); err != nil {
		t.Fatalf("dbio.SetFile seed evidence: %v", err)
	}

	// Simulate store pipeline transitioning to FLUSHED before indexing metadata persists.
	evidence.IngestState = structs.IngestStateFlushed
	if err := dbio.SetFile(fileID, evidence, db); err != nil {
		t.Fatalf("dbio.SetFile transition flushed: %v", err)
	}

	internalObjects := map[string]structs.InternalOffset{
		"partition-0": {Start: 0, End: 4095},
		"partition-1": {Start: 4096, End: 8191},
	}

	if err := persistEvidenceInternalObjects(fileID, internalObjects, db); err != nil {
		t.Fatalf("persistEvidenceInternalObjects: %v", err)
	}

	persisted, err := dbio.GetEvidenceFile(fileID, db)
	if err != nil {
		t.Fatalf("dbio.GetEvidenceFile: %v", err)
	}

	if persisted.IngestState != structs.IngestStateFlushed {
		t.Fatalf("ingest state = %q, want %q", persisted.IngestState, structs.IngestStateFlushed)
	}
	if persisted.Completed {
		t.Fatal("completed flag unexpectedly changed")
	}
	if persisted.Failed {
		t.Fatal("failed flag unexpectedly changed")
	}
	if len(persisted.InternalObjects) != len(internalObjects) {
		t.Fatalf("internal object count = %d, want %d", len(persisted.InternalObjects), len(internalObjects))
	}
}
