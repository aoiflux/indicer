package fio

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
)

// containerChunk is one chunk written to a container and where it went.
type containerChunk struct {
	ckey, data   []byte
	path         string
	offset, size int64
}

func newContainerDB(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blob"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func chunkKey(seed int) []byte {
	return append([]byte(cnst.ChonkNamespace), bytes.Repeat([]byte{byte(seed), byte(seed >> 8)}, 16)...)
}

// writeChunks writes n chunks through cm, seeded from seed, and returns them.
func writeChunks(t testing.TB, cm *ContainerManager, seed, n int) []containerChunk {
	t.Helper()
	var out []containerChunk
	for i := range n {
		data := testutil.RandomBytes(uint64(seed+i), 1000+i)
		ckey := chunkKey(seed + i)
		path, off, size, err := cm.WriteChunkToContainer(ckey, func() ([]byte, error) { return data, nil })
		if err != nil {
			t.Fatalf("chunk %d: %v", seed+i, err)
		}
		out = append(out, containerChunk{ckey: ckey, data: data, path: path, offset: off, size: size})
	}
	return out
}

func assertChunksReadable(t *testing.T, chunks []containerChunk) {
	t.Helper()
	for i, c := range chunks {
		got, err := ReadChunkFromContainer(c.path, c.offset, c.size)
		if err != nil {
			t.Fatalf("chunk %d at %s+%d: %v", i, filepath.Base(c.path), c.offset, err)
		}
		if !bytes.Equal(got, c.data) {
			t.Fatalf("chunk %d at %s+%d reads back other bytes", i, filepath.Base(c.path), c.offset)
		}
	}
}

func closeManager(t testing.TB, cm *ContainerManager) {
	t.Helper()
	if err := cm.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestContainerSessionsNeverShareFile is the STO-1 regression. Container
// names were derived from a per-manager counter starting at 0, so every
// session appended to the same file but recorded offsets from 0, and a chunk
// from the second session read back the first session's bytes. Quick mode
// once wrote containers differently; container writing no longer depends on
// it, so one run covers both.
func TestContainerSessionsNeverShareFile(t *testing.T) {
	dir := newContainerDB(t)
	cm1 := NewContainerManager(dir)
	first := writeChunks(t, cm1, 0, 5)
	closeManager(t, cm1)

	cm2 := NewContainerManager(dir)
	second := writeChunks(t, cm2, 100, 5)
	closeManager(t, cm2)

	if first[0].path == second[0].path {
		t.Fatalf("both sessions wrote to %s", filepath.Base(first[0].path))
	}
	// STO-2: closing the second session must not remove the first
	// session's container (quick mode compressed and replaced it).
	assertChunksReadable(t, first)
	assertChunksReadable(t, second)
}

// failingBackend fails the write with index failAt after storing part of it.
type failingBackend struct {
	calls, failAt int
}

func (*failingBackend) Name() string { return "failing" }

func (b *failingBackend) WriteAt(file *os.File, data []byte, off int64) (int, error) {
	b.calls++
	if b.calls == b.failAt {
		n, _ := file.WriteAt(data[:len(data)/2], off)
		return n, testutil.ErrNoSpace()
	}
	return stdlibWriteBackend{}.WriteAt(file, data, off)
}

// TestContainerFailedWriteKeepsOffsets is the STO-3 regression. A short
// write or a full disk left the partial bytes appended without advancing the
// recorded offset, so every later chunk's offset pointed into the wrong bytes.
func TestContainerFailedWriteKeepsOffsets(t *testing.T) {
	dir := newContainerDB(t)
	cm := NewContainerManager(dir)
	backend := &failingBackend{failAt: 7} // header, two frames of two writes, then a payload
	cm.writeBackend = backend

	before := writeChunks(t, cm, 0, 2)
	_, _, _, err := cm.WriteChunkToContainer(chunkKey(50), func() ([]byte, error) {
		return testutil.RandomBytes(50, 4000), nil
	})
	if !errors.Is(err, testutil.ErrNoSpace()) {
		t.Fatalf("failed write returned %v, want the disk-full error", err)
	}
	after := writeChunks(t, cm, 200, 3)
	closeManager(t, cm)

	assertChunksReadable(t, before)
	assertChunksReadable(t, after)

	// Every container holds only whole frames: the partial frame is gone.
	files, _ := filepath.Glob(filepath.Join(dir, "blob", "ctr-*"+cnst.BLOBEXT))
	var frames int
	for _, f := range files {
		err := ScanContainer(f, func(ContainerFrame) error { frames++; return nil })
		if err != nil {
			t.Fatalf("scan %s: %v", filepath.Base(f), err)
		}
	}
	if frames != len(before)+len(after) {
		t.Fatalf("containers hold %d frames, want %d", frames, len(before)+len(after))
	}
}

// TestContainerRotation checks that a full container is closed and a new one
// started, and that chunks in both read back.
func TestContainerRotation(t *testing.T) {
	prev := maxContainerSize
	maxContainerSize = 8 << 10
	t.Cleanup(func() { maxContainerSize = prev })

	dir := newContainerDB(t)
	cm := NewContainerManager(dir)
	chunks := writeChunks(t, cm, 0, 20)
	closeManager(t, cm)

	paths := map[string]bool{}
	for _, c := range chunks {
		paths[c.path] = true
	}
	if len(paths) < 2 {
		t.Fatalf("20 chunks in %d container(s), want rotation", len(paths))
	}
	for p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > maxContainerSize {
			t.Fatalf("%s is %d bytes, over the %d limit", filepath.Base(p), fi.Size(), maxContainerSize)
		}
	}
	assertChunksReadable(t, chunks)
}

