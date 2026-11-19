package store

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"sync/atomic"

	"indicer/lib/parser/image"
)

// ErrSourceRead is matched by every SourceReadError.
var ErrSourceRead = errors.New("evidence source read failed")

// SourceReadError means the bytes being stored could not be read: the device
// went away or a sector is unreadable. The ingest fails and is not
// committed. With parallel readers, chunks past Offset may already have been
// stored; nothing refers to them until an ingest of the evidence succeeds.
type SourceReadError struct {
	Offset int64
	Err    error
}

func (e *SourceReadError) Error() string {
	return fmt.Sprintf("evidence source read failed at offset %d: %v", e.Offset, e.Err)
}

func (e *SourceReadError) Unwrap() []error { return []error{ErrSourceRead, e.Err} }

// feedChunk is one chunk of the source, read into a buffer of the feed's ring.
// Each consumer releases it once; the last release returns the buffer.
type feedChunk struct {
	off  int64
	data []byte
	buf  []byte
	refs atomic.Int32
	feed *chunkFeed
}

func (c *feedChunk) release() {
	if c.refs.Add(-1) == 0 {
		c.feed.slots <- c.buf
	}
}

// chunkFeed reads a source chunk by chunk into a fixed ring of buffers, so the
// memory an ingest holds is bounded whatever the source's size, and nothing is
// mapped: a failing device gives a read error, not a SIGBUS (ING-11). Chunks
// reach the pipeline in any order; the digest tap sees them in order, so the
// hashes it computes are of exactly the bytes stored (ING-12).
type chunkFeed struct {
	slots chan []byte
	out   chan *feedChunk
	tapCh chan *feedChunk
	tap   *digestTap

	cancel context.CancelCauseFunc
	wg     sync.WaitGroup
	tapWG  sync.WaitGroup

	mu  sync.Mutex
	err error
}

type feedOptions struct {
	src        io.ReaderAt
	start, end int64
	chunk      int64
	// readers > 1 reads chunks concurrently: decoding a container costs CPU
	// per chunk. A raw file is read by one reader, in order.
	readers int
	// slots bounds the chunks in memory at once.
	slots int
	tap   *digestTap // nil computes no digests
}

// feedSlots is the ring size for workers pipeline workers and chunk size cs:
// enough to keep every worker busy, at most about 256 MiB of buffers, and
// never fewer than two.
func feedSlots(workers int, cs int64) int {
	n := int64(4 * max(workers, 1))
	n = min(n, max(256<<20/max(cs, 1), 2))
	return int(max(n, 2))
}

func startChunkFeed(ctx context.Context, o feedOptions) *chunkFeed {
	ctx, cancel := context.WithCancelCause(ctx)
	f := &chunkFeed{
		slots:  make(chan []byte, o.slots),
		out:    make(chan *feedChunk),
		tapCh:  make(chan *feedChunk, o.slots),
		tap:    o.tap,
		cancel: cancel,
	}
	for range o.slots {
		f.slots <- make([]byte, o.chunk)
	}

	jobs := make(chan *feedChunk)
	go func() {
		defer close(jobs)
		for off := o.start; off < o.end; off += o.chunk {
			var buf []byte
			select {
			case buf = <-f.slots:
			case <-ctx.Done():
				return
			}
			c := &feedChunk{off: off, buf: buf, data: buf[:min(o.chunk, o.end-off)], feed: f}
			c.refs.Store(1)
			if f.tap != nil {
				c.refs.Store(2)
			}
			select {
			case jobs <- c:
			case <-ctx.Done():
				f.slots <- buf
				return
			}
		}
	}()

	for range max(o.readers, 1) {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			for c := range jobs {
				if err := readFullAt(o.src, c.data, c.off); err != nil {
					f.fail(err)
					f.drop(c)
					continue
				}
				if ctx.Err() != nil {
					f.drop(c)
					continue
				}
				if f.tap != nil {
					f.tapCh <- c // never blocks: it holds as many as there are slots
				}
				select {
				case f.out <- c:
				case <-ctx.Done():
					c.release()
				}
			}
		}()
	}
	go func() {
		f.wg.Wait()
		close(f.out)
		close(f.tapCh)
	}()

	if f.tap != nil {
		f.tapWG.Add(1)
		go func() {
			defer f.tapWG.Done()
			pending := map[int64]*feedChunk{}
			next := o.start
			for c := range f.tapCh {
				pending[c.off] = c
				for {
					p, ok := pending[next]
					if !ok {
						break
					}
					delete(pending, next)
					f.tap.write(p.off, p.data)
					next += int64(len(p.data))
					p.release()
				}
			}
			for _, p := range pending {
				p.release()
			}
			if next < o.end && f.firstErr() == nil && ctx.Err() == nil {
				f.fail(fmt.Errorf("digest tap stopped at %d of %d", next, o.end))
			}
		}()
	}
	return f
}

