package dbopen

import (
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/engine"
)

// TestOpenRegistersEngine: an open registers the database's layout and the
// tuning it was asked for, and closing it forgets them.
func TestOpenRegistersEngine(t *testing.T) {
	root := setup(t)
	tun := cnst.DefaultTuning()
	tun.LowResource, tun.HashStrategy, tun.StoreWorkers = true, cnst.AsyncHashStrategy, 3
	h := open(t, Request{Path: root, Mode: ReadWrite, Layout: Requested{ChunkKiB: 64, Container: true}, Tuning: &tun})
	db := h.DB
	e := engine.For(db)
	if l, ok := e.Layout(); !ok || l != h.Manifest.Layout {
		t.Fatalf("engine layout %+v, %v; want the manifest's %+v", l, ok, h.Manifest.Layout)
	}
	if e.Tuning() != tun || !e.Container() || e.BlockIndex() {
		t.Fatalf("tuning %+v, container %v, block index %v", e.Tuning(), e.Container(), e.BlockIndex())
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := engine.For(db).Layout(); ok {
		t.Fatal("a closed database is still registered")
	}
}

// TestOpenTakesGlobalTuning: an open given no tuning takes the process-wide
// settings as they are when it opens, and keeps them.
func TestOpenTakesGlobalTuning(t *testing.T) {
	root := setup(t)
	cnst.StoreHashStrategy = cnst.HochoHashStrategy
	h := open(t, Request{Path: root, Mode: ReadWrite})
	want := cnst.Globals()
	cnst.StoreHashStrategy = cnst.SyncHashStrategy
	if got := engine.For(h.DB).Tuning(); got != want {
		t.Fatalf("tuning %+v, want the globals at the open %+v", got, want)
	}
}
