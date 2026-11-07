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
	"indicer/lib/zstdx"

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
	// SkipVerify skips the separate check of each chunk against its hash.
	// Every read is verified unless a caller turns this on (I4). The v2
	// encoding's Decode checks the hash anyway; legacy encodings do not.
	SkipVerify bool
	// CacheBytes bounds the decoded chunks a reader keeps for reuse. Zero
	// means DefaultChunkCacheBytes; a negative value keeps none.
	CacheBytes int64
	// Cache, when set, is the cache the reader keeps decoded chunks in,
	// shared with the other readers given it; CacheBytes is then ignored.
	Cache *ChunkCache
}

// DefaultChunkCacheBytes is the decoded chunk data a reader keeps by default.
const DefaultChunkCacheBytes = 64 * cnst.MB

// ChunkSpan is the part of one stored chunk that a range needs: the bytes
// chunk[Lo:Hi] of the chunk at evidence offset Chunk, which go at Offset in
// the range's own coordinates (the evidence's, for an EvidenceChunks span;
// the object's, for a Logical span).
//
// A Logical span may instead be bytes no chunk holds: Zero marks Hi-Lo bytes
// of a hole, and Inline holds content kept in its record (bytes Inline[Lo:Hi]).
// Such spans have no Hash or Loc.
type ChunkSpan struct {
	Offset int64
	Chunk  int64
	Lo, Hi int64
	Hash   []byte
	Loc    structs.ChonkMetadata
	Zero   bool
	Inline []byte
}

// Stored reports whether the span's bytes are read from a stored chunk.
func (s ChunkSpan) Stored() bool { return !s.Zero && s.Inline == nil }

// ErrContentUnavailable means a record holds no content that can be read: a
// file the index could only describe (its content could not be located or
// decoded), or a symlink.
var ErrContentUnavailable = errors.New("content unavailable")

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
	cache := opts.Cache
	if cache == nil {
		cacheBytes := opts.CacheBytes
		if cacheBytes == 0 {
			cacheBytes = DefaultChunkCacheBytes
		}
		cache = newChunkCache(cacheBytes)
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
		cache:  cache,
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
	root := KVDir(e.db)
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
	if decoded, err := zstdx.DecodeAll(chash, nil); err == nil {
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
	path := ResolveBlobPath(KVDir(e.db), s.Loc.Path)
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

// Logical reads a stored object: an evidence, a partition or an indexed file.
// An evidence, a partition, and an indexed file recorded before W6 are a byte
// range of the evidence ([Start, Start+Size)). An indexed file recorded since
// says where its bytes are (D2, D10): extents of the evidence with holes that
// read as zeros, bytes in its record, or an object stored under its own ID.
type Logical struct {
	ev        *EvidenceChunks // the evidence, or a materialised object
	key       []byte
	start     int64
	size      int64 // the bytes the reader serves
	recorded  int64 // the size the record gives
	fileHash  string
	completed bool

	content *structs.ContentRef // nil: the range [start, start+size) of ev
	extents []structs.Extent
	extentR *ExtentReader
	zeros   []byte // backs Zero spans, which are at most a chunk long
}

// OpenLogical opens the object whose record key is fid. A record whose
// content cannot be read is ErrContentUnavailable.
func OpenLogical(db *badger.DB, fid []byte, opts ReadOptions) (*Logical, error) {
	var rec structs.IndexedFile
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
		return &Logical{ev: ev, key: fid, start: rec.Start, size: rec.Size, recorded: rec.Size, fileHash: rec.FileHash, completed: rec.Completed}, nil
	case bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)):
		prec, err := GetPartitionFile(fid, db)
		if err != nil {
			return nil, err
		}
		rec = prec.IndexedFile
	case bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)):
		var err error
		if rec, err = GetIndexedFile(fid, db); err != nil {
			return nil, err
		}
	default:
		return nil, cnst.ErrUnknownFileType
	}

	rid, err := RecordEvidenceID(rec)
	if err != nil {
		return nil, err
	}
	erec, err := GetEvidenceFile(CanonicalEvidenceKey(rid), db)
	if err != nil {
		return nil, fmt.Errorf("evidence %x of %q: %w", rid, RecordPath(rec), err)
	}
	if rec.Content != nil {
		return openContent(db, fid, rid, rec, erec, opts)
	}
	ev, err := openEvidenceChunks(db, rid, erec, opts)
	if err != nil {
		return nil, err
	}
	start, size := rec.Start, rec.Size
	if start < ev.start || size < 0 || size > ev.end-start {
		return nil, fmt.Errorf("%w: %q records [%d, +%d), outside its evidence [%d, %d)", ErrChunkRange, RecordPath(rec), start, size, ev.start, ev.end)
	}
	return &Logical{ev: ev, key: fid, start: start, size: size, recorded: size, fileHash: rec.FileHash, completed: erec.Completed}, nil
}

