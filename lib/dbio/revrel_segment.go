package dbio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"indicer/lib/cnst"

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

func containsInt64(values []int64, target int64) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
