package dbio

import (
	"sync"

	"github.com/dgraph-io/badger/v4"
)

// kvDirs holds the real badger directory of each database that badger opened
// somewhere else: dbopen opens a database on write-protected media through a
// temporary copy of its badger files (badger needs one of them writable even
// to read).
var kvDirs sync.Map // *badger.DB -> string

// RegisterKVDir records that db's badger files belong to the kvdb directory
// dir of its data root, though badger opened them elsewhere.
func RegisterKVDir(db *badger.DB, dir string) { kvDirs.Store(db, dir) }

// UnregisterKVDir forgets db's directory; dbopen calls it at close.
func UnregisterKVDir(db *badger.DB) { kvDirs.Delete(db) }

// KVDir returns the kvdb directory of db's data root, beside which its blobs,
// graph and full-text index are found. Code needing the data root goes
// through it rather than db.Opts().Dir.
func KVDir(db *badger.DB) string {
	if v, ok := kvDirs.Load(db); ok {
		return v.(string)
	}
	return db.Opts().Dir
}
