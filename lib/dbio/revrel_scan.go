package dbio

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"

	"indicer/lib/cnst"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// ForEachChunkOccurrence calls fn with each recorded occurrence of the chunk
// chash: the raw ID of the evidence or materialised object that holds it,
// and its offset there. Every form reverse relations were kept in is read
// (one key per occurrence, the legacy per-offset maps and the segments),
// and an occurrence is reported once. After limit occurrences (no limit
// when limit <= 0) it stops and reports truncated if more remain. fn runs
// inside a read transaction and must not write.
func ForEachChunkOccurrence(db *badger.DB, chash []byte, limit int, fn func(owner []byte, offset int64) error) (truncated bool, err error) {
	type occ struct {
		owner  string
		offset int64
	}
	seen := map[occ]struct{}{}
	errLimit := errors.New("limit")
	emit := func(owner []byte, offset int64) error {
		k := occ{string(owner), offset}
		if _, dup := seen[k]; dup {
			return nil
		}
		if limit > 0 && len(seen) >= limit {
			return errLimit
		}
		seen[k] = struct{}{}
		return fn(owner, offset)
	}
	err = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 256
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := util.AppendToBytesSlice(cnst.ReverseRelationAppendNamespace, chash, cnst.DataSeperator)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			idx, ok := reverseRelationAppendMemberIndex(chash, it.Item().Key())
			if !ok {
				return fmt.Errorf("malformed reverse relation key %q", it.Item().Key())
			}
			owner, err := decodedValue(it.Item())
			if err != nil {
				return err
			}
			if err := emit(owner, idx); err != nil {
				return err
			}
		}

		// Legacy maps: Я|||:<chash>|||<offset> holds the owners at offset.
		prefix = util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			idx, err := strconv.ParseInt(string(bytes.TrimLeft(it.Item().Key()[len(prefix):], "|:")), 10, 64)
			if err != nil {
				continue // another chunk whose hash extends this one's
			}
			data, err := decodedValue(it.Item())
			if err != nil {
				return err
			}
			var owners map[string]struct{}
			if err := msgpack.Unmarshal(data, &owners); err != nil {
				return fmt.Errorf("reverse relation map %q: %w", it.Item().Key(), err)
			}
			for owner := range owners {
				if err := emit([]byte(owner), idx); err != nil {
					return err
				}
			}
		}

		prefix = util.AppendToBytesSlice(cnst.ReverseRelationSegmentNamespace, chash, cnst.DataSeperator)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			data, err := decodedValue(it.Item())
			if err != nil {
				return err
			}
			state, err := decodeReverseRelationSegmentState(data)
			if err != nil {
				return err
			}
			for owner, idxs := range state {
				for _, idx := range idxs {
					if err := emit([]byte(owner), idx); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if errors.Is(err, errLimit) {
		return true, nil
	}
	return false, err
}

func decodedValue(item *badger.Item) ([]byte, error) {
	data, err := item.ValueCopy(nil)
	if err != nil {
		return nil, err
	}
	if decoded, err := cnst.DECODER.DecodeAll(data, nil); err == nil {
		data = decoded
	}
	return data, nil
}

// ChunkOccursAt reports whether a reverse relation records the chunk chash
// at offset of the object owner.
func ChunkOccursAt(db *badger.DB, chash []byte, offset int64, owner []byte) (bool, error) {
	owners, err := GetReverseRelationNode(util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, offset), db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, ok := owners[string(owner)]
	return ok, nil
}

// RelationAt returns the hash of the chunk at offset of the object rid
// (an evidence or a materialised object).
func RelationAt(db *badger.DB, rid []byte, offset int64) ([]byte, error) {
	var chash []byte
	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(relationKey(rid, offset))
		if err != nil {
			return err
		}
		chash, err = decodedValue(item)
		return err
	})
	return chash, err
}
