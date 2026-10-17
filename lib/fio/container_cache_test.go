package fio

import (
	"bytes"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"

	"github.com/klauspost/compress/zstd"
)

// legacyChunk is a chunk inside a legacy compressed container.
type legacyChunk struct {
	data         []byte
	offset, size int64
}

// writeLegacyCompressedContainer writes chunks back to back, zstd-compressed,
// as quick mode used to: the metadata path names the .blob file, and only the
// .blob.zst exists.
func writeLegacyCompressedContainer(t *testing.T, dir, name string, seed uint64, n, size int) (string, []legacyChunk) {
	t.Helper()
	var raw bytes.Buffer
	var chunks []legacyChunk
	for i := range n {
		data := testutil.RandomBytes(seed+uint64(i), size)
		chunks = append(chunks, legacyChunk{data: data, offset: int64(raw.Len()), size: int64(len(data))})
		raw.Write(data)
	}
	var zbuf bytes.Buffer
	enc, err := zstd.NewWriter(&zbuf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(dir, name+cnst.BLOBEXT)
	if err := os.WriteFile(filepath.Join(dir, name+cnst.BLOBZSTEXT), zbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return blobPath, chunks
}

// withContainerCache enables the read cache with a memory budget, counts the
// container decodes, and spills into a directory the test can inspect.
func withContainerCache(t *testing.T, memBudget int64) (decodes *atomic.Int64, spillDir string) {
	t.Helper()
	spillDir = t.TempDir()
	decodes = &atomic.Int64{}
	prevOpen, prevDir := openContainerDecoder, containerSpillDir
	openContainerDecoder = func(r io.Reader) (io.ReadCloser, error) {
		decodes.Add(1)
		return prevOpen(r)
	}
	containerSpillDir = spillDir
	SetContainerReadCacheSize(memBudget)
	EnableContainerReadCache()
	t.Cleanup(func() {
		DisableContainerReadCache()
		openContainerDecoder, containerSpillDir = prevOpen, prevDir
		SetContainerReadCacheSize(defaultMaxCacheSize)
	})
	return decodes, spillDir
}

// readAllConcurrently reads every chunk from 8 goroutines in random orders.
func readAllConcurrently(t *testing.T, path string, chunks []legacyChunk) {
	t.Helper()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 1))
			for _, i := range r.Perm(len(chunks)) {
				c := chunks[i]
				got, err := ReadChunkFromContainer(path, c.offset, c.size)
				if err != nil {
					t.Errorf("chunk %d: %v", i, err)
					return
				}
				if !bytes.Equal(got, c.data) {
					t.Errorf("chunk %d reads back other bytes", i)
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestCompressedContainerDecodedOnce is the STO-12 regression. A legacy
// compressed container larger than the memory budget was decompressed in full
// on every chunk read and then thrown away, under the cache's global lock.
// It must be decoded once, spilled to a file that disappears when the cache
// is released, while every chunk still reads back.
func TestCompressedContainerDecodedOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int64
	}{{"spilled", 16 << 10}, {"in memory", 64 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			decodes, spillDir := withContainerCache(t, tc.budget)
			path, chunks := writeLegacyCompressedContainer(t, t.TempDir(), "legacy", 1, 60, 5000)

			readAllConcurrently(t, path, chunks)
			if n := decodes.Load(); n != 1 {
				t.Fatalf("container decoded %d times, want once", n)
			}

			DisableContainerReadCache()
			entries, err := os.ReadDir(spillDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("released cache left %d spill files", len(entries))
			}
			EnableContainerReadCache() // balance the cleanup's Disable
		})
	}
}

// TestCompressedContainerCacheBounded checks that at most
// maxSpilledContainers containers stay spilled, and that chunks of an evicted
// container are decoded again rather than lost.
func TestCompressedContainerCacheBounded(t *testing.T) {
	prev := maxSpilledContainers
	maxSpilledContainers = 1
	t.Cleanup(func() { maxSpilledContainers = prev })
	decodes, _ := withContainerCache(t, 1<<10)

	dir := t.TempDir()
	pa, ca := writeLegacyCompressedContainer(t, dir, "a", 10, 20, 3000)
	pb, cb := writeLegacyCompressedContainer(t, dir, "b", 50, 20, 3000)
	for range 3 {
		readAllConcurrently(t, pa, ca)
		readAllConcurrently(t, pb, cb)
	}
	if n := spilledContainers(); n > 1 {
		t.Fatalf("%d containers spilled, want at most 1", n)
	}
	if n := decodes.Load(); n < 2 || n > 6 {
		t.Fatalf("%d decodes for 6 alternating passes over 2 containers with room for 1", n)
	}
}

// TestCompressedContainerSizeCapped checks that a container decompressing past
// the limit fails the read instead of filling memory or disk.
func TestCompressedContainerSizeCapped(t *testing.T) {
	prev := maxDecompressedContainer
	maxDecompressedContainer = 50 << 10
	t.Cleanup(func() { maxDecompressedContainer = prev })

	path, chunks := writeLegacyCompressedContainer(t, t.TempDir(), "big", 1, 30, 5000)
	for _, cached := range []bool{true, false} {
		if cached {
			withContainerCache(t, 16<<10)
		} else {
			DisableContainerReadCache()
		}
		last := chunks[len(chunks)-1]
		_, err := ReadChunkFromContainer(path, last.offset, last.size)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("cached=%v: read past the limit returned %v", cached, err)
		}
	}
}

// TestCompressedContainerWithoutCache checks the uncached path, used outside
// restore, still reads every chunk.
func TestCompressedContainerWithoutCache(t *testing.T) {
	path, chunks := writeLegacyCompressedContainer(t, t.TempDir(), "plain", 3, 10, 2000)
	for i, c := range chunks {
		got, err := ReadChunkFromContainer(path, c.offset, c.size)
		if err != nil || !bytes.Equal(got, c.data) {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
}

// spilledContainers reports how many cached containers are spilled to disk.
func spilledContainers() int {
	c := &containerCache
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.entries {
		if e.file != nil {
			n++
		}
	}
	return n
}
