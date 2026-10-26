package store

import (
	"context"
	"fmt"
	"hash"
	"io"

	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/parser/image"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
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
	// Quiet shows no progress bar.
	Quiet bool
}

// ChunkResult is what a chunk pass computed over exactly the bytes it stored.
type ChunkResult struct {
	Chunks    int64
	PlainHash []byte // the file hash of the whole
	Root      []byte // the hocho root over the chunk hashes, in order
	Ranges    []RangeDigest
	EWF       *image.DigestResult // nil unless EWF was given
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
}

func shapeFor(p ChunkPass) passShape {
	s := passShape{
		chunk:     dbmeta.ChunkSize(p.DB),
		chunkHash: dbmeta.ChunkHasher(p.DB),
		fileHash:  dbmeta.FileHasher(p.DB),
	}
	s.workers, s.queueDepth = cnst.GetStorePipelineTuning(cnst.CONTAINERMODE)
	s.readers = 1
	if p.Parallel {
		s.readers = s.workers
	}
	s.slots = feedSlots(s.workers, s.chunk)
	return s
}

func (s passShape) feed(ctx context.Context, p ChunkPass, tap *digestTap) *chunkFeed {
	return startChunkFeed(ctx, feedOptions{src: p.Source, end: p.Size, chunk: s.chunk, readers: s.readers, slots: s.slots, tap: tap})
}

func newPassBar(p ChunkPass) *progressbar.ProgressBar {
	if p.Quiet {
		return progressbar.DefaultSilent(p.Size)
	}
	return progressbar.NewOptions64(
		p.Size,
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWriter(console.Progress()),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)
}

func finishPassBar(p ChunkPass, bar *progressbar.ProgressBar) {
	bar.Finish()
	if !p.Quiet {
		fmt.Fprintln(console.Err())
	}
	// A progress bar error (an empty input has nothing to show) is not the
	// operation's error.
	_ = bar.Close()
}

// passResult assembles the result once the pipeline finished cleanly.
func passResult(tap *digestTap, root []byte, chunks int64) ChunkResult {
	res := ChunkResult{Chunks: chunks, PlainHash: tap.plain.Sum(nil), Root: root, Ranges: tap.rangeDigests()}
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
