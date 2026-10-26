package store

import (
	"indicer/internal/faultinject"
	"time"

	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/fsx"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// When the chunk-metadata barrier releases what it holds: after this many
// chunks, after chunks holding this many bytes, or once the oldest pending
// entry has waited this long. The writer also fires it at the end of a pass.
const (
	barrierMaxChunks = 4096
	barrierMaxBytes  = 64 << 20
	barrierMaxDelay  = 250 * time.Millisecond
)

// setChunkMeta hands a chunk's C||| metadata to the write batch. It is a
// variable so tests can observe when metadata becomes visible.
var setChunkMeta = dbio.SetBatchNode

// syncChunkMaterial makes the chunk bytes written so far durable: the open
// container, when there is one, and the blob directory, so the names of new
// blobs and containers survive a power loss. Each file-mode blob is synced
// when it is written. It is a variable so tests can observe the syncs.
var syncChunkMaterial = func(dbDir string, containerMgr *fio.ContainerManager) error {
	if containerMgr != nil {
		if err := containerMgr.Sync(); err != nil {
			return err
		}
	}
	return fsx.SyncDir(util.BlobPath(dbDir))
}

// chunkMetaBarrier holds C||| metadata back until the chunk bytes it points
// at are durable (STO-5). Ingest deduplicates against C|||, so metadata that
// reached the database before its bytes could, after a power loss, make every
// later ingest of the chunk trust bytes that were never written.
//
// Relation keys need no barrier: an ingest is trusted only once it is
// FLUSHED, which the writer records after its final fire.
type chunkMetaBarrier struct {
	now  func() time.Time
	sync func() error
	set  func(key, val []byte) error

	pending []pendingChunkMeta
	bytes   int64
	oldest  time.Time
}

type pendingChunkMeta struct {
	key, val []byte
}

// newChunkMetaBarrier returns a barrier that releases metadata into batch
// once the chunk material of the database at dbDir is synced.
func newChunkMetaBarrier(dbDir string, containerMgr *fio.ContainerManager, batch *badger.WriteBatch) *chunkMetaBarrier {
	return &chunkMetaBarrier{
		now:  time.Now,
		sync: func() error { return syncChunkMaterial(dbDir, containerMgr) },
		set:  func(key, val []byte) error { return setChunkMeta(key, val, batch) },
	}
}

// record is called by the writer for every chunk it processes. A chunk with
// metadata to store is added; for any other chunk, a duplicate or one already
// stored, the barrier only checks its delay.
func (b *chunkMetaBarrier) record(key, val []byte, size int64) error {
	if len(val) == 0 {
		return b.tick()
	}
	return b.add(key, val, size)
}

// add holds the metadata val for the chunk key, whose content is size bytes,
// and fires the barrier when a threshold is reached.
func (b *chunkMetaBarrier) add(key, val []byte, size int64) error {
	if len(b.pending) == 0 {
		b.oldest = b.now()
	}
	b.pending = append(b.pending, pendingChunkMeta{key: key, val: val})
	b.bytes += size
	if len(b.pending) >= barrierMaxChunks || b.bytes >= barrierMaxBytes {
		return b.fire()
	}
	return b.tick()
}

// tick fires the barrier when the oldest pending entry has waited long
// enough. The writer calls it for every chunk, stored or not, so metadata
// does not wait behind a long run of duplicates.
func (b *chunkMetaBarrier) tick() error {
	if len(b.pending) == 0 || b.now().Sub(b.oldest) < barrierMaxDelay {
		return nil
	}
	return b.fire()
}

// fire syncs the chunk material, then releases every pending entry. When the
// sync fails nothing is released, and the error ends the ingest.
func (b *chunkMetaBarrier) fire() error {
	if len(b.pending) == 0 {
		return nil
	}
	if err := faultinject.Hit(faultinject.StoreChunkBeforeBarrier); err != nil {
		return err
	}
	if err := b.sync(); err != nil {
		return err
	}
	for _, p := range b.pending {
		if err := b.set(p.key, p.val); err != nil {
			return err
		}
	}
	if err := faultinject.Hit(faultinject.StoreChunkAfterBarrier); err != nil {
		return err
	}
	clear(b.pending)
	b.pending = b.pending[:0]
	b.bytes = 0
	return nil
}
