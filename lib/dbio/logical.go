package dbio

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// ErrRelationMissing reports an evidence offset with no relation (R|||) key,
// so which chunk is stored there is unknown.
var ErrRelationMissing = errors.New("relation missing")

// ChunkCorruptError reports a stored chunk whose bytes do not decode to the
// content its hash names: damaged, substituted, or read from the wrong
// place. It matches blobcodec.ErrCorrupt.
type ChunkCorruptError struct {
	EvidenceID []byte // the raw ID the evidence's relations are keyed by
	Offset     int64  // the chunk's offset in the evidence
	Hash       []byte // the chunk hash its relation records
	Path       string // the blob or container holding the chunk
	Err        error
}

func (e *ChunkCorruptError) Error() string {
	return fmt.Sprintf("chunk %x at offset %d of evidence %x (%s): %v", e.Hash, e.Offset, e.EvidenceID, e.Path, e.Err)
}

func (e *ChunkCorruptError) Unwrap() error { return e.Err }

// ReadOptions control how the readers in this file read chunks.
type ReadOptions struct {
	// SkipVerify reads chunks without checking them against their hash.
	// Every read is verified unless a caller turns this on (I4).
	SkipVerify bool
	// CacheBytes bounds the decoded chunks a reader keeps for reuse. Zero
	// means DefaultChunkCacheBytes; a negative value keeps none.
	CacheBytes int64
}

// DefaultChunkCacheBytes is the decoded chunk data a reader keeps by default.
const DefaultChunkCacheBytes = 64 * cnst.MB

// ChunkSpan is the part of one stored chunk that a range needs: the bytes
// chunk[Lo:Hi] of the chunk at evidence offset Chunk, which go at Offset in
// the range's own coordinates (the evidence's, for an EvidenceChunks span;
// the object's, for a Logical span).
type ChunkSpan struct {
	Offset int64
	Chunk  int64
	Lo, Hi int64
	Hash   []byte
	Loc    structs.ChonkMetadata
}

// Len is the number of bytes the span covers.
func (s ChunkSpan) Len() int64 { return s.Hi - s.Lo }

// SpanBatch is how many chunks a reader looks up per read transaction for
// chunks of size chunkSize: enough to amortise the transaction, few enough
// that a batch's decoded chunks stay near 256 MiB.
func SpanBatch(chunkSize int64) int {
	return int(min(128, max(4, 256*cnst.MB/max(chunkSize, 1))))
}

// EvidenceChunks reads an evidence through its stored chunks: relations
// (R|||) give each chunk's hash, the locator its place, and each chunk read
// is decoded and checked against its hash. It is safe for concurrent use.
type EvidenceChunks struct {
	db     *badger.DB
	id     []byte
	start  int64
	end    int64
	stride int64
	codec  blobcodec.Codec
	verify bool
	cache  *chunkCache
}

// OpenEvidenceChunks opens the stored chunks of the evidence eid, given as
// its record key (E|||…) or the raw ID its relations are keyed by.
func OpenEvidenceChunks(db *badger.DB, eid []byte, opts ReadOptions) (*EvidenceChunks, error) {
	rid := EvidenceRawID(eid)
	rec, err := GetEvidenceFile(CanonicalEvidenceKey(rid), db)
	if err != nil {
		return nil, fmt.Errorf("evidence %x: %w", rid, err)
	}
	return openEvidenceChunks(db, rid, rec, opts)
}

func openEvidenceChunks(db *badger.DB, rid []byte, rec structs.EvidenceFile, opts ReadOptions) (*EvidenceChunks, error) {
	if rec.Start < 0 || rec.Size < 0 || rec.Size > int64(^uint64(0)>>1)-rec.Start {
		return nil, fmt.Errorf("%w: evidence %x records start %d, size %d", ErrChunkRange, rid, rec.Start, rec.Size)
	}
	stride, err := EvidenceChunkSize(db, rid, rec.Start+rec.Size)
	if err != nil {
		return nil, err
	}
	codec := blobcodec.For(db)
	cacheBytes := opts.CacheBytes
	if cacheBytes == 0 {
		cacheBytes = DefaultChunkCacheBytes
	}
	return &EvidenceChunks{
		db:     db,
		id:     append([]byte(nil), rid...),
		start:  rec.Start,
		end:    rec.Start + rec.Size,
		stride: stride,
		codec:  codec,
		// The v2 codec checks every chunk it decodes against its hash.
		verify: !opts.SkipVerify && codec.Name() != blobcodec.V2,
		cache:  newChunkCache(cacheBytes),
	}, nil
}

