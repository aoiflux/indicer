package dbio

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"

	"indicer/lib/structs"
)

// ErrBadExtents means an indexed record's extents cannot describe a file: they
// do not tile its content in order, or a range overflows.
var ErrBadExtents = errors.New("bad extents")

// ExtentReader reads an indexed file's located content from its extents over
// src, the decoded evidence (D2). Sparse and unwritten extents read as zeros.
// Nothing past the located bytes is ever filled in: a read there is io.EOF.
// It is safe for concurrent use when src is.
type ExtentReader struct {
	src     io.ReaderAt
	ext     []structs.Extent
	located int64
}

// NewExtentReader returns a reader of the content ext describes. The extents
// must tile [0, located) in order, as the index pass writes them.
func NewExtentReader(src io.ReaderAt, ext []structs.Extent) (*ExtentReader, error) {
	var pos int64
	for i, e := range ext {
		switch {
		case e.FileOffset != pos:
			return nil, fmt.Errorf("%w: extent %d starts at %d, want %d", ErrBadExtents, i, e.FileOffset, pos)
		case e.Length <= 0 || e.FileOffset > math.MaxInt64-e.Length:
			return nil, fmt.Errorf("%w: extent %d has length %d", ErrBadExtents, i, e.Length)
		case e.Kind > structs.ExtentUnwritten:
			return nil, fmt.Errorf("%w: extent %d has kind %d", ErrBadExtents, i, e.Kind)
		case e.Kind == structs.ExtentData && (e.Offset < 0 || e.Offset > math.MaxInt64-e.Length):
			return nil, fmt.Errorf("%w: extent %d maps to %d+%d", ErrBadExtents, i, e.Offset, e.Length)
		}
		pos += e.Length
	}
	return &ExtentReader{src: src, ext: ext, located: pos}, nil
}

// Size returns the number of located bytes.
func (r *ExtentReader) Size() int64 { return r.located }

// ReadAt reads located content at off. A short read of a data extent from
// src is io.ErrUnexpectedEOF: the evidence ends before the extent does.
func (r *ExtentReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("extent read: negative offset %d", off)
	}
	if off >= r.located {
		return 0, io.EOF
	}
	i := sort.Search(len(r.ext), func(i int) bool { return r.ext[i].FileOffset+r.ext[i].Length > off })
	n := 0
	for ; n < len(p) && i < len(r.ext); i++ {
		e := r.ext[i]
		within := off + int64(n) - e.FileOffset
		want := min(int64(len(p)-n), e.Length-within)
		buf := p[n : n+int(want)]
		if e.Kind != structs.ExtentData {
			clear(buf)
			n += len(buf)
			continue
		}
		m, err := r.src.ReadAt(buf, e.Offset+within)
		n += m
		if m < len(buf) {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
