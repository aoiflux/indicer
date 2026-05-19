package fio

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// makeHash returns a deterministic 64-byte hash for testing.
func makeHash(seed byte) [64]byte {
	var h [64]byte
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// newTestBlockManager creates a BlockManager rooted at a temp directory.
func newTestBlockManager(t *testing.T) (*BlockManager, string) {
	t.Helper()
	dir := t.TempDir()
	// The BlockManager uses util.BlobPath(dbpath) which appends "/blob".
	// Create that sub-directory so flushBlock can create "blocks/" inside it.
	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0755); err != nil {
		t.Fatalf("mkdir blob: %v", err)
	}
	bm := NewBlockManager(dir, nil)
	return bm, dir
}

// TestBlockIndexRoundTrip writes a handful of chunks and reads each one back.
func TestBlockIndexRoundTrip(t *testing.T) {
	bm, _ := newTestBlockManager(t)

	// ChonkNamespaceLength prefix prepended to each hash to mimic DB key format.
	prefix := bytes.Repeat([]byte("C|||:"), 1)[:ChonkNamespaceLength]

	type entry struct {
		hash          [64]byte
		containerPath string
		offset        int64
		size          int64
	}

	entries := []entry{
		{makeHash(0x10), "/blob/c1.bin", 0, 1024},
		{makeHash(0x20), "/blob/c2.bin", 1024, 512},
		{makeHash(0x30), "/blob/c3.bin", 1536, 256},
		{makeHash(0x05), "/blob/c4.bin", 0, 4096}, // intentionally lower hash
	}

	for _, e := range entries {
		key := append(prefix, e.hash[:]...)
		if err := bm.AddChunkMetadata(key, e.containerPath, e.offset, e.size); err != nil {
			t.Fatalf("AddChunkMetadata: %v", err)
		}
	}

	if err := bm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen via a fresh BlockManager to test file-based lookup.
	bm2, _ := newTestBlockManager(t)
	// Point it at the same directory.
	bm2.dbpath = bm.dbpath

	for _, e := range entries {
		key := append(prefix, e.hash[:]...)
		gotPath, gotOffset, gotSize, err := bm2.GetChunkMetadata(key)
		if err != nil {
			t.Errorf("GetChunkMetadata seed=%x: %v", e.hash[0], err)
			continue
		}
		if gotPath != e.containerPath {
			t.Errorf("seed=%x: containerPath want %q got %q", e.hash[0], e.containerPath, gotPath)
		}
		if gotOffset != e.offset {
			t.Errorf("seed=%x: offset want %d got %d", e.hash[0], e.offset, gotOffset)
		}
		if gotSize != e.size {
			t.Errorf("seed=%x: size want %d got %d", e.hash[0], e.size, gotSize)
		}
	}
}

// TestBlockIndexMergeAcrossFlushes verifies that two separate flushes for the
// same block ID are merged into a single sorted file searchable by binary search.
func TestBlockIndexMergeAcrossFlushes(t *testing.T) {
	bm, _ := newTestBlockManager(t)
	prefix := bytes.Repeat([]byte("C|||:"), 1)[:ChonkNamespaceLength]

	// Force two separate flush cycles by adding ChunksPerBlock+1 entries with
	// the same block-ID prefix (first byte of hash = 0xAA).
	const total = ChunksPerBlock + 3
	type entry struct {
		hash [64]byte
		path string
	}
	entries := make([]entry, total)
	for i := 0; i < total; i++ {
		var h [64]byte
		h[0] = 0xAA
		h[1] = byte(i >> 8)
		h[2] = byte(i)
		for j := 3; j < 64; j++ {
			h[j] = byte(j * i)
		}
		entries[i] = entry{h, fmt.Sprintf("/blob/chunk_%04d.bin", i)}
		key := append(prefix, h[:]...)
		if err := bm.AddChunkMetadata(key, entries[i].path, int64(i*512), 512); err != nil {
			t.Fatalf("AddChunkMetadata[%d]: %v", i, err)
		}
	}

	if err := bm.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	bm2 := NewBlockManager(bm.dbpath, nil)
	for i, e := range entries {
		key := append(prefix, e.hash[:]...)
		gotPath, _, _, err := bm2.GetChunkMetadata(key)
		if err != nil {
			t.Errorf("entry[%d]: GetChunkMetadata: %v", i, err)
			continue
		}
		if gotPath != e.path {
			t.Errorf("entry[%d]: want path %q got %q", i, e.path, gotPath)
		}
	}
}

// TestBlockIndexUnknownFormatError verifies that a file with wrong magic returns
// a clear error rather than silently returning wrong data.
func TestBlockIndexUnknownFormatError(t *testing.T) {
	bm, _ := newTestBlockManager(t)
	prefix := bytes.Repeat([]byte("C|||:"), 1)[:ChonkNamespaceLength]

	var h [64]byte
	h[0] = 0xFF
	key := append(prefix, h[:]...)

	// Write an old-format (no magic) block file at the expected path.
	blockDir := filepath.Join(bm.dbpath, "blob", "blocks")
	if err := os.MkdirAll(blockDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	blockFile := filepath.Join(blockDir, "block_ff.bidx")
	if err := os.WriteFile(blockFile, make([]byte, 80), 0644); err != nil {
		t.Fatalf("write fake block: %v", err)
	}

	_, _, _, err := bm.GetChunkMetadata(key)
	if err == nil {
		t.Fatal("expected error for unknown-format block file, got nil")
	}
}
