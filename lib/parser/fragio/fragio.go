// Package fragio presents the logical content of a fragmented file — defined by
// its absolute image byte ranges (core.AbsRange) — as one contiguous stream over
// an underlying image (io.ReaderAt).
//
// It is the foundation for two things the pure-Go stack needs beyond the
// contiguous-file path: a uniform way to read any file's content from its
// DataRuns, and (for P4e) feeding fragmented-file logical bytes to the store
// chunker, which today only reads a single contiguous mmap range. FragmentReader
// itself touches no ingest code — it is a standalone reader.
package fragio

import (
	"errors"
	"io"
	"sort"

	"indicer/lib/parser/core"
)

// FragmentReader maps logical offsets over an ordered set of absolute image byte
// ranges to physical reads on the underlying source. Sparse ranges read as
// zeros. It implements io.ReaderAt (concurrent-safe if the source is) and
// io.Reader (stateful cursor, not concurrent-safe).
type FragmentReader struct {
	src    io.ReaderAt
	frags  []core.AbsRange
	starts []int64 // logical start offset of each fragment (prefix sums)
	size   int64   // total logical size
	pos    int64   // cursor for io.Reader
}

// New builds a FragmentReader over src for the given ordered fragments.
func New(src io.ReaderAt, frags []core.AbsRange) *FragmentReader {
	starts := make([]int64, len(frags))
	var total int64
	for i, f := range frags {
		starts[i] = total
		total += f.Len()
	}
	return &FragmentReader{src: src, frags: frags, starts: starts, size: total}
}

// Size returns the total logical length of all fragments.
func (r *FragmentReader) Size() int64 { return r.size }

// ReadAt reads len(p) bytes starting at logical offset off, transparently
// spanning fragment boundaries. Sparse ranges yield zero bytes.
func (r *FragmentReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("fragio: negative offset")
	}
	total := 0
	for total < len(p) {
		loff := off + int64(total)
		if loff >= r.size {
			return total, io.EOF
		}
		i := r.fragIndex(loff)
		f := r.frags[i]
		within := loff - r.starts[i]

		want := len(p) - total
		if avail := f.Len() - within; int64(want) > avail {
			want = int(avail)
		}

		if f.Sparse {
			for j := 0; j < want; j++ {
				p[total+j] = 0
			}
			total += want
			continue
		}

		m, err := r.src.ReadAt(p[total:total+want], f.Start+within)
		total += m
		if m < want {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return total, err
		}
	}
	return total, nil
}

// Read implements io.Reader over an internal cursor.
func (r *FragmentReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	n, err := r.ReadAt(p, r.pos)
	r.pos += int64(n)
	if err == io.EOF && n > 0 {
		return n, nil // deliver bytes now; report EOF on the next call
	}
	return n, err
}

// fragIndex returns the index of the fragment containing logical offset loff
// (0 <= loff < size).
func (r *FragmentReader) fragIndex(loff int64) int {
	// smallest k with starts[k] > loff, minus one → the fragment holding loff
	k := sort.Search(len(r.starts), func(k int) bool { return r.starts[k] > loff })
	return k - 1
}