// openContent opens an indexed file through its content reference.
func openContent(db *badger.DB, fid, rid []byte, rec structs.IndexedFile, erec structs.EvidenceFile, opts ReadOptions) (*Logical, error) {
	c := rec.Content
	path := RecordPath(rec)
	l := &Logical{key: fid, size: c.Located, recorded: rec.Size, fileHash: rec.FileHash, completed: erec.Completed, content: c}
	if c.Located < 0 || c.Located > rec.Size {
		return nil, fmt.Errorf("%w: %q locates %d of its %d bytes", ErrChunkRange, path, c.Located, rec.Size)
	}
	var err error
	switch c.Kind {
	case structs.ContentMaterialized:
		var obj structs.ObjectRecord
		if l.ev, obj, err = OpenObjectChunks(db, c.ObjectID, opts); err != nil {
			return nil, fmt.Errorf("%q: %w", path, err)
		}
		if obj.Size != c.Located {
			return nil, fmt.Errorf("%w: %q locates %d bytes, its object holds %d", ErrChunkRange, path, c.Located, obj.Size)
		}
		return l, nil
	case structs.ContentExtents, structs.ContentInline, structs.ContentNone:
		if l.ev, err = openEvidenceChunks(db, rid, erec, opts); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: %q has %s content", ErrContentUnavailable, path, c.Kind)
	}
	switch c.Kind {
	case structs.ContentNone:
		if c.Located != 0 {
			return nil, fmt.Errorf("%w: %q has no content but locates %d bytes", ErrChunkRange, path, c.Located)
		}
	case structs.ContentInline:
		if int64(len(c.Inline)) != c.Located {
			return nil, fmt.Errorf("%w: %q holds %d inline bytes and locates %d", ErrChunkRange, path, len(c.Inline), c.Located)
		}
	case structs.ContentExtents:
		if l.extents, err = recordExtents(db, fid, c); err != nil {
			return nil, fmt.Errorf("%q: %w", path, err)
		}
		if l.extentR, err = NewExtentReader(l.ev, l.extents); err != nil {
			return nil, fmt.Errorf("%q: %w", path, err)
		}
		if l.extentR.Size() != c.Located {
			return nil, fmt.Errorf("%w: %q has extents for %d bytes and locates %d", ErrBadExtents, path, l.extentR.Size(), c.Located)
		}
		for _, e := range l.extents {
			switch {
			case e.Kind != structs.ExtentData:
				if l.zeros == nil {
					l.zeros = make([]byte, l.ev.stride)
				}
			case e.Offset < l.ev.start || e.Length > l.ev.end-e.Offset:
				return nil, fmt.Errorf("%w: %q has an extent at evidence [%d, +%d), outside [%d, %d)", ErrChunkRange, path, e.Offset, e.Length, l.ev.start, l.ev.end)
			}
		}
	}
	return l, nil
}

// IndexedExtents returns the extents of the extents file whose record key and
// record are key and rec, or nil for a record of another kind.
func IndexedExtents(db *badger.DB, key []byte, rec structs.IndexedFile) ([]structs.Extent, error) {
	if rec.Content == nil || rec.Content.Kind != structs.ContentExtents {
		return nil, nil
	}
	return recordExtents(db, CanonicalIndexedKey(key), rec.Content)
}

// recordExtents returns an extents file's extents, from its record or from
// the pages a very fragmented file keeps them in.
func recordExtents(db *badger.DB, fid []byte, c *structs.ContentRef) ([]structs.Extent, error) {
	if c.ExtentPages == 0 {
		return c.Extents, nil
	}
	iid := fid[len(cnst.IdxFileNamespace):]
	ext := append([]structs.Extent(nil), c.Extents...)
	err := db.View(func(txn *badger.Txn) error {
		for p := range c.ExtentPages {
			var page []structs.Extent
			if err := GetRecordTxn(txn, ExtentPageKey(iid, p), &page); err != nil {
				return fmt.Errorf("extent page %d: %w", p, err)
			}
			ext = append(ext, page...)
		}
		return nil
	})
	return ext, err
}

// Key returns the object's record key.
func (l *Logical) Key() []byte { return l.key }

// Size returns the number of bytes the object's reader serves. It is the
// recorded size, except for a file whose content could only partly be
// located: nothing past what was located is ever filled in.
func (l *Logical) Size() int64 { return l.size }

// RecordedSize returns the size the object's record gives.
func (l *Logical) RecordedSize() int64 { return l.recorded }

