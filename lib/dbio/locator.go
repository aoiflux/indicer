package dbio

import (
	"errors"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/fio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// ErrChunkNotFound reports a chunk the database holds no location for. The
// error also matches badger.ErrKeyNotFound.
var ErrChunkNotFound = errors.New("chunk not found")

// ErrChunkMetaInvalid reports C||| metadata that does not decode or describe
// a location.
var ErrChunkMetaInvalid = errors.New("invalid chunk metadata")

// LocateChunk returns where the chunk ckey is stored: its C||| metadata, or,
// in a legacy hierarchical database, its block index record (STO-21). Every
// path that needs a chunk's location goes through it: reads, the skip of
// chunks already stored, and ingest validation.
//
// A chunk with no location gives an error wrapping ErrChunkNotFound. Any
// other error, a database or block index read error or damaged metadata, is
// returned as such, never taken for a missing chunk.
func LocateChunk(ckey []byte, db *badger.DB) (structs.ChonkMetadata, error) {
	raw, err := GetNode(ckey, db)
	if err == nil {
		return parseChonkMetadata(raw)
	}
	if !errors.Is(err, badger.ErrKeyNotFound) {
		return structs.ChonkMetadata{}, err
	}
	notFound := fmt.Errorf("%w: %x (%w)", ErrChunkNotFound, ckey[min(len(ckey), len(cnst.ChonkNamespace)):], err)
	if !cnst.LegacyBlockIndex {
		return structs.ChonkMetadata{}, notFound
	}
	path, offset, size, err := fio.LookupBlockIndex(db.Opts().Dir, ckey)
	if errors.Is(err, fio.ErrChunkNotIndexed) {
		return structs.ChonkMetadata{}, notFound
	}
	if err != nil {
		return structs.ChonkMetadata{}, err
	}
	return structs.ChonkMetadata{Path: path, Offset: offset, StoredSize: size, EncodedSize: size, Container: true}, nil
}
