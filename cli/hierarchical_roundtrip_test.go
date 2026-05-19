package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
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
