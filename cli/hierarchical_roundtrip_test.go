package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/klauspost/compress/zstd"
)

func TestHierarchicalContainerStoreRestoreRoundTrip(t *testing.T) {
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

	oldContainerMode := cnst.CONTAINERMODE
	oldHierarchicalMode := cnst.HIERARCHICALINDEX
	cnst.CONTAINERMODE = true
	cnst.HIERARCHICALINDEX = true
	t.Cleanup(func() {
		cnst.CONTAINERMODE = oldContainerMode
		cnst.HIERARCHICALINDEX = oldHierarchicalMode
	})

	dbPath := filepath.Join(t.TempDir(), "db")
	inputDir := t.TempDir()
	outputDir := t.TempDir()
	inputPath := filepath.Join(inputDir, "sample.bin")
	outputPath := filepath.Join(outputDir, "restored.bin")

	chunk := []byte("0123456789abcdef")
	data := bytes.Repeat(chunk, 20000)
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		t.Fatalf("write input file: %v", err)
	}

	key := []byte("01234567890123456789012345678901")
	if err := StoreData(64, dbPath, inputPath, key, false, true, false, false); err != nil {
		t.Fatalf("StoreData: %v", err)
	}

	fhandle, err := os.Open(inputPath)
	if err != nil {
		t.Fatalf("open input for hash: %v", err)
	}
	hash, err := util.GetFileHash(fhandle, cnst.GetHashAlgo(true))
	closeErr := fhandle.Close()
	if err != nil {
		t.Fatalf("get input hash: %v", err)
	}
	if closeErr != nil {
		t.Fatalf("close input for hash: %v", closeErr)
	}

	rhash := base64.StdEncoding.EncodeToString(hash)
	db, err := dbio.ConnectDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}

	fid, err := dbio.GuessFileType(rhash, db)
	if err != nil {
		t.Fatalf("GuessFileType: %v", err)
	}
	legacyFID := util.AppendToBytesSlice(cnst.EviFileNamespace, hash)
	if bytes.Equal(fid, legacyFID) {
		t.Fatal("expected UUID-keyed evidence id, got legacy hash-keyed id")
	}
	evi, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		t.Fatalf("GetEvidenceFile: %v", err)
	}
	if evi.FileHash != rhash {
		t.Fatalf("expected committed FileHash %q, got %q", rhash, evi.FileHash)
	}
	lookupKey := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, rhash)
	mappedIDs, err := dbio.GetHashLookupUUIDsByKey(lookupKey, db)
	if err != nil {
		t.Fatalf("GetHashLookupUUIDsByKey: %v", err)
	}
	found := false
	for _, mappedID := range mappedIDs {
		if bytes.Equal(mappedID, fid) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected hash lookup to contain committed evidence id")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	if err := RestoreData(64, dbPath, rhash, outputPath, key); err != nil {
		t.Fatalf("RestoreData: %v", err)
	}

	restored, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if !bytes.Equal(data, restored) {
		t.Fatalf("restored bytes mismatch: got=%d want=%d", len(restored), len(data))
	}
}
