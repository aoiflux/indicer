package dbio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"sort"

	"github.com/dgraph-io/badger/v4"
)

const reverseRelationSegmentValueVersion byte = 1

type reverseRelationSegmentState map[string][]int64

func buildReverseRelationSegmentKey(chash []byte, shard int) []byte {
	key := make([]byte, 0, len(cnst.ReverseRelationSegmentNamespace)+len(chash)+len(cnst.DataSeperator)+1)
	key = append(key, cnst.ReverseRelationSegmentNamespace...)
	key = append(key, chash...)
	key = append(key, cnst.DataSeperator...)
	key = append(key, byte(shard))
	return key
}

func SetReverseRelationSegmentMembers(fhash []byte, members []ReverseRelationAppendMember, db *badger.DB, batch *badger.WriteBatch) error {
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

func getReverseRelationSegmentMembersAtIndex(chash []byte, index int64, db *badger.DB) (map[string]struct{}, error) {
	result := make(map[string]struct{})

	err := db.View(func(txn *badger.Txn) error {
		for shard := 0; shard < cnst.ReverseRelationAppendShardCount; shard++ {
			segKey := buildReverseRelationSegmentKey(chash, shard)
			item, err := txn.Get(segKey)
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return err
			}

			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			decoded, err := cnst.DECODER.DecodeAll(value, nil)
			if err == nil {
				value = decoded
			}

			state, err := decodeReverseRelationSegmentState(value)
			if err != nil {
				return err
			}
			for fhash, idxs := range state {
				if containsInt64(idxs, index) {
					result[fhash] = struct{}{}
				}
			}
		}
		return nil
	})

	return result, err
}

func getReverseRelationSegmentPrefixMembers(chash []byte, db *badger.DB) (map[int64][]string, error) {
	result := make(map[int64][]string)
	seen := make(map[int64]map[string]struct{})
	prefix := util.AppendToBytesSlice(cnst.ReverseRelationSegmentNamespace, chash, cnst.DataSeperator)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 64
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			decoded, err := cnst.DECODER.DecodeAll(value, nil)
			if err == nil {
				value = decoded
			}

			state, err := decodeReverseRelationSegmentState(value)
			if err != nil {
				return err
			}
			for fhash, idxs := range state {
				for _, idx := range idxs {
					if _, ok := seen[idx]; !ok {
						seen[idx] = make(map[string]struct{})
					}
					if _, ok := seen[idx][fhash]; ok {
						continue
					}
					seen[idx][fhash] = struct{}{}
					result[idx] = append(result[idx], fhash)
				}
			}
		}
		return nil
	})

	return result, err
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

	decoded, derr := cnst.DECODER.DecodeAll(raw, nil)
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

func decodeReverseRelationSegmentState(data []byte) (reverseRelationSegmentState, error) {
	state := make(reverseRelationSegmentState)
	if len(data) == 0 {
		return state, nil
	}
	if data[0] != reverseRelationSegmentValueVersion {
		return nil, fmt.Errorf("invalid reverse relation segment version: %d", data[0])
	}

	cursor := 1
	fileCount, n := binary.Uvarint(data[cursor:])
	if n <= 0 {
		return nil, fmt.Errorf("invalid reverse relation segment file count")
	}
	cursor += n

	for i := uint64(0); i < fileCount; i++ {
		fileLen, n := binary.Uvarint(data[cursor:])
		if n <= 0 {
			return nil, fmt.Errorf("invalid reverse relation segment file length")
		}
		cursor += n
		if cursor+int(fileLen) > len(data) {
			return nil, fmt.Errorf("reverse relation segment file length out of range")
		}
		fhash := string(data[cursor : cursor+int(fileLen)])
		cursor += int(fileLen)

		idxCount, n := binary.Uvarint(data[cursor:])
		if n <= 0 {
			return nil, fmt.Errorf("invalid reverse relation segment index count")
		}
		cursor += n

		idxs := make([]int64, 0, idxCount)
		var prev int64
		for j := uint64(0); j < idxCount; j++ {
			delta, n := binary.Uvarint(data[cursor:])
			if n <= 0 {
				return nil, fmt.Errorf("invalid reverse relation segment index delta")
			}
			cursor += n

			idx := int64(delta)
			if j > 0 {
				idx = prev + int64(delta)
			}
			idxs = append(idxs, idx)
			prev = idx
		}
		state[fhash] = idxs
	}

	if cursor != len(data) {
		return nil, fmt.Errorf("invalid reverse relation segment trailing bytes: %d", len(data)-cursor)
	}

	return state, nil
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

func containsInt64(values []int64, target int64) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
