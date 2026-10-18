package store

import (
	"encoding/binary"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// legacyHochoWithEdges computes the "hocho with edges" hash ingests before W6
// gave partitions and indexed files under the hocho strategy: the file hash
// over, for each chunk the range touches, the chunk's own hash when the range
// covers it whole, or else the hash of the covered piece, each length first.
// Databases written then keep such hashes, and restore must still verify
// them; the tests build their records with it.
func legacyHochoWithEdges(fileID []byte, start, size int64, content []byte, db *badger.DB) ([]byte, error) {
	end := start + size
	if size <= 0 || start < 0 || end > int64(len(content)) {
		return nil, fmt.Errorf("range %d+%d outside %d bytes", start, size, len(content))
	}
	hasher := cnst.GetHashAlgo(true)
	entry := func(h []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(h)))
		hasher.Write(n[:])
		hasher.Write(h)
	}
	cs := cnst.ChonkSize
	for chunk := start / cs * cs; chunk < end; chunk += cs {
		lo, hi := max(start, chunk), min(end, chunk+cs)
		if lo == chunk && hi == chunk+cs {
			h, err := dbio.GetNode(util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, chunk), db)
			if err != nil {
				return nil, err
			}
			entry(h)
			continue
		}
		h, err := util.GetChonkHash(content[lo:hi], cnst.GetHashAlgo(true))
		if err != nil {
			return nil, err
		}
		entry(h)
	}
	return hasher.Sum(nil), nil
}
