package index

import (
	"encoding/binary"

	"github.com/zeebo/blake3"
)

// Record IDs are derived from what identifies a record, so indexing the same
// evidence again (reindex, a resumed ingest) writes the same keys instead of
// adding copies (I7, ING-6).

// IDLen is the length of a partition or indexed-file ID.
const IDLen = 16

// PartitionID is blake3-128 of the evidence ID, the partition's ordinal in
// its table (-1 for a whole-device filesystem) and its offset.
func PartitionID(eid []byte, ordinal int, offset int64) []byte {
	h := blake3.New()
	h.Write([]byte("dues-pid1"))
	writeBytes(h, eid)
	writeInt(h, int64(ordinal))
	writeInt(h, offset)
	return h.Sum(nil)[:IDLen]
}

// IndexedID is blake3-128 of the evidence ID, the partition's ordinal and
// offset, and the node's inode, path and deleted flag; collision counts the
// earlier records of the same partition that hashed to the same ID (two
// deleted entries with one name and inode), so every record keeps its own.
func IndexedID(eid []byte, ordinal int, partOffset int64, inode uint64, path string, deleted bool, collision uint32) []byte {
	h := blake3.New()
	h.Write([]byte("dues-iid1"))
	writeBytes(h, eid)
	writeInt(h, int64(ordinal))
	writeInt(h, partOffset)
	writeInt(h, int64(inode))
	writeBytes(h, []byte(path))
	if deleted {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	writeInt(h, int64(collision))
	return h.Sum(nil)[:IDLen]
}

func writeInt(h *blake3.Hasher, v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	h.Write(b[:])
}

func writeBytes(h *blake3.Hasher, b []byte) {
	writeInt(h, int64(len(b)))
	h.Write(b)
}
