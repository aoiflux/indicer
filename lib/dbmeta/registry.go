package dbmeta

import (
	"sync"

	"indicer/lib/cnst"

	"github.com/dgraph-io/badger/v4"
)

var layouts sync.Map // *badger.DB -> registered

type registered struct {
	layout   Layout
	inferred bool
}

// RegisterManifest records the layout of the open database db, so code
// given only the handle reads the layout the database was written with (I3).
// dbopen registers every database it opens.
func RegisterManifest(db *badger.DB, m Manifest) {
	layouts.Store(db, registered{layout: m.Layout, inferred: m.Inferred != nil})
}

// UnregisterLayout forgets db's layout when it closes.
func UnregisterLayout(db *badger.DB) { layouts.Delete(db) }

// LayoutFor returns the layout registered for db.
func LayoutFor(db *badger.DB) (Layout, bool) {
	r, ok := layouts.Load(db)
	if !ok {
		return Layout{}, false
	}
	return r.(registered).layout, true
}

// UniformChunkSize reports whether every evidence in db was stored with the
// manifest's chunk size. That holds for a database created with a manifest,
// whose chunk size never changes. A legacy database's layout was inferred,
// and its evidence may have been stored with different chunk sizes; a
// database opened without dbopen has no layout at all.
func UniformChunkSize(db *badger.DB) bool {
	r, ok := layouts.Load(db)
	return ok && !r.(registered).inferred && r.(registered).layout.ChunkKiB > 0
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