// ID returns the raw ID the evidence's relations are keyed by.
func (e *EvidenceChunks) ID() []byte { return e.id }

// Size returns the evidence's size.
func (e *EvidenceChunks) Size() int64 { return e.end - e.start }

// ChunkSize returns the chunk size the evidence was stored with.
func (e *EvidenceChunks) ChunkSize() int64 { return e.stride }

// ChunkLen is the length of the chunk at evidence offset off: every chunk is
// the evidence's chunk size, except a shorter last one.
func (e *EvidenceChunks) ChunkLen(off int64) int64 { return min(e.stride, e.end-off) }

// Spans calls fn with the spans that make up the evidence bytes [start, end),
// in order, at most batch at a time; each batch's relations and chunk
// locations are read in one transaction. fn may keep the slices it is given.
// A range outside the evidence is an error wrapping ErrChunkRange.
func (e *EvidenceChunks) Spans(ctx context.Context, start, end int64, batch int, fn func([]ChunkSpan) error) error {
	if start < e.start || end < start || end > e.end {
		return fmt.Errorf("%w: [%d, %d) is outside evidence %x [%d, %d)", ErrChunkRange, start, end, e.id, e.start, e.end)
	}
	batch = max(batch, 1)
	root := e.db.Opts().Dir
	off := start / e.stride * e.stride
	for off < end {
		if err := ctx.Err(); err != nil {
			return err
		}
		spans := make([]ChunkSpan, 0, batch)
		err := e.db.View(func(txn *badger.Txn) error {
			for ; off < end && len(spans) < batch; off += e.stride {
				chash, err := e.relationTxn(txn, off)
				if err != nil {
					return err
				}
				loc, err := locateChunkTxn(txn, util.AppendToBytesSlice(cnst.ChonkNamespace, chash), root)
				if err != nil {
					return fmt.Errorf("chunk %x at offset %d of evidence %x: %w", chash, off, e.id, err)
				}
				lo, hi, err := Intersect(off, e.ChunkLen(off), start, end)
				if err != nil {
					return err
				}
				spans = append(spans, ChunkSpan{Offset: off + lo, Chunk: off, Lo: lo, Hi: hi, Hash: chash, Loc: loc})
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := fn(spans); err != nil {
			return err
		}
	}
	return nil
}

func (e *EvidenceChunks) relationTxn(txn *badger.Txn, off int64) ([]byte, error) {
	item, err := txn.Get(relationKey(e.id, off))
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, fmt.Errorf("%w: evidence %x has none at offset %d", ErrRelationMissing, e.id, off)
	}
	if err != nil {
		return nil, fmt.Errorf("relation at offset %d of evidence %x: %w", off, e.id, err)
	}
	chash, err := item.ValueCopy(nil)
	if err != nil {
		return nil, fmt.Errorf("relation at offset %d of evidence %x: %w", off, e.id, err)
	}
	// Relation values are zstd-compressed outside quick mode.
	if decoded, err := cnst.DECODER.DecodeAll(chash, nil); err == nil {
		chash = decoded
	}
	return chash, nil
}

func relationKey(rid []byte, off int64) []byte {
	key := make([]byte, 0, len(cnst.RelationNamespace)+len(rid)+len(cnst.DataSeperator)+20)
	key = append(key, cnst.RelationNamespace...)
	key = append(key, rid...)
	key = append(key, cnst.DataSeperator...)
	return strconv.AppendInt(key, off, 10)
}

// ReadSpan returns the bytes of s. They may be shared with other reads and
// must not be modified.
func (e *EvidenceChunks) ReadSpan(s ChunkSpan) ([]byte, error) {
	data, err := e.chunk(s)
	if err != nil {
		return nil, err
	}
	// The chunk's hash checked its content; its length checks that it is
	// the chunk the evidence's layout puts here.
	if want := e.ChunkLen(s.Chunk); int64(len(data)) != want {
		return nil, fmt.Errorf("%w: chunk %x at offset %d of evidence %x holds %d bytes; in %d-byte chunks the evidence needs %d there", ErrChunkRange, s.Hash, s.Chunk, e.id, len(data), e.stride, want)
	}
	if s.Lo < 0 || s.Hi < s.Lo || s.Hi > int64(len(data)) {
		return nil, fmt.Errorf("%w: span [%d, %d) of chunk at offset %d", ErrChunkRange, s.Lo, s.Hi, s.Chunk)
	}
	return data[s.Lo:s.Hi], nil
}

// chunk returns the whole decoded, checked content of the chunk s is part of.
func (e *EvidenceChunks) chunk(s ChunkSpan) ([]byte, error) {
	if data, ok := e.cache.get(s.Hash); ok {
		return data, nil
	}
	path := ResolveBlobPath(e.db.Opts().Dir, s.Loc.Path)
	var stored []byte
	var err error
	if s.Loc.Container {
		stored, err = fio.ReadChunkFromContainer(path, s.Loc.Offset, s.Loc.EncodedSize)
	} else {
		stored, err = fio.ReadChonk(path)
	}
	if err != nil {
		return nil, fmt.Errorf("chunk %x at offset %d of evidence %x: %w", s.Hash, s.Chunk, e.id, err)
	}
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, s.Hash)
	plain, err := e.codec.Decode(ckey, stored)
	if err == nil && e.verify {
		err = blobcodec.Verify(ckey, plain)
	}
	if errors.Is(err, blobcodec.ErrCorrupt) {
		return nil, &ChunkCorruptError{EvidenceID: e.id, Offset: s.Chunk, Hash: s.Hash, Path: path, Err: err}
	}
	if err != nil {
		return nil, fmt.Errorf("chunk %x at offset %d of evidence %x: %w", s.Hash, s.Chunk, e.id, err)
	}
	e.cache.put(s.Hash, plain)
	return plain, nil
}

// readRange fills p from the evidence bytes starting at off, reading no
// further than limit, as io.ReaderAt does.
func (e *EvidenceChunks) readRange(p []byte, off, limit int64) (int, error) {
	if off < e.start {
		return 0, fmt.Errorf("%w: read at %d is before evidence %x starts at %d", ErrChunkRange, off, e.id, e.start)
	}
	if off >= limit {
		return 0, io.EOF
	}
	end := off + min(int64(len(p)), limit-off)
	err := e.Spans(context.Background(), off, end, SpanBatch(e.stride), func(spans []ChunkSpan) error {
		for _, s := range spans {
			data, err := e.ReadSpan(s)
			if err != nil {
				return err
			}
			copy(p[s.Offset-off:], data)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n := int(end - off); n < len(p) {
		return n, io.EOF
	}
	return len(p), nil
}

// ReadAt reads the evidence bytes at off (an offset in the evidence).
func (e *EvidenceChunks) ReadAt(p []byte, off int64) (int, error) {
	return e.readRange(p, off, e.end)
}

// Logical reads a stored object, an evidence, a partition or an indexed
// file, as a byte range of its evidence. Partitions and indexed files
// recorded before extents exist are the single range [Start, Start+Size).
type Logical struct {
	ev        *EvidenceChunks
	key       []byte
	start     int64
	size      int64
	fileHash  string
	completed bool
}

// OpenLogical opens the object whose record key is fid.
func OpenLogical(db *badger.DB, fid []byte, opts ReadOptions) (*Logical, error) {
	var start, size int64
	var fileHash, name string
	switch {
	case bytes.HasPrefix(fid, []byte(cnst.EviFileNamespace)):
		rid := EvidenceRawID(fid)
		rec, err := GetEvidenceFile(fid, db)
		if err != nil {
			return nil, err
		}
		ev, err := openEvidenceChunks(db, rid, rec, opts)
		if err != nil {
			return nil, err
		}
		return &Logical{ev: ev, key: fid, start: rec.Start, size: rec.Size, fileHash: rec.FileHash, completed: rec.Completed}, nil
	case bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)):
		rec, err := GetPartitionFile(fid, db)
		if err != nil {
			return nil, err
		}
		start, size, fileHash, name = rec.Start, rec.Size, rec.FileHash, rec.Name
	case bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)):
		rec, err := GetIndexedFile(fid, db)
		if err != nil {
			return nil, err
		}
		start, size, fileHash, name = rec.Start, rec.Size, rec.FileHash, rec.Name
	default:
		return nil, cnst.ErrUnknownFileType
	}

	rid, err := util.GetEvidenceFileHash(name)
	if err != nil {
		return nil, err
	}
	erec, err := GetEvidenceFile(CanonicalEvidenceKey(rid), db)
	if err != nil {
		return nil, fmt.Errorf("evidence %x of %q: %w", rid, name, err)
	}
	ev, err := openEvidenceChunks(db, rid, erec, opts)
	if err != nil {
		return nil, err
	}
	if start < ev.start || size < 0 || size > ev.end-start {
		return nil, fmt.Errorf("%w: %q records [%d, +%d), outside its evidence [%d, %d)", ErrChunkRange, name, start, size, ev.start, ev.end)
	}
	return &Logical{ev: ev, key: fid, start: start, size: size, fileHash: fileHash, completed: erec.Completed}, nil
}

