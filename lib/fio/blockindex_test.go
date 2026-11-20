package fio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
)

// bidxHash returns a deterministic 64-byte chunk hash for i, spread across
// the 256 block files.
func bidxHash(i int) []byte {
	h := make([]byte, 64)
	for j := range h {
		h[j] = byte(i*7 + j)
	}
	h[0], h[1] = byte(i), byte(i>>8)
	return h
}

func bidxKey(hash []byte) []byte { return append([]byte(cnst.ChonkNamespace), hash...) }

// newLegacyBlockIndex writes n records into the block index of a new data
// root and returns the root and the records.
func newLegacyBlockIndex(t *testing.T, n int) (string, []testutil.LegacyBlockRecord) {
	t.Helper()
	root := t.TempDir()
	var recs []testutil.LegacyBlockRecord
	for i := range n {
		recs = append(recs, testutil.LegacyBlockRecord{
			Hash:   bidxHash(i),
			Path:   fmt.Sprintf("/data/blob/container_%d.blob", i%3),
			Offset: int64(i) * 1000,
			Size:   int64(100 + i),
		})
	}
	if err := testutil.WriteLegacyBlockIndex(filepath.Join(root, "blob"), recs); err != nil {
		t.Fatal(err)
	}
	return root, recs
}

func TestBlockIndexLookup(t *testing.T) {
	t.Parallel()
	root, recs := newLegacyBlockIndex(t, 600)
	for i, r := range recs {
		path, off, size, err := LookupBlockIndex(root, bidxKey(r.Hash))
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if path != r.Path || off != r.Offset || size != r.Size {
			t.Fatalf("record %d: %s+%d (%d), want %s+%d (%d)", i, path, off, size, r.Path, r.Offset, r.Size)
		}
	}

	missing := bidxHash(5)
	missing[63] ^= 0xff // same block file, absent hash
	if _, _, _, err := LookupBlockIndex(root, bidxKey(missing)); !errors.Is(err, ErrChunkNotIndexed) {
		t.Fatalf("absent hash: %v, want ErrChunkNotIndexed", err)
	}
	if _, _, _, err := LookupBlockIndex(t.TempDir(), bidxKey(recs[0].Hash)); !errors.Is(err, ErrChunkNotIndexed) {
		t.Fatalf("no block files: %v, want ErrChunkNotIndexed", err)
	}
}

// TestBlockIndexRejectsDamage checks that a damaged block file is an error,
// never "not indexed" (which would let ingest store the chunk again and
// restore report it missing) and never an allocation sized by the file's own
// counts (STO-16).
func TestBlockIndexRejectsDamage(t *testing.T) {
	t.Parallel()
	blockFile := func(root string, hash []byte) string {
		return filepath.Join(root, "blob", "blocks", fmt.Sprintf("block_%x.bidx", hash[:1]))
	}
	cases := map[string]func(t *testing.T, path string, raw []byte) []byte{
		"count past the end": func(t *testing.T, _ string, raw []byte) []byte {
			binary.LittleEndian.PutUint32(raw[5:9], 0xffffffff)
			return raw
		},
		"path past the string table": func(t *testing.T, _ string, raw []byte) []byte {
			for r := 0; r < int(binary.LittleEndian.Uint32(raw[5:9])); r++ {
				binary.LittleEndian.PutUint32(raw[9+r*86+80:], 0xfffffff0)
			}
			return raw
		},
		"unknown magic": func(t *testing.T, _ string, raw []byte) []byte {
			copy(raw, "XXXX")
			return raw
		},
		"truncated records": func(t *testing.T, _ string, raw []byte) []byte {
			return raw[:9+86+40]
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			root, recs := newLegacyBlockIndex(t, 600)
			target := recs[3]
			path := blockFile(root, target.Hash)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, damage(t, path, bytes.Clone(raw)), 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, _, err = LookupBlockIndex(root, bidxKey(target.Hash))
			if err == nil || errors.Is(err, ErrChunkNotIndexed) {
				t.Fatalf("lookup in a damaged block file returned %v, want a corruption error", err)
			}
		})
	}
}
