package store

import (
	"bytes"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

func seedFlushedEvidenceWithChunk(t testing.TB, db *badger.DB, name string, hashSeed byte) structs.InputFile {
	t.Helper()

	infile := newTestInputFile(db, name, bytes.Repeat([]byte{hashSeed}, 32))
	evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd")
	evidenceFile.IngestState = structs.IngestStateFlushed
	if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
		t.Fatalf("seed flushed evidence: %v", err)
	}

	restoreIndex := util.GetDBStartOffset(evidenceFile.Start)
	chunkHash := bytes.Repeat([]byte{hashSeed + 1}, 32)
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, infile.GetHash(), cnst.DataSeperator, restoreIndex)
	if err := dbio.SetNode(relKey, chunkHash, db); err != nil {
		t.Fatalf("seed relation node: %v", err)
	}

	chunkKey := util.AppendToBytesSlice(cnst.ChonkNamespace, chunkHash)
	payload := []byte("flushed-chunk-payload")
	if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
		t.Fatalf("ensure blob path: %v", err)
	}
	chunkPath, err := fio.WriteChonk(db.Opts().Dir, payload, chunkKey, db.Opts().EncryptionKey)
	if err != nil {
		t.Fatalf("seed chunk file: %v", err)
	}

	metadata := structs.ChonkMetadata{
		Path:         string(chunkPath),
		Offset:       0,
		OriginalSize: int64(len(payload)),
		StoredSize:   int64(len(payload)),
		EncodedSize:  int64(len(payload)),
		Container:    false,
	}
	encodedMetadata, err := msgpack.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal chunk metadata: %v", err)
	}
	if err := dbio.SetNode(chunkKey, encodedMetadata, db); err != nil {
		t.Fatalf("seed chunk metadata node: %v", err)
	}

	return infile
}
