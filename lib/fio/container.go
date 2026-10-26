package fio

/*
Container files (container mode)

Chunks are appended to container files by a single writer goroutine; worker
goroutines hand their encoded chunks to it over a buffered channel and wait
for the location it assigns.

Format v2 (STO-1/2/3). Every container is a new file, blob/ctr-<uuidv7>.blob,
created exclusively, so no session ever appends to a file another session
wrote, and the offsets a session records are offsets in the file it created.
The file starts with a 64-byte header:

	[0:8]   magic "DUESCTR2"
	[8:10]  format version (2), little endian
	[10:16] zero
	[16:32] container id (the UUID in the name)
	[32:40] creation time, Unix nanoseconds
	[40:60] zero
	[60:64] CRC-32C of bytes 0-59

Each chunk follows as one frame:

	[0:4]   magic "DCR2"
	[4]     length of the chunk hash, h
	[5:8]   zero
	[8:16]  payload length, little endian
	[16:20] CRC-32C of the payload
	[20:24] CRC-32C of bytes 0-19 and the hash
	[24:24+h] chunk hash
	payload: the encoded chunk (lib/blobcodec)

C||| metadata records the payload's offset and length, so a reader reads the
encoded chunk directly, as it did from legacy containers, and the frames
alone are enough to rebuild the chunk index (ScanContainer).

Frames are written with WriteAt at the writer's own offset, which advances
only after a complete write. A failed write truncates the container back to
the last complete frame and rotates to a new container, so a short write or
a full disk never shifts a later chunk's offset. Containers are synced when
they rotate or close, and through Sync, which the ingest's durability
barrier calls before chunk metadata becomes visible (STO-5).

Legacy containers (named from a per-manager counter, possibly compressed to
.blob.zst in quick mode) are still read, but never written.
*/

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"indicer/internal/faultinject"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	containerMagic      = "DUESCTR2"
	containerVersion    = 2
	containerHeaderSize = 64
	containerPrefix     = "ctr-"

	frameMagic      = "DCR2"
	frameHeaderSize = 24
	maxFrameHash    = 64
)

// maxContainerSize is the size at which a container is closed and a new one
// started. It is a variable so tests can force rotation.
var maxContainerSize int64 = 1 * cnst.GB

var crc32c = crc32.MakeTable(crc32.Castagnoli)

// ErrContainerCorrupt reports a container whose header or frames fail their
// checks.
var ErrContainerCorrupt = errors.New("container is corrupt")

// WriteRequest is one request to the writer goroutine: a chunk to append, or
// a sync.
type WriteRequest struct {
	hash       []byte
	data       []byte
	sync       bool // sync the open container instead of writing data
	responseCh chan WriteResponse
}

// WriteResponse is the writer goroutine's answer: where the chunk's payload
// went, or the error.
type WriteResponse struct {
	containerPath string
	offset        int64
	size          int64
	err           error
}

// ContainerManager writes chunks to container files through a single writer
// goroutine.
type ContainerManager struct {
	currentContainer string
	currentFile      *os.File
	currentOffset    int64
	dbpath           string
	writeBackend     writeBackend
	writeQueue       chan *WriteRequest
	writerDone       chan struct{}
	acceptMu         sync.RWMutex
	seenMu           sync.RWMutex
	seen             map[string]WriteResponse // in-run dedup by chunk key
	closed           bool
	closeOnce        sync.Once
}

// NewContainerManager returns a manager writing containers into the blob
// directory of the database at dbpath, and starts its writer goroutine.
func NewContainerManager(dbpath string) *ContainerManager {
	cm := &ContainerManager{
		dbpath:       dbpath,
		writeBackend: getWriteBackend(),
		writeQueue:   make(chan *WriteRequest, 1000),
		writerDone:   make(chan struct{}),
		seen:         make(map[string]WriteResponse),
	}
	go cm.writerLoop()
	return cm
}