// readFullAt fills p from src at off; running out of source is an error,
// since the source's size was known before the read.
func readFullAt(src io.ReaderAt, p []byte, off int64) error {
	for n := 0; n < len(p); {
		m, err := src.ReadAt(p[n:], off+int64(n))
		n += m
		if n == len(p) {
			return nil
		}
		if err == nil && m == 0 {
			err = io.ErrNoProgress
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return &SourceReadError{Offset: off + int64(n), Err: err}
		}
	}
	return nil
}

func (f *chunkFeed) drop(c *feedChunk) {
	for c.refs.Load() > 0 {
		c.release()
	}
}

func (f *chunkFeed) fail(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
	f.cancel(err)
}

func (f *chunkFeed) firstErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// chunks delivers the source's chunks; it is closed when every chunk was
// delivered or the feed stopped.
func (f *chunkFeed) chunks() <-chan *feedChunk { return f.out }

// wait returns once the readers and the tap are done, with the first error.
// The consumer must have drained chunks first.
func (f *chunkFeed) wait() error {
	f.wg.Wait()
	f.tapWG.Wait()
	f.cancel(nil)
	return f.firstErr()
}

// ByteRange is a range of a source whose digest a chunk pass computes.
type ByteRange struct{ Offset, Length int64 }

// RangeDigest is the plain hash of one range of a source.
type RangeDigest struct {
	ByteRange
	Hash []byte
}

// digestTap hashes a source as it is stored, in order: the plain hash of the
// whole, the digest of each requested range, and the digests an EWF set
// stores (MD5, SHA-1), so none needs a second read of the evidence.
type digestTap struct {
	plain  hash.Hash
	second hash.Hash // nil: none
	ranges []rangeHasher
	ewf    *image.Digester
}

type rangeHasher struct {
	r ByteRange
	h hash.Hash
}

func newDigestTap(newHash, second func() hash.Hash, ranges []ByteRange, ewf *image.EWFMeta) *digestTap {
	t := &digestTap{plain: newHash()}
	if second != nil {
		t.second = second()
	}
	for _, r := range ranges {
		t.ranges = append(t.ranges, rangeHasher{r: r, h: newHash()})
	}
	if ewf != nil {
		t.ewf = image.NewDigester(image.Meta{EWF: ewf})
	}
	return t
}

func (t *digestTap) write(off int64, p []byte) {
	t.plain.Write(p)
	if t.second != nil {
		t.second.Write(p)
	}
	if t.ewf != nil {
		t.ewf.Write(p)
	}
	end := off + int64(len(p))
	for _, rh := range t.ranges {
		lo, hi := max(off, rh.r.Offset), min(end, rh.r.Offset+rh.r.Length)
		if lo < hi {
			rh.h.Write(p[lo-off : hi-off])
		}
	}
}

func (t *digestTap) rangeDigests() []RangeDigest {
	out := make([]RangeDigest, 0, len(t.ranges))
	for _, rh := range t.ranges {
		out = append(out, RangeDigest{ByteRange: rh.r, Hash: rh.h.Sum(nil)})
	}
	return out
}
