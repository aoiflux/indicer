package fio

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/fsx"
)

// TestWriteChonkCreatesAndReplaces covers both write paths of WriteChonk: a
// new blob is created in place, and an existing file under the blob's name
// (a blob torn by a crash, or truncated) is replaced whole.
func TestWriteChonkCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blob")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ckey := append(append([]byte{}, cnst.ChonkNamespace...), bytes.Repeat([]byte{7}, 32)...)
	data := bytes.Repeat([]byte("chunk"), 1000)

	path, err := WriteChonk(dir, data, ckey, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertBlob := func(stage string) {
		t.Helper()
		got, err := os.ReadFile(string(path))
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("%s: blob holds %d bytes, want the %d written", stage, len(got), len(data))
		}
	}
	assertBlob("new blob")

	if err := os.Truncate(string(path), int64(len(data)/2)); err != nil {
		t.Fatal(err)
	}
	again, err := WriteChonk(dir, data, ckey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(path) {
		t.Fatalf("rewrite went to %s, want %s", again, path)
	}
	assertBlob("rewritten blob")

	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if fsx.IsTempName(e.Name()) {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
