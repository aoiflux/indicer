package store

import (
	"context"
	"fmt"
	"hash"
	"io"
	"sync/atomic"

	"indicer/internal/progress"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/parser/image"

	"github.com/dgraph-io/badger/v4"
)

// ChunkPass is one object to store through the chunk pipeline: an evidence,
// or a materialised object (D10). Its chunks are keyed R|||<ID>|||<offset>.
type ChunkPass struct {
	DB *badger.DB
	// ID is the raw ID the object's relations are keyed by.
	ID     []byte
	Source io.ReaderAt
	Size   int64
	// Parallel reads the source with several readers (decoded containers).
	Parallel bool
	// Ranges are ranges of the source whose plain hash the pass computes
	// as it reads (partition-table entries).
	Ranges []ByteRange
	// EWF is the set's stored metadata, whose digests the pass checks.
	EWF *image.EWFMeta
	// Quiet reports no progress.
	Quiet bool
	// RevRel also writes each chunk's reverse relation, which near needs to
	// find where else a chunk occurs.
	RevRel bool
	// Simhash also writes the SimHash signature of each chunk, which near's
	// deep mode compares.
	Simhash bool
	// MaxWorkers caps the pass's workers; 0 leaves the configured width. A
	// folder store running several evidence at once shares the machine so.
	MaxWorkers int
	// Credit places the pass in a run whose passes count new chunks as one
	// at a time would (ChunkCredits); the zero Credit counts what it stores.
	Credit Credit
}

// ChunkResult is what a chunk pass computed over exactly the bytes it stored.
type ChunkResult struct {
	Chunks    int64
	PlainHash []byte // the file hash of the whole
	Root      []byte // the hocho root over the chunk hashes, in order
	Ranges    []RangeDigest
	EWF       *image.DigestResult // nil unless EWF was given
	// NewChunks and NewBytes are the chunks this pass stored for the first
	// time and their raw size; every other chunk was already stored, by
	// this pass or before it (deduplicated).
	NewChunks, NewBytes int64
	// WrittenBytes is what the new chunks take on disk: their encoded size,
	// compressed and, in an encrypted database, sealed. In a run
	// (ChunkPass.Credit), ChunkCredits.Adjust says what to add to NewBytes
	// and WrittenBytes for what a pass at a time would have counted.
	WrittenBytes int64
}

// IngestChunks stores every chunk of p.Source, unless the database already
// holds it intact, and writes the object's relations. Chunk metadata becomes
// visible only once its bytes are durable (I1). It returns once nothing it
// started is still running (I6). A source read error is a SourceReadError.
func IngestChunks(ctx context.Context, p ChunkPass) (ChunkResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p.Size < 0 {
		return ChunkResult{}, fmt.Errorf("chunk pass: negative size %d", p.Size)
	}
	if p.Quiet {
		ctx = progress.Without(ctx)
	}
	if c := p.Credit; !c.Object {
		defer c.Ledger.evidenceEnded(c.Item)
	}
	if cnst.StorePipelineMode == cnst.StorePipelineStaged {
		return ingestChunksStaged(ctx, p)
	}
	return ingestChunksBatchOwner(ctx, p)
}

// passShape is what both pipelines derive from the database: its chunk size
// and hashes (I3), and how wide to run.
type passShape struct {
	chunk      int64
	chunkHash  func() hash.Hash
	fileHash   func() hash.Hash
	workers    int
	queueDepth int
	readers    int
	slots      int
	revrel     bool
}

func shapeFor(p ChunkPass) passShape {
	s := passShape{
		chunk:     dbmeta.ChunkSize(p.DB),
		chunkHash: dbmeta.ChunkHasher(p.DB),
		fileHash:  dbmeta.FileHasher(p.DB),
		revrel:    p.RevRel,
	}
	s.workers, s.queueDepth = cnst.GetStorePipelineTuning(cnst.CONTAINERMODE)
	if p.MaxWorkers > 0 {
		s.workers = min(s.workers, p.MaxWorkers)
	}
	s.readers = 1
	if p.Parallel {
		s.readers = s.workers
	}
	s.slots = feedSlots(s.workers, s.chunk)
	// The ring's buffers are allocated when the pass starts: a source of a
	// few chunks needs no more of them than it has chunks.
	if chunks := (p.Size + s.chunk - 1) / max(s.chunk, 1); chunks < int64(s.slots) {
		s.slots = int(max(chunks, 2))
	}
	return s
}

