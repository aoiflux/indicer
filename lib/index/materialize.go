package index

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"

	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// sequentialSource is content that can only be read in order: a stream the
// filesystem decodes, or bytes too large to keep inline. Closer may be nil.
type sequentialSource struct {
	r io.Reader
	c io.Closer
}

func newSequentialSource(r io.Reader, c io.Closer) sequentialSource {
	return sequentialSource{r: r, c: c}
}

// sequentialReaderAt serves a chunk pass's reads of a sequential source. A
// pass with one reader reads in order; any other read is an error. It keeps
// the content's first bytes, which the file's type is detected from.
type sequentialReaderAt struct {
	mu   sync.Mutex
	r    io.Reader
	pos  int64
	head []byte
}

func (s *sequentialReaderAt) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off != s.pos {
		return 0, fmt.Errorf("decoded content read at %d, out of order (next is %d)", off, s.pos)
	}
	n, err := io.ReadFull(s.r, p)
	if len(s.head) < headLen {
		s.head = append(s.head, p[:min(n, headLen-len(s.head))]...)
	}
	s.pos += int64(n)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

// materialize stores content that is not a range of the evidence through the
// chunk pipeline, as an object keyed by the record's own ID (D10), so it is
// deduplicated like any other content. It runs inside the scan's File
// callback, the only time a decoded stream may be opened.
//
// The member key is made durable first: a pass that stops while the object
// is stored leaves a member whose missing record tells the delete to remove
// the object's relations. Content that cannot be decoded leaves the record
// metadata only, and the index partial.
func (ix *indexer) materialize(op *openPartition, iid []byte, f *structs.IndexedFile, size int64, desc string, open func() (sequentialSource, error)) error {
	mkey, mval, err := memberEntry(op, iid, nil)
	if err != nil {
		return err
	}
	if err := ix.opts.DB.Update(func(txn *badger.Txn) error { return txn.Set(mkey, mval) }); err != nil {
		return fmt.Errorf("index: %s: %w", f.Path, err)
	}
	src, err := open()
	if err != nil {
		return ix.unavailable(op, iid, f, "decoded content could not be opened")
	}
	sr := &sequentialReaderAt{r: src.r}
	res, err := store.IngestChunks(ix.ctx, store.ChunkPass{DB: ix.opts.DB, ID: iid, Source: sr, Size: size, Quiet: true})
	if src.c != nil {
		_ = src.c.Close() // a read-only stream; what was read was checked
	}
	if err != nil {
		if ix.ctx.Err() != nil {
			return context.Cause(ix.ctx)
		}
		if !errors.Is(err, store.ErrSourceRead) {
			return fmt.Errorf("index: %s: store decoded content: %w", f.Path, err)
		}
		if derr := store.DeleteObject(ix.ctx, ix.opts.DB, iid); derr != nil {
			return fmt.Errorf("index: %s: remove the partly stored content: %w", f.Path, derr)
		}
		return ix.unavailable(op, iid, f, "decoded content could not be read")
	}

	hash := base64.StdEncoding.EncodeToString(res.PlainHash)
	obj, err := dbio.EncodeRecord(structs.ObjectRecord{
		Size:        size,
		ChunkSize:   dbmeta.ChunkSize(ix.opts.DB),
		FileHash:    hash,
		EvidenceID:  ix.opts.EvidenceID,
		IndexedID:   iid,
		Description: desc,
	})
	if err != nil {
		return err
	}
	ix.cov.Materialized++
	op.rep.Materialized++
	f.Content = &structs.ContentRef{Kind: structs.ContentMaterialized, Located: size, ObjectID: iid}
	f.FileHash = hash
	f.IndexedType = util.DetectFileType(f.Path, sr.head)
	return ix.write(op, iid, f, nil, kv{dbio.ObjectKey(iid), obj})
}

// unavailable writes the record as metadata only.
func (ix *indexer) unavailable(op *openPartition, iid []byte, f *structs.IndexedFile, why string) error {
	ix.cov.Unavailable++
	ix.problem("%s", why)
	f.Content = &structs.ContentRef{Kind: structs.ContentUnavailable}
	return ix.write(op, iid, f, nil)
}
