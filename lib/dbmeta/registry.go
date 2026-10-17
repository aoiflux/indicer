package dbmeta

import (
	"sync"

	"indicer/lib/cnst"

	"github.com/dgraph-io/badger/v4"
)

var layouts sync.Map // *badger.DB -> Layout

// RegisterLayout records the layout of the open database db, so code given
// only the handle reads the layout the database was written with (I3).
// dbopen registers every database it opens.
func RegisterLayout(db *badger.DB, l Layout) { layouts.Store(db, l) }

// UnregisterLayout forgets db's layout when it closes.
func UnregisterLayout(db *badger.DB) { layouts.Delete(db) }

// LayoutFor returns the layout registered for db.
func LayoutFor(db *badger.DB) (Layout, bool) {
	l, ok := layouts.Load(db)
	if !ok {
		return Layout{}, false
	}
	return l.(Layout), true
}

// ChunkSize returns the chunk size in bytes of the database db. A database
// opened without dbopen, as some tests do, has no registered layout and gets
// the process-wide setting.
func ChunkSize(db *badger.DB) int64 {
	if l, ok := LayoutFor(db); ok && l.ChunkKiB > 0 {
		return int64(l.ChunkKiB) * cnst.KB
	}
	return cnst.ChonkSize
}
