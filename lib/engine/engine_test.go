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

// keepGlobals puts back the process-wide settings a test changes.
func keepGlobals(t *testing.T) {
	t.Helper()
	container, blockIndex, chunk, low := cnst.CONTAINERMODE, cnst.LegacyBlockIndex, cnst.ChonkSize, cnst.MEMOPT
	strategy, quick := cnst.StoreHashStrategy, cnst.QUICKOPT
	t.Cleanup(func() {
		cnst.CONTAINERMODE, cnst.LegacyBlockIndex, cnst.ChonkSize, cnst.MEMOPT = container, blockIndex, chunk, low
		cnst.StoreHashStrategy, cnst.QUICKOPT = strategy, quick
	})
}

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

// TestUnregisteredFollowsGlobals: a database dbopen did not open answers
// from the process-wide settings as they are at each call, so a test that
// changes one after opening its database sees the change.
func TestUnregisteredFollowsGlobals(t *testing.T) {
	keepGlobals(t)
	db := new(badger.DB)
	e := For(db)
	if _, ok := e.Layout(); ok {
		t.Fatal("an unregistered database has a layout")
	}
	for _, on := range []bool{true, false} {
		cnst.CONTAINERMODE, cnst.LegacyBlockIndex = on, on
		if e.Container() != on || For(db).BlockIndex() != on {
			t.Fatalf("container %v, block index %v; want both %v", e.Container(), e.BlockIndex(), on)
		}
	}
	cnst.StoreHashStrategy = "no-such-strategy"
	if got := e.Tuning(); got != cnst.Globals() || got.HashStrategy != "no-such-strategy" {
		t.Fatalf("tuning %+v, want the globals as they are", got)
	}
	e.SetBlockIndex(true)
	if !cnst.LegacyBlockIndex {
		t.Fatal("SetBlockIndex on an unregistered database left the global off")
	}
}

// TestRegisteredKeepsItsOwn: a registered database answers from its layout
// and tuning, whatever the globals say, until it is unregistered.
func TestRegisteredKeepsItsOwn(t *testing.T) {
	keepGlobals(t)
	db := new(badger.DB)
	l := dbmeta.Layout{ChunkKiB: 64, ChunkHash: dbmeta.HashBLAKE3, Storage: dbmeta.StorageContainerBIDX}
	tun := cnst.DefaultTuning()
	tun.LowResource, tun.HashStrategy = true, cnst.AsyncHashStrategy
	e := register(t, db, l, tun)

	cnst.CONTAINERMODE, cnst.LegacyBlockIndex, cnst.MEMOPT = false, false, false
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
// from the database's chunk size, or the global one for a database dbopen
// did not open.
func TestMaxBatchCountTakesTheChunkSize(t *testing.T) {
	keepGlobals(t)
	db := new(badger.DB)
	// Chunks of a TiB: no machine has four available, so one transaction.
	huge := dbmeta.Layout{ChunkKiB: 1 << 30, Storage: dbmeta.StorageFile}
	register(t, db, huge, cnst.DefaultTuning())
	if n := For(db).MaxBatchCount(); n != 1 {
		t.Fatalf("1 TiB chunks: %d pending transactions, want 1", n)
	}

	other := new(badger.DB)
	cnst.ChonkSize, cnst.MEMOPT = 1<<40, false
	if n := For(other).MaxBatchCount(); n != 1 {
		t.Fatalf("unregistered, 1 TiB chunks: %d pending transactions, want 1", n)
	}
	cnst.MEMOPT = true
	if n := For(other).MaxBatchCount(); n != 16 {
		t.Fatalf("unregistered, low resource: %d pending transactions, want 16", n)
	}
}

// TestEncodeValueFollowsTheLayout: a database of raw values stores them as
// they are and any other compresses them with its own encoder, whatever
// quick mode the process says and whether it has an encoder at all.
func TestEncodeValueFollowsTheLayout(t *testing.T) {
	keepGlobals(t)
	data := bytes.Repeat([]byte("record "), 200)
	raw := register(t, new(badger.DB), dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageFile, ValueEncoding: dbmeta.EncodingRaw}, cnst.DefaultTuning())
	zst := register(t, new(badger.DB), dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageFile, ValueEncoding: dbmeta.EncodingZstd}, cnst.DefaultTuning())
	prevEnc := cnst.ENCODER
	t.Cleanup(func() { cnst.ENCODER = prevEnc })
	cnst.ENCODER = nil
	for _, quick := range []bool{true, false} {
		cnst.QUICKOPT = quick
		if !raw.RawValues() || !bytes.Equal(raw.EncodeValue(data), data) {
			t.Fatalf("quick %v: a raw-values database changed its value", quick)
		}
		if zst.RawValues() || zst.Encoder() == nil {
			t.Fatalf("quick %v: a zstd database has raw values or no encoder", quick)
		}
		got, err := zstdx.DecodeAll(zst.EncodeValue(data), nil)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("quick %v: zstd value does not decode back: %v", quick, err)
		}
	}
}

// TestUnregisteredEncodesByGlobals: a database dbopen did not open encodes
// as quick mode and the process encoder say at each call.
func TestUnregisteredEncodesByGlobals(t *testing.T) {
	keepGlobals(t)
	enc, err := zstdx.Acquire("fast", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(enc.Release)
	prevEnc := cnst.ENCODER
	t.Cleanup(func() { cnst.ENCODER = prevEnc })
	cnst.ENCODER = enc
	e, data := For(new(badger.DB)), []byte("record record record record")
	cnst.QUICKOPT = true
	if !e.RawValues() || !bytes.Equal(e.EncodeValue(data), data) {
		t.Fatal("quick mode compressed a value")
	}
	cnst.QUICKOPT = false
	if e.RawValues() || e.Encoder() != enc {
		t.Fatal("unregistered database ignores the process encoder")
	}
	if got, err := zstdx.DecodeAll(e.EncodeValue(data), nil); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("value does not decode back: %v", err)
	}
}
