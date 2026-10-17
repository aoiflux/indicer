package parser

import (
	"math"
	"testing"
	"time"

	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
)

// TestIndexFilesystemReportsOnce is the ING-2 regression. When one file
// failed, the indexer sent the error, carried on to commit the partial
// index, then sent again; the caller reads one value, so the second send
// blocked forever and the goroutine leaked with a half-written partition.
func TestIndexFilesystemReportsOnce(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	mapped := mmap.MMap(make([]byte, 4096))
	pfile := structs.NewInputFile(db.DB, nil, mapped, "evidence|||p0", cnst.PartiFileNamespace, []byte("partition-hash-0123456789abcdef!"), 4096, 0)

	// The second file's only fragment runs past the end of the partition.
	report := `{"filesystem": {"type": "ntfs"}, "files": [
		{"filename": "/ok.txt", "size": 10, "fragments": [{"start_offset": 0, "end_offset": 9}]},
		{"filename": "/bad.txt", "size": 100, "fragments": [{"start_offset": 4000, "end_offset": 9999}]}
	]}`

	idxChan := make(chan error)
	done := make(chan struct{})
	go func() {
		defer close(done)
		IndexFilesystem(report, pfile, []byte("0123456789abcdef"), idxChan, false, false)
	}()

	select {
	case err := <-idxChan:
		if err == nil {
			t.Fatal("indexer reported success for a file outside its partition")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("indexer reported nothing")
	}
	select {
	case <-done:
	case extra := <-idxChan:
		t.Fatalf("indexer reported a second time (%v) after its error", extra)
	case <-time.After(10 * time.Second):
		t.Fatal("indexer did not return after reporting its error")
	}

	// A failed partition commits nothing.
	err := db.View(func(txn *badger.Txn) error {
		for _, ns := range []string{cnst.PartiFileNamespace, cnst.IdxFileNamespace} {
			it := txn.NewIterator(badger.IteratorOptions{Prefix: []byte(ns)})
			it.Rewind()
			found := it.Valid()
			it.Close()
			if found {
				t.Errorf("failed partition left %q records behind", ns)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRegisterIndexedRangeOverflow is the PAR-8 regression: a fragment whose
// start plus length overflows int64 wrapped negative, passed the bounds
// check, and panicked slicing the mapped evidence.
func TestRegisterIndexedRangeOverflow(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	mapped := mmap.MMap(make([]byte, 4096))
	pfile := structs.NewInputFile(db.DB, nil, mapped, "evidence|||p0", cnst.PartiFileNamespace, []byte("partition-hash-0123456789abcdef!"), 4096, 0)
	idxmap := map[string]structs.IndexedFile{}
	err := registerIndexedRange(idxmap, pfile, []byte("0123456789abcdef"), "/huge", 10, math.MaxInt64, false, nil)
	if err == nil {
		t.Fatal("a range that overflows int64 was accepted")
	}
}