// Key returns the object's record key.
func (l *Logical) Key() []byte { return l.key }

// Size returns the object's size.
func (l *Logical) Size() int64 { return l.size }

// Evidence returns the stored chunks of the object's evidence.
func (l *Logical) Evidence() *EvidenceChunks { return l.ev }

// Completed reports whether the object's evidence finished ingesting.
func (l *Logical) Completed() bool { return l.completed }

// FileHash returns the hash the object's record carries (base64), or "".
func (l *Logical) FileHash() string { return l.fileHash }

// Spans calls fn with the spans that make up the object, in order, at most
// batch at a time, with offsets in the object's coordinates.
func (l *Logical) Spans(ctx context.Context, batch int, fn func([]ChunkSpan) error) error {
	return l.ev.Spans(ctx, l.start, l.start+l.size, batch, func(spans []ChunkSpan) error {
		for i := range spans {
			spans[i].Offset -= l.start
		}
		return fn(spans)
	})
}

// ReadSpan returns the bytes of s, which must not be modified.
func (l *Logical) ReadSpan(s ChunkSpan) ([]byte, error) { return l.ev.ReadSpan(s) }

// ReadAt reads the object's bytes at off.
func (l *Logical) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("%w: negative offset %d", ErrChunkRange, off)
	}
	if off >= l.size {
		return 0, io.EOF
	}
	return l.ev.readRange(p, l.start+off, l.start+l.size)
}