// WriteChunkToContainer appends the chunk ckey to a container, unless this
// manager has already written it. encode returns the bytes to store (see
// lib/blobcodec); it runs in the caller's goroutine and only for chunks not
// yet written. The returned offset and size locate the encoded bytes.
func (cm *ContainerManager) WriteChunkToContainer(ckey []byte, encode func() ([]byte, error)) (containerPath string, offset int64, size int64, err error) {
	hashKey := base64.RawURLEncoding.EncodeToString(ckey)
	cm.seenMu.RLock()
	if cached, ok := cm.seen[hashKey]; ok {
		cm.seenMu.RUnlock()
		return cached.containerPath, cached.offset, cached.size, cached.err
	}
	cm.seenMu.RUnlock()

	hash := bytes.TrimPrefix(ckey, []byte(cnst.ChonkNamespace))
	if len(hash) > maxFrameHash {
		return "", 0, 0, fmt.Errorf("chunk hash of %d bytes exceeds the container limit of %d", len(hash), maxFrameHash)
	}
	data, err := encode()
	if err != nil {
		return "", 0, 0, err
	}

	resp, err := cm.submit(&WriteRequest{hash: hash, data: data})
	if err != nil {
		return "", 0, 0, err
	}
	if resp.err == nil {
		cm.seenMu.Lock()
		if _, exists := cm.seen[hashKey]; !exists {
			cm.seen[hashKey] = resp
		}
		cm.seenMu.Unlock()
	}
	return resp.containerPath, resp.offset, resp.size, resp.err
}

// Sync makes every chunk this manager has already written durable. The writer
// goroutine orders it after those writes; a container it rotated away from
// was synced when it was closed.
func (cm *ContainerManager) Sync() error {
	resp, err := cm.submit(&WriteRequest{sync: true})
	if err != nil {
		return err
	}
	return resp.err
}

// submit hands req to the writer goroutine and waits for its answer.
func (cm *ContainerManager) submit(req *WriteRequest) (WriteResponse, error) {
	req.responseCh = make(chan WriteResponse, 1)
	cm.acceptMu.RLock()
	if cm.closed {
		cm.acceptMu.RUnlock()
		return WriteResponse{}, errors.New("container manager is closed")
	}
	cm.writeQueue <- req
	cm.acceptMu.RUnlock()
	return <-req.responseCh, nil
}

// writerLoop is the writer goroutine: it alone touches the open container.
func (cm *ContainerManager) writerLoop() {
	defer close(cm.writerDone)
	for req := range cm.writeQueue {
		if req.sync {
			var err error
			if cm.currentFile != nil {
				err = cm.currentFile.Sync()
			}
			req.responseCh <- WriteResponse{err: err}
			continue
		}
		path, offset, size, err := cm.doWrite(req.hash, req.data)
		req.responseCh <- WriteResponse{containerPath: path, offset: offset, size: size, err: err}
	}
}

// doWrite appends one frame holding payload and returns where the payload
// went. Only the writer goroutine calls it.
func (cm *ContainerManager) doWrite(hash, payload []byte) (containerPath string, offset int64, size int64, err error) {
	frame := encodeFrameHeader(hash, payload)
	frameSize := int64(len(frame) + len(payload))
	if cm.currentFile != nil && cm.currentOffset+frameSize > maxContainerSize {
		if err := cm.closeCurrent(); err != nil {
			return "", 0, 0, err
		}
	}
	if cm.currentFile == nil {
		if err := cm.createNewContainer(); err != nil {
			return "", 0, 0, err
		}
	}

	start := cm.currentOffset
	if err := cm.writeAt(frame, start); err != nil {
		return "", 0, 0, cm.abandonFrame(start, err)
	}
	if err := faultinject.Hit(faultinject.ContainerMidRecord); err != nil {
		return "", 0, 0, cm.abandonFrame(start, err)
	}
	if err := cm.writeAt(payload, start+int64(len(frame))); err != nil {
		return "", 0, 0, cm.abandonFrame(start, err)
	}
	cm.currentOffset = start + frameSize
	return cm.currentContainer, start + int64(len(frame)), int64(len(payload)), nil
}

