package dbio

// The reverse-relation segment writer, kept only to seed tests: databases
// written before STO-14 hold segments, which readers still merge, but
// nothing writes them any more.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"indicer/lib/zstdx"
	"sort"

	"github.com/dgraph-io/badger/v4"
)

func seedLegacySegmentMembers(fhash []byte, members []ReverseRelationAppendMember, db *badger.DB, batch *badger.WriteBatch) error {
	if len(members) == 0 {
		return nil
	}
	if db == nil || batch == nil {
		return fmt.Errorf("reverse relation segment write requires db and batch")
	}

	shard := reverseRelationAppendShard(fhash)
	fhashKey := string(fhash)

	keysByChash := make(map[string][]byte)
	idxByChash := make(map[string][]int64)
	for _, member := range members {
		chashKey := string(member.Chash)
		if _, ok := keysByChash[chashKey]; !ok {
			keysByChash[chashKey] = buildReverseRelationSegmentKey(member.Chash, shard)
		}
		idxByChash[chashKey] = append(idxByChash[chashKey], member.Index)
	}

	states := make(map[string]reverseRelationSegmentState, len(keysByChash))
	err := db.View(func(txn *badger.Txn) error {
		for chashKey, segKey := range keysByChash {
			state, err := loadReverseRelationSegmentState(txn, segKey)
			if err != nil {
				return err
			}
			states[chashKey] = state
		}
		return nil
	})
	if err != nil {
		return err
	}

	for chashKey, idxs := range idxByChash {
		state := states[chashKey]
		state[fhashKey] = append(state[fhashKey], idxs...)
		encoded, err := encodeReverseRelationSegmentState(state)
		if err != nil {
			return err
		}
		if err := SetBatchNode(keysByChash[chashKey], encoded, batch); err != nil {
			return err
		}
	}

	return nil
}

func loadReverseRelationSegmentState(txn *badger.Txn, key []byte) (reverseRelationSegmentState, error) {
	item, err := txn.Get(key)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return make(reverseRelationSegmentState), nil
	}
	if err != nil {
		return nil, err
	}

	var raw []byte
	err = item.Value(func(val []byte) error {
		raw = append(raw[:0], val...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	decoded, derr := zstdx.DecodeAll(raw, nil)
	if derr == nil {
		raw = decoded
	}

	return decodeReverseRelationSegmentState(raw)
}

func encodeReverseRelationSegmentState(state reverseRelationSegmentState) ([]byte, error) {
	files := make([]string, 0, len(state))
	for fhash := range state {
		files = append(files, fhash)
	}
	sort.Slice(files, func(i, j int) bool {
		return bytes.Compare([]byte(files[i]), []byte(files[j])) < 0
	})

	buf := make([]byte, 0, 64)
	buf = append(buf, reverseRelationSegmentValueVersion)
	buf = binary.AppendUvarint(buf, uint64(len(files)))
	for _, fhash := range files {
		idxs := dedupeAndSortInt64s(state[fhash])
		fileBytes := []byte(fhash)
		buf = binary.AppendUvarint(buf, uint64(len(fileBytes)))
		buf = append(buf, fileBytes...)
		buf = binary.AppendUvarint(buf, uint64(len(idxs)))

		var prev int64
		for i, idx := range idxs {
			if idx < 0 {
				return nil, fmt.Errorf("invalid reverse relation index: %d", idx)
			}
			delta := idx
			if i > 0 {
				delta = idx - prev
			}
			if delta < 0 {
				return nil, fmt.Errorf("invalid reverse relation index delta: %d", delta)
			}
			buf = binary.AppendUvarint(buf, uint64(delta))
			prev = idx
		}
	}

	return buf, nil
}

func dedupeAndSortInt64s(values []int64) []int64 {
	if len(values) == 0 {
		return values
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := sorted[:1]
	for i := 1; i < len(sorted); i++ {
		if sorted[i] != sorted[i-1] {
			out = append(out, sorted[i])
		}
	}
	return out
}
