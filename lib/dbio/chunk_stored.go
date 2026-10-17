package dbio

import (
	"errors"
	"os"

	"indicer/lib/fio"

	"github.com/dgraph-io/badger/v4"
)

// ChunkStored reports whether the chunk under ckey is already stored with
// its material intact, as far as a stat can tell: its metadata decodes, and
// its blob is the size recorded, or its container is long enough to hold
// it. Ingest then skips the chunk instead of encoding and writing it again
// (STO-23).
//
// The chunk is found through LocateChunk, so a chunk a legacy block index
// holds counts as stored (STO-21).
//
// A chunk whose metadata or material is missing, or whose blob is not the
// size recorded, as a crash or a full disk can leave it, reports false, so
// ingest writes it again (STO-4). Only a read error is returned.
func ChunkStored(ckey []byte, db *badger.DB) (bool, error) {
	parsed, err := LocateChunk(ckey, db)
	if errors.Is(err, ErrChunkNotFound) || errors.Is(err, ErrChunkMetaInvalid) {
		// Unreadable metadata is replaced by the rewrite.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	path := ResolveBlobPath(db.Opts().Dir, parsed.Path)
	if parsed.Container {
		return fio.ContainerHolds(path, parsed.Offset, parsed.EncodedSize), nil
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == parsed.StoredSize, nil
}