// TestScanContainer checks that the frames alone are enough to rebuild the
// chunk index, and that a damaged frame is reported, not skipped.
func TestScanContainer(t *testing.T) {
	dir := newContainerDB(t)
	cm := NewContainerManager(dir)
	chunks := writeChunks(t, cm, 0, 4)
	closeManager(t, cm)

	var got []ContainerFrame
	if err := ScanContainer(chunks[0].path, func(f ContainerFrame) error { got = append(got, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(chunks) {
		t.Fatalf("scan found %d frames, want %d", len(got), len(chunks))
	}
	for i, f := range got {
		c := chunks[i]
		if !bytes.Equal(f.Hash, c.ckey[len(cnst.ChonkNamespace):]) || f.Offset != c.offset || f.Size != c.size {
			t.Fatalf("frame %d: hash %x at %d+%d, want %x at %d+%d", i, f.Hash, f.Offset, f.Size,
				c.ckey[len(cnst.ChonkNamespace):], c.offset, c.size)
		}
	}

	// Flip one payload byte of the third chunk.
	fh, err := os.OpenFile(chunks[2].path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte{0}
	if _, err := fh.ReadAt(b, chunks[2].offset+5); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if _, err := fh.WriteAt(b, chunks[2].offset+5); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	n := 0
	err = ScanContainer(chunks[0].path, func(ContainerFrame) error { n++; return nil })
	if !errors.Is(err, ErrContainerCorrupt) || n != 2 {
		t.Fatalf("scan of a damaged container: %d frames then %v, want 2 then ErrContainerCorrupt", n, err)
	}
}

// TestContainerFileHeader checks that a new container starts with the v2
// header, and that an existing file is never appended to.
func TestContainerFileHeader(t *testing.T) {
	dir := newContainerDB(t)
	cm := NewContainerManager(dir)
	chunks := writeChunks(t, cm, 0, 1)
	closeManager(t, cm)

	raw, err := os.ReadFile(chunks[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < containerHeaderSize || string(raw[:len(containerMagic)]) != containerMagic {
		t.Fatalf("container starts with %q, want the %q header", raw[:min(len(raw), 8)], containerMagic)
	}
	if base := filepath.Base(chunks[0].path); len(base) < 4 || base[:4] != "ctr-" {
		t.Fatalf("container named %s, want ctr-<uuid>%s", base, cnst.BLOBEXT)
	}
	if chunks[0].offset < containerHeaderSize {
		t.Fatalf("chunk recorded at offset %d, inside the header", chunks[0].offset)
	}
}
