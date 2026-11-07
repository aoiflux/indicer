package store

import (
	"bytes"
	"errors"
	"runtime"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
)

func TestProcessRevRelSkippedWhenDisabled(t *testing.T) {
	db := openTestDB(t)

	index := int64(0)
	fhash := []byte("file-disabled")
	chash := []byte("chunk-disabled")

	batch := db.NewWriteBatch()
	if err := processRevRel(false, index, fhash, chash, db, batch); err != nil {
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

	index := int64(0)
	fhash := []byte("file-enabled")
	chash := []byte("chunk-enabled")

	batch := db.NewWriteBatch()
	if err := processRevRel(true, index, fhash, chash, db, batch); err != nil {
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
	if cnst.ENCODER == nil {
		encoder, err := zstdx.Acquire("best", runtime.GOMAXPROCS(0))
		if err != nil {
			t.Fatalf("zstdx.Acquire: %v", err)
		}
		cnst.ENCODER = encoder
	}
	t.Cleanup(func() {
		cnst.QUICKOPT = prevQuick
	})

	db, err := dbio.ConnectDB(t.TempDir(), bytes.Repeat([]byte{0}, 32), cnst.QUICKOPT)
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

// testEvidence names an evidence record a test writes by hand: a fresh ID,
// by which its record and relations are keyed, and a 128-byte size. The
// hash argument is ignored, as InputFile ignored it for keys.
type testEvidence struct {
	id   []byte
	name string
}

func newTestInputFile(_ *badger.DB, name string, _ []byte) testEvidence {
	id, err := uuid.NewV7()
	if err != nil {
		panic(err)
	}
	return testEvidence{id: id[:], name: name}
}

func (e testEvidence) GetID() []byte        { return e.id }
func (e testEvidence) GetHash() []byte      { return e.id }
func (e testEvidence) GetName() string      { return e.name }
func (e testEvidence) GetStartIndex() int64 { return 0 }
func (e testEvidence) GetSize() int64       { return 128 }
func (e testEvidence) GetInternalObjects() map[string]structs.InternalOffset {
	return map[string]structs.InternalOffset{}
}
