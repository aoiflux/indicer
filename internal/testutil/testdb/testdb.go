// Package testdb opens throwaway DUES databases for tests and runs a test
// once per storage mode.
//
// A test database is opened without dbopen, as a legacy database (one with
// no manifest) would be, and registered the way dbopen registers one: its
// layout, tuning, encoder and blob codec belong to it alone (package
// engine), so nothing a test chooses for one database reaches another.
package testdb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"

	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/util"
)

// Mode is one storage configuration a database can be written in.
type Mode struct {
	Name      string
	Quick     bool
	Container bool
	Password  string
}

// The storage modes the matrix covers. Default is what `dues store` uses with
// no flags; the others are the non-default paths that shipped untested.
var (
	Default   = Mode{Name: "default"}
	Encrypted = Mode{Name: "encrypted", Password: "test-password"}
	Quick     = Mode{Name: "quick", Quick: true}
	Container = Mode{Name: "container", Container: true}
)

// Modes is the mode matrix ForEachMode runs.
var Modes = []Mode{Default, Encrypted, Quick, Container}

// Key returns the database key for the mode's password, derived the way the
// CLI derives it.
func (m Mode) Key(tb testing.TB) []byte {
	tb.Helper()
	key, err := util.HashPassword(m.Password)
	if err != nil {
		tb.Fatalf("HashPassword: %v", err)
	}
	return key
}

// Options are a test database's layout and tuning beyond its mode. The zero
// value is the default layout (256 KiB chunks, BLAKE3) and DefaultTuning.
type Options struct {
	// ChunkKiB is the chunk size in KiB; 0 means cnst.DefaultChonkSize.
	ChunkKiB int
	// Hash is the chunk hash, cnst.SHA3 or cnst.BLAKE3 (the default).
	Hash string
	// Tuning is the tuning the database runs with; nil means
	// cnst.DefaultTuning.
	Tuning *cnst.Tuning
}

// Layout returns the layout a database of mode m with these options has.
func (o Options) Layout(m Mode) dbmeta.Layout {
	l := dbmeta.Layout{
		ChunkKiB:          int(cnst.DefaultChonkSize / cnst.KB),
		ChunkHash:         dbmeta.HashBLAKE3,
		Storage:           dbmeta.StorageFile,
		BlobEncoding:      blobcodec.LegacySealed,
		ValueEncoding:     dbmeta.EncodingZstd,
		BadgerCompression: dbmeta.EncodingZstd,
	}
	if o.ChunkKiB > 0 {
		l.ChunkKiB = o.ChunkKiB
	}
	if cnst.NormalizeHashAlgo(o.Hash) == cnst.SHA3 {
		l.ChunkHash = dbmeta.HashSHA3
	}
	if m.Container {
		l.Storage = dbmeta.StorageContainer
	}
	if m.Quick {
		l.BlobEncoding = blobcodec.LegacyRaw
		l.ValueEncoding = dbmeta.EncodingRaw
		l.BadgerCompression = dbmeta.EncodingSnappy
	}
	return l
}

// tuning returns the tuning the options ask for.
func (o Options) tuning() cnst.Tuning {
	if o.Tuning != nil {
		return *o.Tuning
	}
	return cnst.DefaultTuning()
}

// NewDataRoot returns a fresh, empty data root inside the test's temp dir.
func NewDataRoot(tb testing.TB) string {
	tb.Helper()
	root := filepath.Join(tb.TempDir(), "dues-data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		tb.Fatal(err)
	}
	return root
}

// DB is an open test database.
type DB struct {
	*badger.DB
	Root    string
	Mode    Mode
	Key     []byte
	Options Options
}

// Open opens a new database of mode m in a fresh data root, with the
// options given (at most one). The database is closed when the test ends; a
// close error fails the test.
func Open(tb testing.TB, m Mode, opts ...Options) *DB {
	tb.Helper()
	return OpenAt(tb, m, NewDataRoot(tb), opts...)
}

// OpenAt opens (or creates) the database at root in mode m, with the options
// given (at most one).
func OpenAt(tb testing.TB, m Mode, root string, opts ...Options) *DB {
	tb.Helper()
	var o Options
	switch len(opts) {
	case 0:
	case 1:
		o = opts[0]
	default:
		tb.Fatalf("testdb: %d Options, want at most one", len(opts))
	}
	key := m.Key(tb)
	db, err := dbio.ConnectDBTuned(root, key, m.Quick, o.tuning())
	if err != nil {
		tb.Fatalf("ConnectDB(%s, mode %s): %v", root, m.Name, err)
	}
	d := &DB{DB: db, Root: root, Mode: m, Key: key, Options: o}
	if err := d.register(); err != nil {
		_ = db.Close()
		tb.Fatalf("registering test database: %v", err)
	}
	tb.Cleanup(func() {
		if d.IsClosed() {
			unregister(db)
			return
		}
		if err := d.Close(); err != nil {
			tb.Errorf("closing test database: %v", err)
		}
	})
	return d
}

// register records the database's layout, tuning, encoder and blob codec,
// as dbopen does for a legacy database it opens. Its layout counts as
// inferred, so the database is not taken to hold one chunk size only.
func (d *DB) register() error {
	l := d.Options.Layout(d.Mode)
	eng, err := engine.Register(d.DB, l, d.Options.tuning())
	if err != nil {
		return err
	}
	codec := blobcodec.NewLegacyRaw()
	if !d.Mode.Quick {
		codec = blobcodec.NewLegacySealed(d.DB.Opts().EncryptionKey, eng.Encoder())
	}
	blobcodec.Register(d.DB, codec)
	dbmeta.RegisterManifest(d.DB, dbmeta.Manifest{Layout: l, Inferred: &dbmeta.Inference{Notes: []string{"testdb"}}})
	return nil
}

func unregister(db *badger.DB) {
	blobcodec.Unregister(db)
	dbmeta.UnregisterLayout(db)
	engine.Unregister(db)
}

// Reconfigure changes the database's options while it is open, as a test
// that stores with one chunk size or tuning and then another does.
func (d *DB) Reconfigure(tb testing.TB, o Options) {
	tb.Helper()
	d.Options = o
	if err := d.register(); err != nil {
		tb.Fatalf("reconfiguring test database: %v", err)
	}
}

// Close forgets the database's registration and closes it.
func (d *DB) Close() error {
	unregister(d.DB)
	return d.DB.Close()
}

// Reopen closes the database and opens it again with the same mode and
// options, as a restarted process would.
func (d *DB) Reopen(tb testing.TB) *DB {
	tb.Helper()
	if err := d.Close(); err != nil {
		tb.Fatalf("close before reopen: %v", err)
	}
	return OpenAt(tb, d.Mode, d.Root, d.Options)
}

// ZeroKey is the 32-byte all-zero key some existing tests use.
func ZeroKey() []byte { return bytes.Repeat([]byte{0}, 32) }

// ForEachMode runs fn once per mode in Modes, each as a subtest.
func ForEachMode(t *testing.T, fn func(t *testing.T, m Mode)) {
	t.Helper()
	for _, m := range Modes {
		t.Run(m.Name, func(t *testing.T) {
			fn(t, m)
		})
	}
}
