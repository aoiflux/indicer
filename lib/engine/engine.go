// Package engine holds what the storage code knows about each open
// database: the layout it was written with, the tuning it was opened with,
// whether its retired block index is still read, and the encoder its values
// and chunks are compressed with. dbopen registers each database it opens,
// so code given only the *badger.DB reads that database's settings and not
// another's (I3).
package engine

import (
	"sync"
	"sync/atomic"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
)

// Engine is one open database's settings. It is safe for concurrent use.
type Engine struct {
	// registered is false for the engine of a database dbopen did not open,
	// which answers from the process-wide settings.
	registered bool
	tuning     cnst.Tuning
	layout     atomic.Pointer[dbmeta.Layout]
	blockIndex atomic.Bool
	enc        *zstdx.Encoder
}

var engines sync.Map // *badger.DB -> *Engine

// unregistered answers for every database dbopen did not open, as some tests
// and tools open them, from the process-wide settings as they are at each
// call.
var unregistered = &Engine{}

// Register records the layout l and tuning t of the database db, which
// dbopen has just opened, acquires its encoder, and returns its engine. The
// encoder compresses at the tuning's level and is as wide as its store
// workers (cnst.Tuning.EncodeWorkers). The block index is read when l says
// the database keeps one (StorageContainerBIDX); SetBlockIndex changes that.
func Register(db *badger.DB, l dbmeta.Layout, t cnst.Tuning) (*Engine, error) {
	enc, err := zstdx.Acquire(t.CompressLevel, t.EncodeWorkers(l.Storage != dbmeta.StorageFile))
	if err != nil {
		return nil, err
	}
	e := &Engine{registered: true, tuning: t, enc: enc}
	e.layout.Store(&l)
	e.blockIndex.Store(l.Storage == dbmeta.StorageContainerBIDX)
	if old, loaded := engines.Swap(db, e); loaded {
		old.(*Engine).enc.Release()
	}
	return e, nil
}

// Unregister forgets db when it closes, and releases its encoder.
func Unregister(db *badger.DB) {
	if old, loaded := engines.LoadAndDelete(db); loaded {
		old.(*Engine).enc.Release()
	}
}

// For returns db's engine. A database dbopen did not open gets one that
// answers from the process-wide settings.
func For(db *badger.DB) *Engine {
	if e, ok := engines.Load(db); ok {
		return e.(*Engine)
	}
	return unregistered
}

// Layout returns the database's layout, and false for a database dbopen did
// not open, which has none.
func (e *Engine) Layout() (dbmeta.Layout, bool) {
	if !e.registered {
		return dbmeta.Layout{}, false
	}
	return *e.layout.Load(), true
}

// SetLayout replaces the layout, when `repair --upgrade` records a new
// storage while the database is open. A database dbopen did not open has no
// layout to replace.
func (e *Engine) SetLayout(l dbmeta.Layout) {
	if e.registered {
		e.layout.Store(&l)
	}
}

// Tuning returns the tuning the database was opened with.
func (e *Engine) Tuning() cnst.Tuning {
	if !e.registered {
		return cnst.Globals()
	}
	return e.tuning
}

// Container reports whether the database packs chunks into container files.
func (e *Engine) Container() bool {
	if !e.registered {
		return cnst.CONTAINERMODE
	}
	return e.layout.Load().Storage != dbmeta.StorageFile
}

// BlockIndex reports whether chunk lookups that miss C||| fall back to the
// block index of the retired hierarchical mode (D9).
func (e *Engine) BlockIndex() bool {
	if !e.registered {
		return cnst.LegacyBlockIndex
	}
	return e.blockIndex.Load()
}

// SetBlockIndex turns the block index lookup on or off: on while block index
// files remain beside the database, off once repair deletes them.
func (e *Engine) SetBlockIndex(on bool) {
	if !e.registered {
		cnst.LegacyBlockIndex = on
		return
	}
	e.blockIndex.Store(on)
}

// MaxBatchCount is how many transactions a write batch on the database may
// have pending (cnst.Tuning.MaxBatchCount, for its chunk size).
func (e *Engine) MaxBatchCount() int {
	if !e.registered {
		return cnst.Globals().MaxBatchCount(cnst.ChonkSize)
	}
	return e.tuning.MaxBatchCount(int64(e.layout.Load().ChunkKiB) * cnst.KB)
}

// RawValues reports whether the database stores record values as they are,
// as legacy quick mode did, rather than zstd-compressed.
func (e *Engine) RawValues() bool {
	if !e.registered {
		return cnst.QUICKOPT
	}
	return e.layout.Load().ValueEncoding == dbmeta.EncodingRaw
}

// Encoder returns the database's zstd encoder, which its blob codec also
// compresses chunks with.
func (e *Engine) Encoder() *zstdx.Encoder {
	if !e.registered {
		return cnst.ENCODER
	}
	return e.enc
}

// EncodeValue returns data as the database stores a record value:
// compressed with its encoder, or as it is when its values are raw.
func (e *Engine) EncodeValue(data []byte) []byte {
	if e.RawValues() {
		return data
	}
	return e.Encoder().EncodeAll(data, make([]byte, 0, len(data)))
}
