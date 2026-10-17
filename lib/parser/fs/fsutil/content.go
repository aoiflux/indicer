package fsutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"indicer/lib/parser/core"
)

// ErrNotLocated is returned by a content reader asked for bytes past what the
// content located: they are unknown, not zero.
var ErrNotLocated = errors.New("fsutil: content not located")

// OpenContent streams c's logical bytes. Data extents are read from r, the
// partition reader, at ImageOffset-base; holes and unwritten extents read as
// zeros; inline content is served from memory. Reading past Located returns
// ErrNotLocated, so a truncated file never looks complete.
func OpenContent(r io.ReaderAt, base int64, c core.Content) (io.ReadCloser, error) {
	switch c.Kind {
	case core.ContentNone:
		return io.NopCloser(bytes.NewReader(nil)), nil
	case core.ContentInline:
		return io.NopCloser(&locatedReader{r: bytes.NewReader(c.Inline), located: c.Located, size: c.Size}), nil
	case core.ContentExtents:
		er := &extentReaderAt{r: r, base: base, ext: c.Extents}
		return io.NopCloser(&locatedReader{r: io.NewSectionReader(er, 0, c.Located), located: c.Located, size: c.Size}), nil
	}
	return nil, fmt.Errorf("fsutil: %s content cannot be read from extents", c.Kind)
}

// locatedReader ends with ErrNotLocated, not io.EOF, when the located bytes
// stop short of the size.
type locatedReader struct {
	r             io.Reader
	located, size int64
}

func (l *locatedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err == io.EOF && l.located < l.size {
		return n, fmt.Errorf("%w: %d of %d bytes", ErrNotLocated, l.located, l.size)
	}
	return n, err
}

type extentReaderAt struct {
	r    io.ReaderAt
	base int64
	ext  []core.Extent
}

func (e *extentReaderAt) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	for total < len(p) {
		pos := off + int64(total)
		i := e.find(pos)
		if i < 0 {
			return total, io.EOF
		}
		x := e.ext[i]
		within := pos - x.FileOffset
		want := min(int64(len(p)-total), x.Length-within)
		buf := p[total : total+int(want)]
		if x.Kind != core.ExtentData {
			clear(buf)
		} else {
			n, err := e.r.ReadAt(buf, x.ImageOffset-e.base+within)
			if n < len(buf) {
				if err == nil || err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return total + n, err
			}
		}
		total += int(want)
	}
	return total, nil
}

// find returns the extent holding logical offset pos, or -1.
func (e *extentReaderAt) find(pos int64) int {
	lo, hi := 0, len(e.ext)
	for lo < hi {
		m := (lo + hi) / 2
		if e.ext[m].FileEnd() <= pos {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(e.ext) && e.ext[lo].FileOffset <= pos {
		return lo
	}
	return -1
}