// Stream calls fn with the object's bytes in order, a chunk's part at a
// time. The bytes must not be modified.
func (l *Logical) Stream(ctx context.Context, fn func([]byte) error) error {
	return l.Spans(ctx, SpanBatch(l.ev.stride), func(spans []ChunkSpan) error {
		for _, s := range spans {
			data, err := l.ReadSpan(s)
			if err != nil {
				return err
			}
			if err := fn(data); err != nil {
				return err
			}
		}
		return nil
	})
}

// chunkCache keeps recently decoded chunks, keyed by chunk hash, within a
// byte budget. Content is immutable for a hash, so entries never go stale.
type chunkCache struct {
	mu      sync.Mutex
	budget  int64
	used    int64
	order   *list.List // front is most recent; values are *cacheEntry
	entries map[string]*list.Element
}

type cacheEntry struct {
	hash string
	data []byte
}

func newChunkCache(budget int64) *chunkCache {
	return &chunkCache{budget: budget, order: list.New(), entries: map[string]*list.Element{}}
}

func (c *chunkCache) get(hash []byte) ([]byte, bool) {
	if c.budget <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[string(hash)]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*cacheEntry).data, true
}

func (c *chunkCache) put(hash, data []byte) {
	if int64(len(data)) > c.budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[string(hash)]; ok {
		return
	}
	for c.used+int64(len(data)) > c.budget {
		oldest := c.order.Back()
		ent := oldest.Value.(*cacheEntry)
		c.order.Remove(oldest)
		delete(c.entries, ent.hash)
		c.used -= int64(len(ent.data))
	}
	c.entries[string(hash)] = c.order.PushFront(&cacheEntry{hash: string(hash), data: data})
	c.used += int64(len(data))
}

