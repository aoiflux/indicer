// Package image provides pure-Go disk-image (ImageLayer) parsers — raw/dd/split,
// EWF (E01/Ex01/L01/Lx01) and VHD/VHDX — each exposed as a core.Image so the
// partition and filesystem layers can read through it uniformly.
package image

import (
	"errors"
	"io"
	"os"

	"indicer/lib/parser/core"
)

// rawImage adapts a plain io.ReaderAt (a raw / dd image, possibly built from
// split segments) to core.Image.
type rawImage struct {
	ra     io.ReaderAt
	size   int64
	closer io.Closer // non-nil when this Image owns the underlying resource
}

func (im *rawImage) ReadAt(p []byte, off int64) (int, error) { return im.ra.ReadAt(p, off) }
func (im *rawImage) Size() int64                             { return im.size }
func (im *rawImage) Format() core.ImageFormat                { return core.FormatRaw }

func (im *rawImage) Close() error {
	if im.closer != nil {
		return im.closer.Close()
	}
	return nil
}

// NewRaw wraps an already-open io.ReaderAt of known size as a raw core.Image.
// The caller retains ownership of r; Close is a no-op.
func NewRaw(r io.ReaderAt, size int64) core.Image {
	return &rawImage{ra: r, size: size}
}

// OpenRawFile opens a raw/dd image file by path. The returned Image owns the
// file handle and closes it on Close.
func OpenRawFile(path string) (core.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &rawImage{ra: f, size: info.Size(), closer: f}, nil
}

// OpenRawSegments opens split raw image segments (e.g. img.001, img.002, …) in
// the given order and presents them as one contiguous raw core.Image. The
// returned Image owns all the file handles and closes them on Close.
func OpenRawSegments(paths []string) (core.Image, error) {
	if len(paths) == 0 {
		return nil, errors.New("image: no segments provided")
	}
	if len(paths) == 1 {
		return OpenRawFile(paths[0])
	}

	files := make([]*os.File, 0, len(paths))
	segs := make([]segment, 0, len(paths))
	var total int64

	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}

	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			closeAll()
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			closeAll()
			return nil, err
		}
		files = append(files, f)
		segs = append(segs, segment{ra: f, start: total, size: info.Size()})
		total += info.Size()
	}

	return &rawImage{
		ra:     &segmentedReaderAt{segs: segs, size: total},
		size:   total,
		closer: multiCloser(files),
	}, nil
}

// segment is one member of a split image, mapped to a range of the logical device.
type segment struct {
	ra    io.ReaderAt
	start int64 // logical start offset of this segment
	size  int64
}

// segmentedReaderAt presents an ordered set of segments as one contiguous
// io.ReaderAt, transparently spanning segment boundaries.
type segmentedReaderAt struct {
	segs []segment
	size int64
}

func (s *segmentedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("image: negative offset")
	}
	total := 0
	for total < len(p) {
		pos := off + int64(total)
		if pos >= s.size {
			return total, io.EOF
		}
		seg := s.find(pos)
		local := pos - seg.start
		want := len(p) - total
		if avail := seg.size - local; int64(want) > avail {
			want = int(avail)
		}
		n, err := seg.ra.ReadAt(p[total:total+want], local)
		total += n
		if err != nil {
			// io.EOF at the end of a fully-read segment is expected; keep going
			// to the next segment. Any other error (or a short read) is fatal.
			if err == io.EOF && n == want {
				continue
			}
			return total, err
		}
	}
	return total, nil
}

// find returns the segment containing logical offset pos. pos must be < s.size.
func (s *segmentedReaderAt) find(pos int64) segment {
	for _, seg := range s.segs {
		if pos < seg.start+seg.size {
			return seg
		}
	}
	return s.segs[len(s.segs)-1]
}

// multiCloser closes several files together.
type multiCloser []*os.File

func (m multiCloser) Close() error {
	var first error
	for _, f := range m {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
