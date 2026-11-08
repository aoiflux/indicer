package testutil

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFileAndRange(t *testing.T) {
	t.Parallel()
	data := RandomBytes(7, 4096)
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	all := sha256.Sum256(data)
	got, err := HashFile(f, sha256.New())
	if err != nil || !bytes.Equal(got, all[:]) {
		t.Fatalf("HashFile = %x, %v; want %x", got, err, all)
	}
	if pos, _ := f.Seek(0, 1); pos != 0 {
		t.Fatalf("HashFile left the file at %d", pos)
	}

	part := sha256.Sum256(data[64 : 64+256])
	got, err = HashRange(f, sha256.New(), 64, 256)
	if err != nil || !bytes.Equal(got, part[:]) {
		t.Fatalf("HashRange = %x, %v; want %x", got, err, part)
	}
	if _, err := HashRange(f, sha256.New(), 4000, 200); err == nil {
		t.Fatal("HashRange past the end did not fail")
	}
}
