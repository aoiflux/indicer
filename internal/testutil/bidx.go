package testutil

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// LegacyBlockRecord is one chunk location in a legacy hierarchical block
// index.
type LegacyBlockRecord struct {
	Hash         []byte // the chunk hash, at most 64 bytes
	Path         string // the container
	Offset, Size int64
}

// WriteLegacyBlockIndex writes recs as the block index of the blob directory
// blobDir, in the v2 format DUES wrote until D9 retired hierarchical mode:
// one file per first hash byte, blocks/block_<hex>.bidx, holding a header
// ("BIDX", version 2, record count), the records sorted by hash (64-byte
// hash, offset, size, path offset, path length), then the path strings.
// Existing block files are replaced.
func WriteLegacyBlockIndex(blobDir string, recs []LegacyBlockRecord) error {
	buckets := map[string][]LegacyBlockRecord{}
	for _, r := range recs {
		if len(r.Hash) == 0 || len(r.Hash) > 64 {
			return fmt.Errorf("hash of %d bytes", len(r.Hash))
		}
		id := fmt.Sprintf("%x", r.Hash[:1])
		buckets[id] = append(buckets[id], r)
	}
	dir := filepath.Join(blobDir, "blocks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for id, rs := range buckets {
		sort.Slice(rs, func(i, j int) bool { return bytes.Compare(pad64(rs[i].Hash), pad64(rs[j].Hash)) < 0 })
		var b, table bytes.Buffer
		b.WriteString("BIDX")
		b.WriteByte(2)
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(rs)))
		for _, r := range rs {
			b.Write(pad64(r.Hash))
			_ = binary.Write(&b, binary.LittleEndian, r.Offset)
			_ = binary.Write(&b, binary.LittleEndian, r.Size)
			_ = binary.Write(&b, binary.LittleEndian, uint32(table.Len()))
			_ = binary.Write(&b, binary.LittleEndian, uint16(len(r.Path)))
			table.WriteString(r.Path)
		}
		b.Write(table.Bytes())
		if err := os.WriteFile(filepath.Join(dir, "block_"+id+".bidx"), b.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func pad64(h []byte) []byte {
	p := make([]byte, 64)
	copy(p, h)
	return p
}
