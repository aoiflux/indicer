package engine

import (
	"bytes"
	"sync"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
)

// register registers db for the test and unregisters it at the end.
func register(t *testing.T, db *badger.DB, l dbmeta.Layout, tun cnst.Tuning) *Engine {
	t.Helper()
	e, err := Register(db, l, tun)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Unregister(db) })
	return e
}

// TestUnregisteredHasTheDefaults: a database nothing registered answers with
// the default layout and tuning, keeps no block index, and encodes with the
// default encoder.
func TestUnregisteredHasTheDefaults(t *testing.T) {
	db := new(badger.DB)
	e := For(db)
	if _, ok := e.Layout(); ok {
		t.Fatal("an unregistered database has a layout")
	}
	e.SetBlockIndex(true)
	if e.Container() || e.BlockIndex() || e.RawValues() {
		t.Fatalf("container %v, block index %v, raw values %v; want none", e.Container(), e.BlockIndex(), e.RawValues())
	}
	if got := e.Tuning(); got != cnst.DefaultTuning() {
		t.Fatalf("tuning %+v, want the default", got)
	}
	if got, want := e.MaxBatchCount(), cnst.DefaultTuning().MaxBatchCount(cnst.DefaultChonkSize); got != want {
		t.Fatalf("%d pending transactions, want %d", got, want)
	}
	if e.Encoder() == nil || For(new(badger.DB)).Encoder() != e.Encoder() {
		t.Fatal("unregistered databases do not share the default encoder")
	}
	data := []byte("record record record record")
	if got, err := zstdx.DecodeAll(e.EncodeValue(data), nil); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("value does not decode back: %v", err)
	}
}

// TestRegisteredKeepsItsOwn: a registered database answers from its layout
// and tuning until it is unregistered.
func TestRegisteredKeepsItsOwn(t *testing.T) {
	db := new(badger.DB)
	l := dbmeta.Layout{ChunkKiB: 64, ChunkHash: dbmeta.HashBLAKE3, Storage: dbmeta.StorageContainerBIDX}
	tun := cnst.DefaultTuning()
	tun.LowResource, tun.HashStrategy = true, cnst.AsyncHashStrategy
	e := register(t, db, l, tun)

	if For(db) != e {
		t.Fatal("For returned another engine")
	}
	if got, ok := e.Layout(); !ok || got != l {
		t.Fatalf("layout %+v, %v; want %+v", got, ok, l)
	}
	if !e.Container() || !e.BlockIndex() || e.Tuning() != tun {
		t.Fatalf("container %v, block index %v, tuning %+v", e.Container(), e.BlockIndex(), e.Tuning())
	}

	Unregister(db)
	if For(db) == e {
		t.Fatal("an unregistered database kept its engine")
	}
}

// TestSetLayoutInPlace: repair changes an open database's storage; whoever
// holds its engine sees the change.
func TestSetLayoutInPlace(t *testing.T) {
	db := new(badger.DB)
	l := dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageContainerBIDX}
	held := register(t, db, l, cnst.DefaultTuning())

	l.Storage = dbmeta.StorageContainer
	For(db).SetLayout(l)
	For(db).SetBlockIndex(false)
	if got, _ := held.Layout(); got.Storage != dbmeta.StorageContainer || held.BlockIndex() {
		t.Fatalf("storage %s, block index %v after the change", got.Storage, held.BlockIndex())
	}
	if !held.Container() {
		t.Fatal("a container database is not one after the change")
	}
}

// TestConcurrentUse: readers and the repair that changes the layout and the
// block index may run at once (run under -race).
func TestConcurrentUse(t *testing.T) {
	db := new(badger.DB)
	e := register(t, db, dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageContainerBIDX}, cnst.DefaultTuning())
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				if i == 0 {
					e.SetLayout(dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageContainer})
					e.SetBlockIndex(false)
					continue
				}
				_ = For(db).Container()
				_ = For(db).BlockIndex()
				_, _ = For(db).Layout()
			}
		}()
	}
	wg.Wait()
}

// TestMaxBatchCountTakesTheChunkSize: the pending-transaction limit comes
// from the database's chunk size and its tuning.
func TestMaxBatchCountTakesTheChunkSize(t *testing.T) {
	// Chunks of a TiB: no machine has four available, so one transaction.
	huge := dbmeta.Layout{ChunkKiB: 1 << 30, Storage: dbmeta.StorageFile}
	if n := register(t, new(badger.DB), huge, cnst.DefaultTuning()).MaxBatchCount(); n != 1 {
		t.Fatalf("1 TiB chunks: %d pending transactions, want 1", n)
	}
	low := cnst.DefaultTuning()
	low.LowResource = true
	if n := register(t, new(badger.DB), huge, low).MaxBatchCount(); n != 16 {
		t.Fatalf("low resource: %d pending transactions, want 16", n)
	}
}

// TestEncodeValueFollowsTheLayout: a database of raw values stores them as
// they are and any other compresses them with its own encoder.
func TestEncodeValueFollowsTheLayout(t *testing.T) {
	data := bytes.Repeat([]byte("record "), 200)
	raw := register(t, new(badger.DB), dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageFile, ValueEncoding: dbmeta.EncodingRaw}, cnst.DefaultTuning())
	zst := register(t, new(badger.DB), dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageFile, ValueEncoding: dbmeta.EncodingZstd}, cnst.DefaultTuning())
	if !raw.RawValues() || !bytes.Equal(raw.EncodeValue(data), data) {
		t.Fatal("a raw-values database changed its value")
	}
	if zst.RawValues() || zst.Encoder() == nil {
		t.Fatal("a zstd database has raw values or no encoder")
	}
	got, err := zstdx.DecodeAll(zst.EncodeValue(data), nil)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("zstd value does not decode back: %v", err)
	}
}
