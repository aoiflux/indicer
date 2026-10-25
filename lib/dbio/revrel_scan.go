package dbio

import (
	"bytes"
	"context"
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

// CountSharedChunks counts the chunks whose reverse relations name more than
// one object, over every form reverse relations were kept in: one key per
// occurrence, the legacy per-offset maps and the segments, a chunk's
// relations possibly spread over several. It reads each form in key order,
// one chunk at a time, so its memory does not grow with the database.
// hashLen is the database's chunk hash length, which tells the chunk hash
// in a key from the bytes after it.
func CountSharedChunks(ctx context.Context, txn *badger.Txn, hashLen int) (int64, error) {
	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = false
	cursors := []*ownerCursor{
		{prefix: []byte(cnst.ReverseRelationAppendNamespace), owners: appendOwners},
		{prefix: []byte(cnst.ReverseRelationNamespace), owners: mapOwners},
		{prefix: []byte(cnst.ReverseRelationSegmentNamespace), owners: segmentOwners},
	}
	for _, c := range cursors {
		c.it = txn.NewIterator(opts)
		defer c.it.Close()
		c.it.Seek(c.prefix)
	}

	var shared int64
	keys := 0
	for {
		var next []byte
		for _, c := range cursors {
			chash, ok, err := c.chash(hashLen)
			if err != nil {
				return 0, err
			}
			if ok && (next == nil || bytes.Compare(chash, next) < 0) {
				next = chash
			}
		}
		if next == nil {
			return shared, nil
		}
		next = bytes.Clone(next)

		// The chunk is shared once a second object is seen; the rest of its
		// keys are skipped without reading their values.
		var first string
		seen, isShared := false, false
		see := func(owner string) bool {
			switch {
			case !seen:
				first, seen = owner, true
			case owner != first:
				isShared = true
			}
			return !isShared
		}
		for _, c := range cursors {
			for {
				chash, ok, err := c.chash(hashLen)
				if err != nil {
					return 0, err
				}
				if !ok || !bytes.Equal(chash, next) {
					break
				}
				if keys%4096 == 0 {
					if err := ctx.Err(); err != nil {
						return 0, err
					}
				}
				keys++
				if !isShared {
					if err := c.owners(c.it.Item(), see); err != nil {
						return 0, err
					}
				}
				c.it.Next()
			}
		}
		if isShared {
			shared++
		}
	}
}

// ownerCursor walks one form of reverse relations.
type ownerCursor struct {
	it     *badger.Iterator
	prefix []byte
	// owners calls fn with each object a key's value names, until fn
	// returns false.
	owners func(item *badger.Item, fn func(owner string) bool) error
}

// chash is the chunk hash of the key the cursor is at, if it is still in
// its form. A chunk hash is hashLen bytes followed by the separator; a key
// whose hash has another length (a legacy database) is cut at the first
// separator.
func (c *ownerCursor) chash(hashLen int) ([]byte, bool, error) {
	if !c.it.ValidForPrefix(c.prefix) {
		return nil, false, nil
	}
	key := c.it.Item().Key()
	body := key[len(c.prefix):]
	sep := []byte(cnst.DataSeperator)
	if hashLen > 0 && len(body) > hashLen+len(sep) && bytes.Equal(body[hashLen:hashLen+len(sep)], sep) {
		return body[:hashLen], true, nil
	}
	if i := bytes.Index(body, sep); i > 0 {
		return body[:i], true, nil
	}
	return nil, false, fmt.Errorf("malformed reverse relation key %q", key)
}

func appendOwners(item *badger.Item, fn func(string) bool) error {
	owner, err := decodedValue(item)
	if err != nil {
		return err
	}
	fn(string(owner))
	return nil
}

func mapOwners(item *badger.Item, fn func(string) bool) error {
	data, err := decodedValue(item)
	if err != nil {
		return err
	}
	var owners map[string]struct{}
	if err := msgpack.Unmarshal(data, &owners); err != nil {
		return fmt.Errorf("reverse relation map %q: %w", item.Key(), err)
	}
	for owner := range owners {
		if !fn(owner) {
			break
		}
	}
	return nil
}

func segmentOwners(item *badger.Item, fn func(string) bool) error {
	data, err := decodedValue(item)
	if err != nil {
		return err
	}
	state, err := decodeReverseRelationSegmentState(data)
	if err != nil {
		return fmt.Errorf("reverse relation segment %q: %w", item.Key(), err)
	}
	for owner := range state {
		if !fn(owner) {
			break
		}
	}
	return nil
}