// writeAt writes all of data at off in the open container.
func (cm *ContainerManager) writeAt(data []byte, off int64) error {
	n, err := cm.writeBackend.WriteAt(cm.currentFile, data, off)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// abandonFrame handles a failed frame write at start: it cuts the container
// back to its last complete frame and closes it, so the next chunk starts a
// new container (STO-3). It returns cause, joined with any cleanup error.
func (cm *ContainerManager) abandonFrame(start int64, cause error) error {
	err := cm.currentFile.Truncate(start)
	err = errors.Join(err, cm.closeCurrent())
	if err != nil {
		return fmt.Errorf("%w (and cleaning up the container failed: %v)", cause, err)
	}
	return cause
}

// closeCurrent syncs and closes the open container.
func (cm *ContainerManager) closeCurrent() error {
	if cm.currentFile == nil {
		return nil
	}
	f := cm.currentFile
	cm.currentFile, cm.currentContainer, cm.currentOffset = nil, "", 0
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// createNewContainer creates a new container file and writes its header.
func (cm *ContainerManager) createNewContainer() error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	blobsDir := util.BlobPath(cm.dbpath)
	if err := os.MkdirAll(blobsDir, cnst.DirPerm); err != nil {
		return err
	}
	path := filepath.Join(blobsDir, containerPrefix+id.String()+cnst.BLOBEXT)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, cnst.FilePerm)
	if err != nil {
		return err
	}
	cm.currentFile, cm.currentContainer, cm.currentOffset = f, path, 0
	if err := cm.writeAt(encodeContainerHeader(id, time.Now()), 0); err != nil {
		cm.currentFile, cm.currentContainer = nil, ""
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	cm.currentOffset = containerHeaderSize
	return nil
}

// Close stops the writer goroutine once the queued writes are done, then
// syncs and closes the open container.
func (cm *ContainerManager) Close() error {
	var closeErr error
	cm.closeOnce.Do(func() {
		cm.acceptMu.Lock()
		cm.closed = true
		close(cm.writeQueue)
		cm.acceptMu.Unlock()
		<-cm.writerDone
		closeErr = cm.closeCurrent()
	})
	return closeErr
}

func encodeContainerHeader(id uuid.UUID, created time.Time) []byte {
	h := make([]byte, containerHeaderSize)
	copy(h, containerMagic)
	binary.LittleEndian.PutUint16(h[8:], containerVersion)
	copy(h[16:32], id[:])
	binary.LittleEndian.PutUint64(h[32:], uint64(created.UnixNano()))
	binary.LittleEndian.PutUint32(h[60:], crc32.Checksum(h[:60], crc32c))
	return h
}

func encodeFrameHeader(hash, payload []byte) []byte {
	h := make([]byte, frameHeaderSize+len(hash))
	copy(h, frameMagic)
	h[4] = byte(len(hash))
	binary.LittleEndian.PutUint64(h[8:], uint64(len(payload)))
	binary.LittleEndian.PutUint32(h[16:], crc32.Checksum(payload, crc32c))
	copy(h[frameHeaderSize:], hash)
	sum := crc32.Update(crc32.Checksum(h[:20], crc32c), crc32c, hash)
	binary.LittleEndian.PutUint32(h[20:], sum)
	return h
}

// ContainerFrame is one chunk found by ScanContainer: its hash and where its
// encoded bytes are.
type ContainerFrame struct {
	Hash         []byte
	Offset, Size int64
}

// ScanContainer reads the v2 container at path frame by frame, checks each
// frame, and calls fn for it. It stops at the first frame that fails a check
// with an error wrapping ErrContainerCorrupt, and returns fn's error if fn
// fails.
func ScanContainer(path string, fn func(ContainerFrame) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	header := make([]byte, containerHeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return fmt.Errorf("%w: %s: header: %v", ErrContainerCorrupt, path, err)
	}
	if string(header[:len(containerMagic)]) != containerMagic ||
		binary.LittleEndian.Uint32(header[60:]) != crc32.Checksum(header[:60], crc32c) {
		return fmt.Errorf("%w: %s: not a v2 container header", ErrContainerCorrupt, path)
	}

	fh := make([]byte, frameHeaderSize+maxFrameHash)
	var payload []byte
	for off := int64(containerHeaderSize); off < size; {
		corrupt := func(what string) error {
			return fmt.Errorf("%w: %s: frame at %d: %s", ErrContainerCorrupt, path, off, what)
		}
		if size-off < frameHeaderSize {
			return corrupt("truncated header")
		}
		if _, err := f.ReadAt(fh[:frameHeaderSize], off); err != nil {
			return err
		}
		if string(fh[:4]) != frameMagic {
			return corrupt("bad magic")
		}
		hlen := int64(fh[4])
		plen := binary.LittleEndian.Uint64(fh[8:])
		if hlen > maxFrameHash || size-off-frameHeaderSize < hlen ||
			plen > uint64(size-off-frameHeaderSize-hlen) {
			return corrupt("length past the end of the container")
		}
		if _, err := f.ReadAt(fh[frameHeaderSize:frameHeaderSize+hlen], off+frameHeaderSize); err != nil {
			return err
		}
		hash := fh[frameHeaderSize : frameHeaderSize+hlen]
		if binary.LittleEndian.Uint32(fh[20:]) != crc32.Update(crc32.Checksum(fh[:20], crc32c), crc32c, hash) {
			return corrupt("header checksum mismatch")
		}
		start := off + frameHeaderSize + hlen
		if uint64(cap(payload)) < plen {
			payload = make([]byte, plen)
		}
		payload = payload[:plen]
		if _, err := f.ReadAt(payload, start); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(fh[16:]) != crc32.Checksum(payload, crc32c) {
			return corrupt("payload checksum mismatch")
		}
		if err := fn(ContainerFrame{Hash: bytes.Clone(hash), Offset: start, Size: int64(plen)}); err != nil {
			return err
		}
		off = start + int64(plen)
	}
	return nil
}

func resolveContainerPath(containerPath string) string {
	if strings.HasSuffix(containerPath, cnst.BLOBZSTEXT) {
		return containerPath
	}
	if _, err := os.Stat(containerPath); os.IsNotExist(err) {
		compressedPath := strings.TrimSuffix(containerPath, cnst.BLOBEXT) + cnst.BLOBZSTEXT
		if _, err := os.Stat(compressedPath); err == nil {
			return compressedPath
		}
	}
	return containerPath
}

// ContainerHolds reports whether the container at containerPath exists and
// is long enough to hold size bytes at offset. A compressed legacy container
// cannot be measured without decompressing it, so its presence is enough.
func ContainerHolds(containerPath string, offset, size int64) bool {
	resolved := resolveContainerPath(containerPath)
	fi, err := os.Stat(resolved)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if strings.HasSuffix(resolved, cnst.BLOBZSTEXT) {
		return true
	}
	return offset >= 0 && size > 0 && offset <= fi.Size()-size
}

// ReadChunkFromContainer returns the stored (still encoded) bytes of the chunk
// at offset; lib/blobcodec decodes them.
func ReadChunkFromContainer(containerPath string, offset, size int64) ([]byte, error) {
	if size < 0 || offset < 0 {
		return nil, fmt.Errorf("invalid offset/size: offset=%d size=%d", offset, size)
	}

	containerPath = resolveContainerPath(containerPath)
	if strings.HasSuffix(containerPath, cnst.BLOBZSTEXT) {
		return readFromCompressedContainer(containerPath, offset, size)
	}

	file, err := os.Open(containerPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// The size comes from chunk metadata; check it against the file before
	// allocating it (STO-16).
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if offset > fi.Size() || size > fi.Size()-offset {
		return nil, fmt.Errorf("offset %d + size %d exceeds container size %d", offset, size, fi.Size())
	}
	encoded := make([]byte, size)
	if _, err = file.ReadAt(encoded, offset); err != nil {
		return nil, err
	}
	return encoded, nil
}
