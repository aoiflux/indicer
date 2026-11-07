package engine

import (
	"sync"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"

	"github.com/dgraph-io/badger/v4"
)

// keepGlobals puts back the process-wide settings a test changes.
func keepGlobals(t *testing.T) {
	t.Helper()
	container, blockIndex, chunk, low := cnst.CONTAINERMODE, cnst.LegacyBlockIndex, cnst.ChonkSize, cnst.MEMOPT
	strategy := cnst.StoreHashStrategy
	t.Cleanup(func() {
		cnst.CONTAINERMODE, cnst.LegacyBlockIndex, cnst.ChonkSize, cnst.MEMOPT = container, blockIndex, chunk, low
		cnst.StoreHashStrategy = strategy
	})
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
	e := Register(db, l, tun)
	t.Cleanup(func() { Unregister(db) })

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
	held := Register(db, l, cnst.DefaultTuning())
	t.Cleanup(func() { Unregister(db) })

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
	e := Register(db, dbmeta.Layout{ChunkKiB: 256, Storage: dbmeta.StorageContainerBIDX}, cnst.DefaultTuning())
	t.Cleanup(func() { Unregister(db) })
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
	Register(db, huge, cnst.DefaultTuning())
	t.Cleanup(func() { Unregister(db) })
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
