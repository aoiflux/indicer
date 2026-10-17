// Package testdb opens throwaway DUES databases for tests and runs a test
// once per storage mode.
//
// DUES keeps its storage layout in process globals (package cnst). Every
// helper here snapshots those globals and restores them when the test ends,
// so a test that changes the chunk size or mode cannot leak it into the next.
// Because the globals are process-wide, tests using this package must not
// call t.Parallel.
package testdb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
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

// Globals is a snapshot of every mutable layout and tuning global in cnst.
type Globals struct {
	HashAlgo, CompressLevel, StoreHashStrategy, HochoMode string
	StorePipelineMode, StoreIOEngine                      string
	ChonkSize                                             int64
	MemOpt, QuickOpt, ContainerMode, LegacyBlockIndex     bool
	EnableSimhash, EnableRevrel                           bool
	StoreIOUringQueueDepth, StoreWorkerCount              int
	StoreTaskQueueDepth, RestoreWorkerCount               int
	RestoreTaskQueueDepth, RestoreProgressIntervalMs      int
	RestoreWriteBufferMB                                  int
	DB                                                    *badger.DB
}

// Snapshot captures the current globals.
func Snapshot() Globals {
	return Globals{
		HashAlgo:                  cnst.HASHALGO,
		CompressLevel:             cnst.CompressLevel,
		StoreHashStrategy:         cnst.StoreHashStrategy,
		HochoMode:                 cnst.HochoMode,
		StorePipelineMode:         cnst.StorePipelineMode,
		StoreIOEngine:             cnst.StoreIOEngine,
		ChonkSize:                 cnst.ChonkSize,
		MemOpt:                    cnst.MEMOPT,
		QuickOpt:                  cnst.QUICKOPT,
		ContainerMode:             cnst.CONTAINERMODE,
		LegacyBlockIndex:          cnst.LegacyBlockIndex,
		EnableSimhash:             cnst.ENABLESIMHASH,
		EnableRevrel:              cnst.ENABLEREVREL,
		StoreIOUringQueueDepth:    cnst.StoreIOUringQueueDepth,
		StoreWorkerCount:          cnst.StoreWorkerCount,
		StoreTaskQueueDepth:       cnst.StoreTaskQueueDepth,
		RestoreWorkerCount:        cnst.RestoreWorkerCount,
		RestoreTaskQueueDepth:     cnst.RestoreTaskQueueDepth,
		RestoreProgressIntervalMs: cnst.RestoreProgressIntervalMs,
		RestoreWriteBufferMB:      cnst.RestoreWriteBufferMB,
		DB:                        cnst.DB,
	}
}

// Restore puts the snapshot back.
func (g Globals) Restore() {
	cnst.HASHALGO = g.HashAlgo
	cnst.CompressLevel = g.CompressLevel
	cnst.StoreHashStrategy = g.StoreHashStrategy
	cnst.HochoMode = g.HochoMode
	cnst.StorePipelineMode = g.StorePipelineMode
	cnst.StoreIOEngine = g.StoreIOEngine
	cnst.ChonkSize = g.ChonkSize
	cnst.MEMOPT = g.MemOpt
	cnst.QUICKOPT = g.QuickOpt
	cnst.CONTAINERMODE = g.ContainerMode
	cnst.LegacyBlockIndex = g.LegacyBlockIndex
	cnst.ENABLESIMHASH = g.EnableSimhash
	cnst.ENABLEREVREL = g.EnableRevrel
	cnst.StoreIOUringQueueDepth = g.StoreIOUringQueueDepth
	cnst.StoreWorkerCount = g.StoreWorkerCount
	cnst.StoreTaskQueueDepth = g.StoreTaskQueueDepth
	cnst.RestoreWorkerCount = g.RestoreWorkerCount
	cnst.RestoreTaskQueueDepth = g.RestoreTaskQueueDepth
	cnst.RestoreProgressIntervalMs = g.RestoreProgressIntervalMs
	cnst.RestoreWriteBufferMB = g.RestoreWriteBufferMB
	cnst.DB = g.DB
}

// Isolate snapshots the globals now and restores them when the test ends.
func Isolate(tb testing.TB) {
	tb.Helper()
	g := Snapshot()
	tb.Cleanup(g.Restore)
}

// EnsureCodecs initialises the process-wide zstd encoder and decoder the
// storage code expects, if nothing has yet.
func EnsureCodecs(tb testing.TB) {
	tb.Helper()
	if cnst.DECODER == nil {
		d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			tb.Fatalf("zstd.NewReader: %v", err)
		}
		cnst.DECODER = d
	}
	if cnst.ENCODER == nil {
		e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			tb.Fatalf("zstd.NewWriter: %v", err)
		}
		cnst.ENCODER = e
	}
}

// Apply isolates the globals and sets them for mode m.
func Apply(tb testing.TB, m Mode) {
	tb.Helper()
	Isolate(tb)
	EnsureCodecs(tb)
	cnst.QUICKOPT = m.Quick
	cnst.CONTAINERMODE = m.Container
	cnst.LegacyBlockIndex = false
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
	Root string
	Mode Mode
	Key  []byte
}

// Open applies mode m and opens a new database in a fresh data root. The
// database is closed when the test ends; a close error fails the test.
func Open(tb testing.TB, m Mode) *DB {
	tb.Helper()
	Apply(tb, m)
	root := NewDataRoot(tb)
	return OpenAt(tb, m, root)
}

// OpenAt opens (or creates) the database at root in mode m, without changing
// the globals; call Apply first.
func OpenAt(tb testing.TB, m Mode, root string) *DB {
	tb.Helper()
	key := m.Key(tb)
	db, err := dbio.ConnectDB(root, key)
	if err != nil {
		tb.Fatalf("ConnectDB(%s, mode %s): %v", root, m.Name, err)
	}
	d := &DB{DB: db, Root: root, Mode: m, Key: key}
	tb.Cleanup(func() {
		if d.IsClosed() {
			return
		}
		if err := d.Close(); err != nil {
			tb.Errorf("closing test database: %v", err)
		}
	})
	return d
}

// Reopen closes the database and opens it again in the same mode, as a
// restarted process would.
func (d *DB) Reopen(tb testing.TB) *DB {
	tb.Helper()
	if err := d.Close(); err != nil {
		tb.Fatalf("close before reopen: %v", err)
	}
	return OpenAt(tb, d.Mode, d.Root)
}

// ZeroKey is the 32-byte all-zero key some existing tests use.
func ZeroKey() []byte { return bytes.Repeat([]byte{0}, 32) }

// ForEachMode runs fn once per mode in Modes, each as a subtest with the
// globals set for that mode and restored afterwards.
func ForEachMode(t *testing.T, fn func(t *testing.T, m Mode)) {
	t.Helper()
	for _, m := range Modes {
		t.Run(m.Name, func(t *testing.T) {
			Apply(t, m)
			fn(t, m)
		})
	}
}
