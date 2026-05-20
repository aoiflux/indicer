package store

import (
	"sync"
	"unsafe"
)

const chunkSetShards = 256

// shardedChunkSet is a concurrency-safe set of chunk hash keys backed by 256
// independent shards, each guarded by its own RWMutex. It replaces sync.Map
// for the cross-worker deduplication gate in the ingest pipeline.
//
// Design notes:
//   - Sharding on chash[0] distributes load evenly because hash bytes are
//     uniformly distributed.
//   - The loadOrStore read probe uses unsafe.String to avoid allocating a string
//     from the byte slice; an allocation only occurs on the write path when a
//     new key is inserted.
type shardedChunkSet struct {
	shards [chunkSetShards]chunkSetShard
}

type chunkSetShard struct {
	mu      sync.RWMutex
	entries map[string]struct{}
}

func newShardedChunkSet() *shardedChunkSet {
	s := &shardedChunkSet{}
	for i := range s.shards {
		s.shards[i].entries = make(map[string]struct{})
	}
	return s
}

// loadOrStore reports whether chash was already present in the set. If not, it
// inserts it and returns false. Callers must not modify chash after calling
// loadOrStore.
func (s *shardedChunkSet) loadOrStore(chash []byte) (alreadyExists bool) {
	if len(chash) == 0 {
		return false
	}
	sh := &s.shards[chash[0]]

	// Fast path: read lock only.
	key := unsafeString(chash)
	sh.mu.RLock()
	_, exists := sh.entries[key]
	sh.mu.RUnlock()
	if exists {
		return true
	}

	// Slow path: promote to write lock and insert.
	sh.mu.Lock()
	_, exists = sh.entries[key] // re-check under write lock
	if !exists {
		sh.entries[string(chash)] = struct{}{} // allocate only on insert
	}
	sh.mu.Unlock()
	return exists
}

// unsafeString converts a byte slice to a string without allocation. The
// returned string shares memory with the slice and must not be stored beyond
// the slice's lifetime.
func unsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}
