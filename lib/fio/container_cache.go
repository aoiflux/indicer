package fio

// Read cache for legacy compressed containers (STO-12).
//
// Quick mode used to compress each finished container into one zstd stream,
// <name>.blob.zst, which cannot be read at an offset without decoding
// everything before it. While a restore runs, the cache decodes each such
// container once, keeping it in memory when it fits the budget and otherwise
// spilling it to a temporary file the file system deletes when the cache
// lets go of it, even if the process dies, because the file holds
// decompressed evidence. Containers written today are never compressed.

import (
	"bytes"
	"fmt"
	"indicer/internal/osdirs"
	"indicer/lib/cnst"
	"indicer/lib/fsx"
	"io"
	"os"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const defaultMaxCacheSize = 2 * cnst.GB

var (
	// maxDecompressedContainer bounds a legacy container's decompressed size.
	// Quick mode closed a container once it reached 1 GB, so anything larger
	// is damage or a decompression bomb.
	maxDecompressedContainer int64 = 1 * cnst.GB

	// maxSpilledContainers bounds how many containers stay spilled to disk.
	maxSpilledContainers = 4

	// containerSpillDir is where spill files go (SetContainerSpillDir);
	// empty means osdirs.Temp. containerCache.mu guards it.
	containerSpillDir = ""

	// openContainerDecoder starts decoding a compressed container. It is a
	// variable so tests can count decodes.
	openContainerDecoder = func(r io.Reader) (io.ReadCloser, error) {
		d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(256<<20))
		if err != nil {
			return nil, fmt.Errorf("failed to create decompressor: %w", err)
		}
		return d.IOReadCloser(), nil
	}
)

// cachedContainer is one decoded container: its bytes in mem, or in file.
type cachedContainer struct {
	ready chan struct{} // closed once the fields below are set
	err   error
	mem   []byte
	file  *os.File
	size  int64

	// Guarded by containerCache.mu.
	lastUse time.Time
	refs    int
	evicted bool
}

type containerCacheState struct {
	mu      sync.Mutex
	users   int
	memMax  int64
	memUsed int64
	entries map[string]*cachedContainer
	loading chan struct{} // one decode at a time bounds the memory in flight
}

var containerCache = containerCacheState{
	memMax:  defaultMaxCacheSize,
	entries: map[string]*cachedContainer{},
	loading: make(chan struct{}, 1),
}

// EnableContainerReadCache turns the cache on for the caller until its
// matching DisableContainerReadCache. Calls nest, so concurrent restores
// share it.
func EnableContainerReadCache() {
	c := &containerCache
	c.mu.Lock()
	c.users++
	c.mu.Unlock()
}

// SetContainerReadCacheSize sets the memory budget for decoded containers.
// Containers that do not fit are spilled to disk.
func SetContainerReadCacheSize(sizeBytes int64) {
	c := &containerCache
	c.mu.Lock()
	c.memMax = sizeBytes
	c.mu.Unlock()
}

// SetContainerSpillDir sets the directory for containers that do not fit
// in memory (--temp-dir); empty means osdirs.Temp. The cache is shared by
// every restore in the process, so the directory is a process setting.
func SetContainerSpillDir(dir string) {
	c := &containerCache
	c.mu.Lock()
	containerSpillDir = dir
	c.mu.Unlock()
}

// DisableContainerReadCache ends one EnableContainerReadCache. When the last
// user is gone every decoded container is released, and spill files are
// deleted as soon as no read is using them.
func DisableContainerReadCache() {
	c := &containerCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.users == 0 {
		return
	}
	c.users--
	if c.users > 0 {
		return
	}
	for path, e := range c.entries {
		c.evictLocked(path, e)
	}
}

func readFromCompressedContainer(containerPath string, offset, size int64) ([]byte, error) {
	e := acquireContainer(containerPath)
	if e == nil {
		return streamReadCompressedChunk(containerPath, offset, size)
	}
	defer releaseContainer(e)
	<-e.ready
	if e.err != nil {
		return nil, e.err
	}
	if offset > e.size || size > e.size-offset {
		return nil, fmt.Errorf("offset %d + size %d exceeds decompressed container size %d", offset, size, e.size)
	}
	if e.mem != nil {
		return bytes.Clone(e.mem[offset : offset+size]), nil
	}
	encoded := make([]byte, size)
	if _, err := e.file.ReadAt(encoded, offset); err != nil {
		return nil, err
	}
	return encoded, nil
}

// acquireContainer returns the cache entry for containerPath with a
// reference held, decoding the container first if no other read has, or nil
// when the cache is off.
func acquireContainer(containerPath string) *cachedContainer {
	c := &containerCache
	c.mu.Lock()
	if c.users == 0 {
		c.mu.Unlock()
		return nil
	}
	e, ok := c.entries[containerPath]
	if !ok {
		e = &cachedContainer{ready: make(chan struct{})}
		c.entries[containerPath] = e
	}
	e.refs++
	e.lastUse = time.Now()
	c.mu.Unlock()
	if !ok {
		loadContainer(containerPath, e)
	}
	return e
}

