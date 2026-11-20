package ntfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/aoiflux/libntfs"
)

// DUES decodes NTFS-compressed streams itself, from the attribute's runs, so
// the same decoder serves deleted files, whose records libntfs will not open.
// It was written when libntfs (up to v0.3.5) took the compression-unit field
// as a cluster count instead of a power of two and failed on every unit
// (PAR-27); TestDecoderMatchesLibntfs checks that the two decoders agree on
// live files now.

var errLZNT1 = errors.New("ntfs: corrupt LZNT1 data")

// maxUnitBytes bounds one compression unit. NTFS compresses only volumes with
// clusters up to 4 KiB in 16-cluster units, so a real unit is at most 64 KiB.
const maxUnitBytes = 1 << 20

// lznt1 decompresses src into dst and returns the bytes written. src is a
// sequence of chunks, each with a 16-bit header: bit 15 set means compressed,
// the low 12 bits are the chunk's length minus one. A zero header ends it.
func lznt1(dst, src []byte) (int, error) {
	out := 0
	for pos := 0; pos+2 <= len(src); {
		hdr := binary.LittleEndian.Uint16(src[pos:])
		pos += 2
		if hdr == 0 {
			break
		}
		size := int(hdr&0x0fff) + 1
		if pos+size > len(src) {
			return out, fmt.Errorf("%w: chunk of %d bytes past the end of the unit", errLZNT1, size)
		}
		chunk := src[pos : pos+size]
		pos += size
		if hdr&0x8000 == 0 {
			if out+len(chunk) > len(dst) {
				return out, fmt.Errorf("%w: stored chunk overruns the unit", errLZNT1)
			}
			out += copy(dst[out:], chunk)
			continue
		}
		start := out
		for i := 0; i < len(chunk); {
			flags := chunk[i]
			i++
			for bit := 0; bit < 8 && i < len(chunk); bit++ {
				if flags&1 == 0 {
					if out >= len(dst) {
						return out, fmt.Errorf("%w: literal overruns the unit", errLZNT1)
					}
					dst[out] = chunk[i]
					out++
					i++
				} else {
					if i+2 > len(chunk) {
						return out, fmt.Errorf("%w: truncated back-reference", errLZNT1)
					}
					tok := int(binary.LittleEndian.Uint16(chunk[i:]))
					i += 2
					// The split between offset and length bits moves with the
					// position inside the chunk: more offset bits further in.
					shift := 0
					for p := out - start - 1; p >= 0x10; p >>= 1 {
						shift++
					}
					offset := (tok >> (12 - shift)) + 1
					length := (tok & (0x0fff >> shift)) + 3
					from := out - offset
					if from < start || out+length > len(dst) {
						return out, fmt.Errorf("%w: back-reference %d+%d at %d", errLZNT1, offset, length, out-start)
					}
					for k := 0; k < length; k++ { // overlapping copies are the point
						dst[out] = dst[from+k]
						out++
					}
				}
				flags >>= 1
			}
		}
	}
	return out, nil
}

// compressedStream reads an NTFS-compressed non-resident attribute: its
// clusters are grouped in compression units, and a unit is stored raw (every
// cluster allocated), absent (every cluster sparse: zeros), or compressed
// (the allocated clusters hold LZNT1 data, the rest are sparse).
type compressedStream struct {
	r        io.ReaderAt // the partition
	cs       int64       // cluster size
	unitCl   int64       // clusters per unit
	size     int64       // logical size
	valid    int64       // initialized size: bytes past it read as zeros
	runs     []libntfs.DataRun
	runStart []int64 // first VCN of each run

	cached int64 // unit held in buf, or -1
	buf    []byte
	raw    []byte
}

func newCompressedStream(r io.ReaderAt, clusterSize uint32, attr *libntfs.NonResidentAttribute) (*compressedStream, error) {
	if attr.CompressionUnit == 0 || attr.CompressionUnit > 16 {
		return nil, fmt.Errorf("ntfs: compression unit 2^%d clusters", attr.CompressionUnit)
	}
	cs := int64(clusterSize)
	unitCl := int64(1) << attr.CompressionUnit
	if cs <= 0 || unitCl*cs > maxUnitBytes {
		return nil, fmt.Errorf("ntfs: compression unit of %d clusters of %d bytes", unitCl, cs)
	}
	s := &compressedStream{
		r: r, cs: cs, unitCl: unitCl,
		size: int64(attr.RealSize), valid: int64(attr.InitializedSize),
		runs: attr.DataRuns, cached: -1,
		buf: make([]byte, unitCl*cs),
	}
	vcn := int64(attr.StartingVCN)
	for _, run := range s.runs {
		s.runStart = append(s.runStart, vcn)
		vcn += int64(run.LengthClusters)
	}
	return s, nil
}

// lcn maps a VCN to its cluster on the volume; ok is false for a sparse or
// unmapped cluster.
func (s *compressedStream) lcn(vcn int64) (int64, bool) {
	i := sort.Search(len(s.runStart), func(i int) bool { return s.runStart[i] > vcn }) - 1
	if i < 0 {
		return 0, false
	}
	run := s.runs[i]
	if vcn-s.runStart[i] >= int64(run.LengthClusters) || run.IsSparse {
		return 0, false
	}
	return run.StartCluster + (vcn - s.runStart[i]), true
}

func (s *compressedStream) unit(u int64) error {
	if s.cached == u {
		return nil
	}
	s.cached = -1
	s.raw = s.raw[:0]
	first := u * s.unitCl
	allocated := int64(0)
	for c := int64(0); c < s.unitCl; c++ {
		lcn, ok := s.lcn(first + c)
		if !ok {
			continue
		}
		allocated++
		at := len(s.raw)
		s.raw = append(s.raw, make([]byte, s.cs)...)
		if n, err := s.r.ReadAt(s.raw[at:], lcn*s.cs); n < int(s.cs) {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("ntfs: read cluster %d: %w", lcn, err)
		}
	}
	switch allocated {
	case 0:
		clear(s.buf)
	case s.unitCl:
		copy(s.buf, s.raw)
	default:
		n, err := lznt1(s.buf, s.raw)
		if err != nil {
			return fmt.Errorf("compression unit %d: %w", u, err)
		}
		clear(s.buf[n:])
	}
	s.cached = u
	return nil
}

func (s *compressedStream) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("ntfs: negative offset")
	}
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		if pos >= s.size {
			return done, io.EOF
		}
		n := min(int64(len(p)-done), s.size-pos)
		if pos >= s.valid {
			clear(p[done : done+int(n)])
			done += int(n)
			continue
		}
		n = min(n, s.valid-pos)
		unitBytes := s.unitCl * s.cs
		u := pos / unitBytes
		if err := s.unit(u); err != nil {
			return done, err
		}
		within := pos - u*unitBytes
		n = min(n, unitBytes-within)
		copy(p[done:done+int(n)], s.buf[within:within+n])
		done += int(n)
	}
	return done, nil
}
