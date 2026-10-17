package dbmeta

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
)

// TestUniformChunkSize: only a database registered with a manifest it was
// created with has one chunk size for all its evidence.
func TestUniformChunkSize(t *testing.T) {
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