// The chunk size of each legacy evidence, once inferred, keyed by database
// directory and evidence ID. Bounded: cleared when it grows past
// maxCachedChunkSizes.
var (
	chunkSizes      sync.Map
	chunkSizesCount atomic.Int64
)

const maxCachedChunkSizes = 4096

// chunkSizeSampleKeys is how many relation offsets inferChunkSize samples.
const chunkSizeSampleKeys = 64

type chunkSizeKey struct{ dir, id string }

// EvidenceChunkSize returns the chunk size the evidence rid, which ends at
// evidence offset end, was stored with (RST-2, I3). In a database created
// with a manifest every evidence uses the manifest's chunk size. In a legacy
// database the size is the setting of the run that stored the evidence, so
// it is read from the evidence's own relation keys.
func EvidenceChunkSize(db *badger.DB, rid []byte, end int64) (int64, error) {
	cs := dbmeta.ChunkSize(db)
	if dbmeta.UniformChunkSize(db) {
		return cs, nil
	}
	key := chunkSizeKey{dir: db.Opts().Dir, id: string(rid)}
	if v, ok := chunkSizes.Load(key); ok {
		return v.(int64), nil
	}
	var s int64
	err := db.View(func(txn *badger.Txn) error {
		var err error
		s, err = inferChunkSize(txn, rid, end, cs)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("chunk size of evidence %x: %w", rid, err)
	}
	if chunkSizesCount.Add(1) > maxCachedChunkSizes {
		chunkSizes.Clear()
		chunkSizesCount.Store(1)
	}
	chunkSizes.Store(key, s)
	return s, nil
}

// inferChunkSize finds the chunk size of the evidence rid from its relation
// keys. Chunks sit at the multiples of the chunk size s below end, so every
// relation offset is a multiple of s, and s itself is the smallest positive
// offset there is a relation at. Given any positive offset o with a
// relation, s is therefore the smallest divisor of o that has a relation
// too: a smaller divisor is not a multiple of s. hint, the database's chunk
// size, is tried first as o.
func inferChunkSize(txn *badger.Txn, rid []byte, end, hint int64) (int64, error) {
	has := func(off int64) (bool, error) {
		_, err := txn.Get(relationKey(rid, off))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return false, nil
		}
		return err == nil, err
	}

	var o int64
	if hint > 0 && hint < end {
		ok, err := has(hint)
		if err != nil {
			return 0, err
		}
		if ok {
			o = hint
		}
	}
	if o == 0 {
		// A short key-only scan: the gcd of a few offsets is a small
		// multiple of s, which keeps the divisor search short.
		prefix := relationKey(rid, 0)
		prefix = prefix[:len(prefix)-1]
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		n := 0
		for it.Rewind(); it.Valid() && n < chunkSizeSampleKeys; it.Next() {
			off, err := strconv.ParseInt(string(it.Item().Key()[len(prefix):]), 10, 64)
			if err != nil || off <= 0 {
				continue
			}
			o = gcdInt64(o, off)
			n++
		}
		it.Close()
	}
	if o == 0 {
		// One chunk at most: any size from end up keeps it whole.
		return max(hint, end), nil
	}

	var small, large []int64
	for d := int64(1); d <= o/d; d++ {
		if o%d == 0 {
			small = append(small, d)
			if d != o/d {
				large = append(large, o/d)
			}
		}
	}
	for i := len(large) - 1; i >= 0; i-- {
		small = append(small, large[i])
	}
	for _, d := range small {
		ok, err := has(d)
		if err != nil {
			return 0, err
		}
		if ok {
			return d, nil
		}
	}
	return o, nil
}

func gcdInt64(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
