package store

import (
	"encoding/binary"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"
	"sync/atomic"

	"github.com/dgraph-io/badger/v4"
)

type LogicalHochoReuseStats struct {
	Attempts                uint64
	Reused                  uint64
	FallbackUnaligned       uint64
	FallbackMissingRelation uint64
	Errors                  uint64
}

var logicalHochoReuseAttempts atomic.Uint64
var logicalHochoReuseReused atomic.Uint64
var logicalHochoReuseFallbackUnaligned atomic.Uint64
var logicalHochoReuseFallbackMissingRelation atomic.Uint64
var logicalHochoReuseErrors atomic.Uint64

func SnapshotLogicalHochoReuseStats() LogicalHochoReuseStats {
	return LogicalHochoReuseStats{
		Attempts:                logicalHochoReuseAttempts.Load(),
		Reused:                  logicalHochoReuseReused.Load(),
		FallbackUnaligned:       logicalHochoReuseFallbackUnaligned.Load(),
		FallbackMissingRelation: logicalHochoReuseFallbackMissingRelation.Load(),
		Errors:                  logicalHochoReuseErrors.Load(),
	}
}

func appendHochoChunkHash(hasher interface{ Write([]byte) (int, error) }, chunkHash []byte) error {
	var lenPrefix [4]byte
	binary.BigEndian.PutUint32(lenPrefix[:], uint32(len(chunkHash)))
	if _, err := hasher.Write(lenPrefix[:]); err != nil {
		return err
	}
	if _, err := hasher.Write(chunkHash); err != nil {
		return err
	}
	return nil
}

// TryComputeAlignedLogicalHocho derives a logical-range hocho from already-ingested
// relation chunk hashes when the requested range is fully chunk-aligned.
//
// Returns (hash, true, nil) when reuse succeeds.
// Returns (nil, false, nil) when reuse is not applicable/available and callers
// should fall back to direct logical hashing.
func TryComputeAlignedLogicalHocho(fileID []byte, start, size int64, db *badger.DB) ([]byte, bool, error) {
	logicalHochoReuseAttempts.Add(1)

	if size <= 0 {
		logicalHochoReuseFallbackUnaligned.Add(1)
		return nil, false, nil
	}
	if start%cnst.ChonkSize != 0 || size%cnst.ChonkSize != 0 {
		logicalHochoReuseFallbackUnaligned.Add(1)
		return nil, false, nil
	}

	hasher := cnst.GetHashAlgo(true)
	end := start + size
	for idx := start; idx < end; idx += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, idx)
		chunkHash, err := dbio.GetNode(relKey, db)
		if err != nil {
			if err == badger.ErrKeyNotFound {
				logicalHochoReuseFallbackMissingRelation.Add(1)
				return nil, false, nil
			}
			logicalHochoReuseErrors.Add(1)
			return nil, false, err
		}

		if err := appendHochoChunkHash(hasher, chunkHash); err != nil {
			logicalHochoReuseErrors.Add(1)
			return nil, false, err
		}
	}

	logicalHochoReuseReused.Add(1)
	return hasher.Sum(nil), true, nil
}

// TryComputeLogicalHochoWithEdges derives a logical-range hocho from chunk hashes
// for fully covered chunks and byte-accurate recomputed hashes for leading/trailing
// partial chunks. Returns (nil, false, nil) when relation hashes are unavailable for
// required full chunks so callers can fall back to direct logical hashing.
func TryComputeLogicalHochoWithEdges(fileID []byte, start, size int64, mapped []byte, db *badger.DB) ([]byte, bool, error) {
	logicalHochoReuseAttempts.Add(1)

	if size <= 0 || start < 0 {
		logicalHochoReuseFallbackUnaligned.Add(1)
		return nil, false, nil
	}
	end := start + size
	if end < start || end > int64(len(mapped)) {
		logicalHochoReuseErrors.Add(1)
		return nil, false, fmt.Errorf("logical hocho range out of bounds: start=%d size=%d mapped=%d", start, size, len(mapped))
	}

	hasher := cnst.GetHashAlgo(true)
	chunkSize := cnst.ChonkSize
	firstChunkStart := (start / chunkSize) * chunkSize

	for chunkStart := firstChunkStart; chunkStart < end; chunkStart += chunkSize {
		chunkEnd := chunkStart + chunkSize
		if chunkEnd < chunkStart {
			logicalHochoReuseErrors.Add(1)
			return nil, false, fmt.Errorf("invalid chunk range overflow at %d", chunkStart)
		}

		segStart := chunkStart
		if start > segStart {
			segStart = start
		}
		segEnd := chunkEnd
		if end < segEnd {
			segEnd = end
		}
		if segStart >= segEnd {
			continue
		}

		fullChunkCovered := segStart == chunkStart && segEnd == chunkEnd
		var chunkHash []byte
		if fullChunkCovered {
			relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, chunkStart)
			storedChunkHash, err := dbio.GetNode(relKey, db)
			if err != nil {
				if err == badger.ErrKeyNotFound {
					logicalHochoReuseFallbackMissingRelation.Add(1)
					return nil, false, nil
				}
				logicalHochoReuseErrors.Add(1)
				return nil, false, err
			}
			chunkHash = storedChunkHash
		} else {
			offsetStart := int(segStart)
			offsetEnd := int(segEnd)
			if offsetStart < 0 || offsetEnd > len(mapped) || offsetStart >= offsetEnd {
				logicalHochoReuseErrors.Add(1)
				return nil, false, fmt.Errorf("invalid boundary slice for logical hocho: start=%d end=%d mapped=%d", offsetStart, offsetEnd, len(mapped))
			}
			recomputedHash, err := util.GetChonkHash(mapped[offsetStart:offsetEnd], cnst.GetHashAlgo(true))
			if err != nil {
				logicalHochoReuseErrors.Add(1)
				return nil, false, err
			}
			chunkHash = recomputedHash
		}

		if err := appendHochoChunkHash(hasher, chunkHash); err != nil {
			logicalHochoReuseErrors.Add(1)
			return nil, false, err
		}
	}

	logicalHochoReuseReused.Add(1)
	return hasher.Sum(nil), true, nil
}
