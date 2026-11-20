package dbio

import (
	"errors"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/engine"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/zstdx"

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
	var loc structs.ChonkMetadata
	err := db.View(func(txn *badger.Txn) error {
		var err error
		loc, err = locateChunkTxn(txn, ckey, KVDir(db), engine.For(db).BlockIndex())
		return err
	})
	return loc, err
}

// locateChunkTxn is LocateChunk inside txn, for the database whose
// directory is dir. A chunk not in C||| is looked up in the block index when
// blockIndex is set (engine.Engine.BlockIndex).
func locateChunkTxn(txn *badger.Txn, ckey []byte, dir string, blockIndex bool) (structs.ChonkMetadata, error) {
	item, err := txn.Get(ckey)
	if err == nil {
		raw, err := item.ValueCopy(nil)
		if err != nil {
			return structs.ChonkMetadata{}, err
		}
		if decoded, err := zstdx.DecodeAll(raw, nil); err == nil {
			raw = decoded
		}
		return parseChonkMetadata(raw)
	}
	if !errors.Is(err, badger.ErrKeyNotFound) {
		return structs.ChonkMetadata{}, err
	}
	notFound := fmt.Errorf("%w: %x (%w)", ErrChunkNotFound, ckey[min(len(ckey), len(cnst.ChonkNamespace)):], err)
	if !blockIndex {
		return structs.ChonkMetadata{}, notFound
	}
	path, offset, size, err := fio.LookupBlockIndex(dir, ckey)
	if errors.Is(err, fio.ErrChunkNotIndexed) {
		return structs.ChonkMetadata{}, notFound
	}
	if err != nil {
		return structs.ChonkMetadata{}, err
	}
	return structs.ChonkMetadata{Path: path, Offset: offset, StoredSize: size, EncodedSize: size, Container: true}, nil
}
