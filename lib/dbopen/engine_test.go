package dbopen

import (
	"bytes"
	"testing"

	"indicer/internal/testutil/legacyfixture"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
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

// TestOpenNeedsNoProcessEncoder: an open database compresses its records
// and chunks with its own encoder, so it writes with no process encoder.
func TestOpenNeedsNoProcessEncoder(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	prev := cnst.ENCODER
	t.Cleanup(func() { cnst.ENCODER = prev })
	cnst.ENCODER = nil
	if engine.For(h.DB).Encoder() == nil {
		t.Fatal("an open database has no encoder")
	}
	data := bytes.Repeat([]byte("chunk "), 512)
	sum := dbmeta.ChunkHasher(h.DB)()
	sum.Write(data)
	ckey := append([]byte(cnst.ChonkNamespace), sum.Sum(nil)...)
	if _, err := blobcodec.For(h.DB).Encode(ckey, data); err != nil {
		t.Fatalf("chunk encode: %v", err)
	}
	if err := dbio.SetNode([]byte("test-record"), data, h.DB); err != nil {
		t.Fatalf("record write: %v", err)
	}
}

// TestDatabasesKeepTheirValueEncodings: a legacy quick database stores its
// record values raw and a new one compresses them, both open at once in one
// process and whatever quick mode the globals say (I3).
func TestDatabasesKeepTheirValueEncodings(t *testing.T) {
	root := setup(t)
	var quick legacyfixture.Fixture
	for _, f := range legacyfixture.Load(t) {
		if f.Mode == "quick" {
			quick = f
		}
	}
	if quick.Mode == "" {
		t.Fatal("no quick fixture")
	}
	fresh := open(t, Request{Path: root, Mode: ReadWrite})
	legacy := open(t, Request{Path: legacyfixture.Extract(t, quick), Mode: ReadWrite, Creds: dbmeta.NoPassword()})
	key, val := []byte("test-record"), bytes.Repeat([]byte("value "), 64)
	for _, global := range []bool{true, false} {
		cnst.QUICKOPT = global
		for _, c := range []struct {
			name string
			db   *badger.DB
			raw  bool
		}{{"new", fresh.DB, false}, {"quick", legacy.DB, true}} {
			if err := dbio.SetNode(key, val, c.db); err != nil {
				t.Fatal(err)
			}
			var stored []byte
			err := c.db.View(func(txn *badger.Txn) error {
				item, err := txn.Get(key)
				if err != nil {
					return err
				}
				stored, err = item.ValueCopy(nil)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if raw := bytes.Equal(stored, val); raw != c.raw {
				t.Fatalf("global quick %v: %s database stored raw=%v, want %v", global, c.name, raw, c.raw)
			}
			if got, err := zstdx.DecodeAll(stored, nil); !c.raw && (err != nil || !bytes.Equal(got, val)) {
				t.Fatalf("global quick %v: %s database value does not decode: %v", global, c.name, err)
			}
		}
	}
}