// PassMemory is about the most memory a chunk pass over size bytes holds at
// once with maxWorkers (0, the configured width): its feed ring, allocated
// when it starts, a chunk being encoded by each busy worker (a worker keeps
// its ring slot until it is done, so no more are busy than the ring has
// slots), and in the staged pipeline the encoded chunks queued for its
// writer.
func PassMemory(db *badger.DB, size int64, maxWorkers int) int64 {
	s := shapeFor(ChunkPass{DB: db, Size: max(size, 0), MaxWorkers: maxWorkers})
	chunks := max((max(size, 0)+s.chunk-1)/max(s.chunk, 1), 1)
	held := int64(s.slots) + min(int64(s.workers), int64(s.slots), chunks)
	if cnst.StorePipelineMode == cnst.StorePipelineStaged {
		held += min(int64(s.queueDepth), chunks)
	}
	return held * s.chunk
}

func (s passShape) feed(ctx context.Context, p ChunkPass, tap *digestTap) *chunkFeed {
	return startChunkFeed(ctx, feedOptions{src: p.Source, end: p.Size, chunk: s.chunk, readers: s.readers, slots: s.slots, tap: tap})
}

// passMeter counts a chunk pass for its progress phase ("store", in bytes
// read) and for the command's footer: bytes read, bytes new to the
// database, and the bytes written for them. It is safe from every worker;
// with no reporter it only counts what ChunkResult reports.
type passMeter struct {
	phase                         *progress.Phase
	fresh, read, freshes, written *progress.Meter
	newChunks, newBytes, onDisk   atomic.Int64
}

func startPass(ctx context.Context, p ChunkPass) *passMeter {
	if p.Quiet {
		return &passMeter{}
	}
	ph := progress.Start(ctx, "store", progress.Bytes, p.Size)
	if blobcodec.Sealed(p.DB) {
		ph.Note("encrypted")
	}
	return &passMeter{
		phase:   ph,
		fresh:   ph.Meter("new", progress.Bytes),
		read:    progress.Tally(ctx, "read", progress.Bytes),
		freshes: progress.Tally(ctx, "new", progress.Bytes),
		written: progress.Tally(ctx, "written", progress.Bytes),
	}
}

// fed counts n bytes read through the pass.
func (m *passMeter) fed(n int64) {
	m.phase.Add(n)
	m.read.Add(n)
}

// stored counts a chunk of n bytes stored for the first time, written as
// written bytes.
func (m *passMeter) stored(n, written int64) {
	m.newChunks.Add(1)
	m.newBytes.Add(n)
	m.onDisk.Add(written)
	m.fresh.Add(n)
	m.freshes.Add(n)
	m.written.Add(written)
}

func (m *passMeter) done(err error) { m.phase.Done(err) }

// passResult assembles the result once the pipeline finished cleanly.
func passResult(tap *digestTap, root []byte, chunks int64, m *passMeter) ChunkResult {
	res := ChunkResult{Chunks: chunks, PlainHash: tap.plain.Sum(nil), Root: root, Ranges: tap.rangeDigests(),
		NewChunks: m.newChunks.Load(), NewBytes: m.newBytes.Load(), WrittenBytes: m.onDisk.Load()}
	if tap.ewf != nil {
		r := tap.ewf.Result()
		res.EWF = &r
	}
	return res
}

// passErr picks the error a failed pass reports: the source failing explains
// everything after it; a cancelled caller explains a cancelled pipeline.
func passErr(parent context.Context, feedErr, pipeErr error) error {
	switch {
	case feedErr != nil:
		return feedErr
	case parent.Err() != nil:
		return parent.Err()
	}
	return pipeErr
}

// byteReaderAt reads a byte slice; a nil slice is an empty source.
type byteReaderAt []byte

func (b byteReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d", off)
	}
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
