package dbmeta

import (
	"bytes"
	"crypto/sha3"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/zeebo/blake3"
)

// TestUniformChunkSize: only a database registered with a manifest it was
// created with has one chunk size for all its evidence.
func TestUniformChunkSize(t *testing.T) {
	t.Parallel()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(t.TempDir(), "kvdb")).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer UnregisterLayout(db)

	if UniformChunkSize(db) {
		t.Fatal("an unregistered database reports a uniform chunk size")
	}
	m := Manifest{Layout: Layout{ChunkKiB: 1024}}
	RegisterManifest(db, m)
	if !UniformChunkSize(db) {
		t.Fatal("a database created with a manifest does not report a uniform chunk size")
	}
	m.Inferred = &Inference{At: time.Now()}
	RegisterManifest(db, m)
	if UniformChunkSize(db) {
		t.Fatal("a legacy database whose layout was inferred reports a uniform chunk size")
	}
	if l, ok := LayoutFor(db); !ok || l.ChunkKiB != 1024 {
		t.Fatalf("LayoutFor = %+v, %v", l, ok)
	}
}

func TestFileHasherNamed(t *testing.T) {
	t.Parallel()
	data := []byte("evidence bytes")
	b3 := blake3.Sum256(data)
	s3 := sha3.Sum256(data)
	for name, want := range map[string][]byte{FileHashBLAKE3: b3[:], FileHashSHA3: s3[:]} {
		newHash, err := FileHasherNamed(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		h := newHash()
		if _, err := h.Write(data); err != nil {
			t.Fatal(err)
		}
		if got := h.Sum(nil); !bytes.Equal(got, want) {
			t.Errorf("%s: hash %x, want %x", name, got, want)
		}
	}
	for _, name := range []string{"", ChunkHashSHA3, HashSHA3, "md5"} {
		if _, err := FileHasherNamed(name); !errors.Is(err, ErrUnknownFileHash) {
			t.Errorf("FileHasherNamed(%q) = %v, want ErrUnknownFileHash", name, err)
		}
	}
}