// Truncated reports whether less content was located than the record's size.
func (l *Logical) Truncated() bool { return l.size < l.recorded }

// ChunkSize returns the chunk size of the stored content the object reads.
func (l *Logical) ChunkSize() int64 { return l.ev.stride }

// Evidence returns the stored chunks of the object's evidence.
func (l *Logical) Evidence() *EvidenceChunks { return l.ev }

// Completed reports whether the object's evidence finished ingesting.
func (l *Logical) Completed() bool { return l.completed }

// FileHash returns the hash the object's record carries (base64), or "".
func (l *Logical) FileHash() string { return l.fileHash }

// Spans calls fn with the spans that make up the object, in order, at most
// batch at a time, with offsets in the object's coordinates. fn may keep the
// slices it is given.
func (l *Logical) Spans(ctx context.Context, batch int, fn func([]ChunkSpan) error) error {
	if l.content == nil {
		return l.ev.Spans(ctx, l.start, l.start+l.size, batch, func(spans []ChunkSpan) error {
			for i := range spans {
				spans[i].Offset -= l.start
			}
			return fn(spans)
		})
	}
	switch l.content.Kind {
	case structs.ContentMaterialized:
		return l.ev.Spans(ctx, 0, l.size, batch, fn)
	case structs.ContentInline:
		if l.size == 0 {
			return nil
		}
		return fn([]ChunkSpan{{Offset: 0, Lo: 0, Hi: l.size, Inline: l.content.Inline}})
	case structs.ContentExtents:
		return l.extentSpans(ctx, max(batch, 1), fn)
	}
	return nil
}

// extentSpans gives an extents file's spans: the evidence's spans of each data
// extent, moved to the file's coordinates, and zero spans for holes, at most
// a chunk each, so a reader never holds more of a hole than of data.
func (l *Logical) extentSpans(ctx context.Context, batch int, fn func([]ChunkSpan) error) error {
	var pending []ChunkSpan
	add := func(s ChunkSpan) error {
		pending = append(pending, s)
		if len(pending) < batch {
			return nil
		}
		out := pending
		pending = nil
		return fn(out)
	}
	piece := l.ev.stride
	for _, e := range l.extents {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Kind != structs.ExtentData {
			for off := int64(0); off < e.Length; off += piece {
				if err := add(ChunkSpan{Offset: e.FileOffset + off, Lo: 0, Hi: min(piece, e.Length-off), Zero: true}); err != nil {
					return err
				}
			}
			continue
		}
		err := l.ev.Spans(ctx, e.Offset, e.Offset+e.Length, batch, func(spans []ChunkSpan) error {
			for _, s := range spans {
				s.Offset = e.FileOffset + (s.Offset - e.Offset)
				if err := add(s); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(pending) == 0 {
		return nil
	}
	return fn(pending)
}

// ReadSpan returns the bytes of s, which must not be modified.
func (l *Logical) ReadSpan(s ChunkSpan) ([]byte, error) {
	switch {
	case s.Zero:
		if n := s.Hi - s.Lo; n < 0 || n > int64(len(l.zeros)) {
			return nil, fmt.Errorf("%w: hole span of %d bytes", ErrChunkRange, n)
		}
		return l.zeros[:s.Hi-s.Lo], nil
	case s.Inline != nil:
		if s.Lo < 0 || s.Hi < s.Lo || s.Hi > int64(len(s.Inline)) {
			return nil, fmt.Errorf("%w: span [%d, %d) of %d inline bytes", ErrChunkRange, s.Lo, s.Hi, len(s.Inline))
		}
		return s.Inline[s.Lo:s.Hi], nil
	}
	return l.ev.ReadSpan(s)
}

// ReadAt reads the object's bytes at off.
func (l *Logical) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("%w: negative offset %d", ErrChunkRange, off)
	}
	if off >= l.size {
		return 0, io.EOF
	}
	if l.content == nil {
		return l.ev.readRange(p, l.start+off, l.start+l.size)
	}
	switch l.content.Kind {
	case structs.ContentMaterialized:
		return l.ev.readRange(p, off, l.size)
	case structs.ContentInline:
		n := copy(p, l.content.Inline[off:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	case structs.ContentExtents:
		return l.extentR.ReadAt(p, off)
	}
	return 0, io.EOF
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

// ChunkCache is a cache of decoded chunks that readers of one database can
// share (ReadOptions.Cache): a search reads the same chunk for an evidence,
// its partition and every small file in the chunk.
type ChunkCache = chunkCache

// NewChunkCache returns a cache that keeps up to budget bytes of decoded
// chunks.
func NewChunkCache(budget int64) *ChunkCache { return newChunkCache(budget) }

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
	key := chunkSizeKey{dir: KVDir(db), id: string(rid)}
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