// releaseContainer drops a reference taken by acquireContainer.
func releaseContainer(e *cachedContainer) {
	c := &containerCache
	c.mu.Lock()
	defer c.mu.Unlock()
	e.refs--
	if e.refs == 0 && e.evicted {
		e.drop()
	}
}

// loadContainer decodes the container into e and makes it ready. Memory is
// used only while it fits in what is left of the budget.
func loadContainer(containerPath string, e *cachedContainer) {
	c := &containerCache
	c.loading <- struct{}{}
	c.mu.Lock()
	room := max(0, c.memMax-c.memUsed)
	spillDir := containerSpillDir
	c.mu.Unlock()
	mem, file, size, err := decodeContainer(containerPath, room, spillDir)
	<-c.loading

	c.mu.Lock()
	e.mem, e.file, e.size, e.err = mem, file, size, err
	if c.entries[containerPath] == e {
		if err != nil {
			// A failed decode is not cached; the next read tries again.
			c.evictLocked(containerPath, e)
		} else {
			if mem != nil {
				c.memUsed += size
			}
			c.trimSpilledLocked(e)
		}
	}
	c.mu.Unlock()
	close(e.ready)
}

// trimSpilledLocked evicts the least recently used spilled containers, other
// than keep, until at most maxSpilledContainers remain.
func (c *containerCacheState) trimSpilledLocked(keep *cachedContainer) {
	for {
		var spilled int
		var oldestPath string
		var oldest *cachedContainer
		for path, e := range c.entries {
			if e.file == nil {
				continue
			}
			spilled++
			if e != keep && (oldest == nil || e.lastUse.Before(oldest.lastUse)) {
				oldestPath, oldest = path, e
			}
		}
		if spilled <= maxSpilledContainers || oldest == nil {
			return
		}
		c.evictLocked(oldestPath, oldest)
	}
}

// evictLocked removes e from the cache; its memory or spill file goes once
// no read holds it.
func (c *containerCacheState) evictLocked(path string, e *cachedContainer) {
	delete(c.entries, path)
	if e.mem != nil {
		c.memUsed -= e.size
	}
	e.evicted = true
	if e.refs == 0 {
		e.drop()
	}
}

func (e *cachedContainer) drop() {
	if e.file != nil {
		_ = e.file.Close()
	}
	e.mem, e.file = nil, nil
}

// decodeContainer decodes the compressed container at path, into memory when
// it fits in room bytes and into a delete-on-close spill file in spillDir
// (osdirs.Temp when empty) otherwise.
func decodeContainer(path string, room int64, spillDir string) (mem []byte, spill *os.File, size int64, err error) {
	src, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer src.Close()
	dec, err := openContainerDecoder(src)
	if err != nil {
		return nil, nil, 0, err
	}
	defer dec.Close()

	limit := maxDecompressedContainer
	tooBig := fmt.Errorf("decompressed container %s exceeds the %d-byte limit", path, limit)
	var buf bytes.Buffer
	n, err := io.CopyN(&buf, dec, min(room, limit)+1)
	if err == io.EOF {
		return buf.Bytes(), nil, n, nil
	}
	if err != nil {
		return nil, nil, 0, fmt.Errorf("failed to decompress container: %w", err)
	}
	if n > limit {
		return nil, nil, 0, tooBig
	}

	if spillDir, err = osdirs.TempOr(spillDir); err != nil {
		return nil, nil, 0, err
	}
	spill, err = fsx.CreateTempDeleteOnClose(spillDir, "dues-container-*.tmp")
	if err != nil {
		return nil, nil, 0, err
	}
	m, err := buf.WriteTo(spill)
	if err == nil {
		var rest int64
		rest, err = io.Copy(spill, io.LimitReader(dec, limit-m+1))
		m += rest
	}
	if err == nil && m > limit {
		err = tooBig
	}
	if err != nil {
		_ = spill.Close()
		return nil, nil, 0, fmt.Errorf("failed to decompress container: %w", err)
	}
	return nil, spill, m, nil
}

// streamReadCompressedChunk reads one chunk by decoding the container up to
// it. It serves reads outside a restore, where no cache is active.
func streamReadCompressedChunk(containerPath string, offset, size int64) ([]byte, error) {
	if offset > maxDecompressedContainer || size > maxDecompressedContainer-offset {
		return nil, fmt.Errorf("offset %d + size %d exceeds the %d-byte limit for a decompressed container", offset, size, maxDecompressedContainer)
	}
	file, err := os.Open(containerPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder, err := openContainerDecoder(file)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	if _, err = io.CopyN(io.Discard, decoder, offset); err != nil {
		if err == io.EOF {
			return nil, fmt.Errorf("offset %d exceeds decompressed container size", offset)
		}
		return nil, err
	}
	encoded := make([]byte, size)
	if _, err = io.ReadFull(decoder, encoded); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("offset %d + size %d exceeds decompressed container size", offset, size)
		}
		return nil, err
	}
	return encoded, nil
}
