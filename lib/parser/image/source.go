package image

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aoiflux/libewf"

	"indicer/internal/safe"
	"indicer/lib/parser/core"
)

// ErrUnsupportedContainer is matched by every UnsupportedContainerError.
var ErrUnsupportedContainer = errors.New("image: unsupported container format")

// UnsupportedContainerError names a container DUES recognises but cannot
// decode (VMDK, QCOW2, DMG, EWF logical evidence files). Ingest stores such
// evidence as opaque bytes and skips indexing.
type UnsupportedContainerError struct{ Format string }

func (e *UnsupportedContainerError) Error() string {
	return fmt.Sprintf("image: %s containers are not supported", e.Format)
}

func (e *UnsupportedContainerError) Unwrap() error { return ErrUnsupportedContainer }

// MissingSegmentsError lists the EWF segments a set lacks.
type MissingSegmentsError = libewf.MissingSegmentsError

// ErrMissingSplitSegment is returned for a split raw set with a gap.
var ErrMissingSplitSegment = errors.New("image: split raw set has a missing segment")

// Options tune OpenSource. The zero value is ready to use.
type Options struct{}

// Source is an opened evidence image: the decoded device and what is known
// about where it came from.
type Source struct {
	Image  core.Image
	Format core.ImageFormat
	Files  []string // every file the image is read from, in order
	Meta   Meta
}

// Meta is container metadata.
type Meta struct {
	EWF *EWFMeta // nil unless Format is FormatEWF
}

// EWFMeta is what an EWF set records about its acquisition.
type EWFMeta struct {
	MD5  []byte // stored digest of the media, nil when absent
	SHA1 []byte

	// ChunkTablesInvalid counts chunk tables whose checksum failed in both
	// copies: those chunks were decoded unverified (PAR-18).
	ChunkTablesInvalid   int
	ChunkTablesRecovered int
	// AcquisitionErrors are ranges the imager could not read; they hold
	// fill bytes, not evidence.
	AcquisitionErrors []ByteRange

	CaseNumber     string
	EvidenceNumber string
	Examiner       string
	Description    string
	Notes          string
	AcquiredAt     time.Time
}

// ByteRange is a range of the decoded image.
type ByteRange struct{ Offset, Length int64 }

// OpenSource opens the evidence at path, detecting the container:
//   - EWF: the whole segment set, found by libewf's own discovery (a stray
//     "case.Exe" is not a segment, PAR-23); a gap is a MissingSegmentsError.
//     Chunk-table checksum failures are counted, not fatal.
//   - VHD/VHDX: differencing parents are resolved among siblings only.
//   - split raw (".001", ".002", ...): every segment, which must be present.
//   - VMDK, QCOW2, DMG and EWF logical files: UnsupportedContainerError.
//   - anything else: raw.
func OpenSource(ctx context.Context, path string, _ Options) (*Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("image: %s is not a regular file", path)
	}
	format, container := sniff(f, info.Size())
	if container != "" {
		_ = f.Close()
		return nil, &UnsupportedContainerError{Format: container}
	}

	switch format {
	case core.FormatEWF:
		_ = f.Close()
		return openEWF(path)
	case core.FormatVHD:
		_ = f.Close()
		img, err := safe.Value(safe.Site{Component: "parser/image", Op: "open-vhd", Path: path}, func() (core.Image, error) {
			return OpenVHDFile(path)
		})
		if err != nil {
			return nil, err
		}
		return &Source{Image: img, Format: core.FormatVHD, Files: []string{path}}, nil
	}

	if segs, ok, err := splitSegments(path); ok {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		img, err := OpenRawSegments(segs)
		if err != nil {
			return nil, err
		}
		return &Source{Image: img, Format: core.FormatRaw, Files: segs}, nil
	}
	return &Source{Image: &rawImage{ra: f, size: info.Size(), closer: f}, Format: core.FormatRaw, Files: []string{path}}, nil
}

// sniff returns the image format, or the name of a container DUES does not
// decode.
func sniff(f *os.File, size int64) (core.ImageFormat, string) {
	head := make([]byte, 64)
	n, _ := f.ReadAt(head, 0)
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("LVF\x09")):
		return core.FormatUnknown, "l01"
	case bytes.HasPrefix(head, []byte("LEF2")):
		return core.FormatUnknown, "lx01"
	case bytes.HasPrefix(head, []byte("KDMV")), bytes.HasPrefix(head, []byte("# Disk DescriptorFile")):
		return core.FormatUnknown, "vmdk"
	case bytes.HasPrefix(head, []byte("QFI\xfb")):
		return core.FormatUnknown, "qcow2"
	}
	if size >= 512 {
		tail := make([]byte, 4)
		if _, err := f.ReadAt(tail, size-512); err == nil && string(tail) == "koly" {
			return core.FormatUnknown, "dmg"
		}
	}
	return DetectFormat(f, size), ""
}

func openEWF(path string) (*Source, error) {
	segs, err := libewf.SegmentPaths(path)
	if err != nil {
		return nil, err
	}
	rd, err := safe.Value(safe.Site{Component: "parser/image", Op: "open-ewf", Path: path}, func() (libewf.Reader, error) {
		return libewf.OpenPathWithOptions(path, libewf.WithChecksumPolicy(libewf.ChecksumWarn))
	})
	if err != nil {
		return nil, err
	}
	return &Source{
		Image:  &ewfImage{r: rd},
		Format: core.FormatEWF,
		Files:  segs,
		Meta:   Meta{EWF: ewfMeta(rd)},
	}, nil
}

func ewfMeta(rd libewf.Reader) *EWFMeta {
	md := rd.Metadata()
	m := &EWFMeta{ChunkTablesInvalid: md.ChunkTablesInvalid, ChunkTablesRecovered: md.ChunkTablesRecovered}
	if md.HasMD5Digest {
		m.MD5 = append([]byte(nil), md.MD5Digest[:]...)
	}
	if md.HasSHA1Digest {
		m.SHA1 = append([]byte(nil), md.SHA1Digest[:]...)
	}
	sector := int64(rd.SectorSize())
	if sector <= 0 {
		sector = 512
	}
	for _, ae := range md.AcquisitionErrors {
		m.AcquisitionErrors = append(m.AcquisitionErrors, ByteRange{Offset: int64(ae.StartSector) * sector, Length: int64(ae.NumberOfSectors) * sector})
	}
	if a := md.Acquisition; a != nil {
		m.CaseNumber, m.EvidenceNumber, m.Examiner = a.CaseNumber, a.EvidenceNumber, a.ExaminerName
		m.Description, m.Notes, m.AcquiredAt = a.Description, a.Notes, a.AcquiryDate
	}
	return m
}

// splitSegments finds a split raw set from its first member, "name.001". ok
// is false when path is not one; a set with a gap is an error.
func splitSegments(path string) ([]string, bool, error) {
	ext := filepath.Ext(path)
	if len(ext) != 4 || ext != ".001" {
		return nil, false, nil
	}
	stem := strings.TrimSuffix(path, ext)
	segs := []string{path}
	for i := 2; i <= 999; i++ {
		p := stem + "." + fmt.Sprintf("%03d", i)
		info, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			// The set ends at the first absent number, unless a later one
			// exists: then a member is missing.
			for j := i + 1; j <= min(i+16, 999); j++ {
				if _, err := os.Stat(stem + "." + fmt.Sprintf("%03d", j)); err == nil {
					return nil, true, fmt.Errorf("%w: %s (later segment %03d exists)", ErrMissingSplitSegment, p, j)
				}
			}
			break
		}
		if err != nil {
			return nil, true, err
		}
		if !info.Mode().IsRegular() {
			return nil, true, fmt.Errorf("image: split segment %s is not a regular file", p)
		}
		segs = append(segs, p)
	}
	if len(segs) == 1 {
		return nil, false, nil // a lone .001 is an ordinary raw image
	}
	return segs, true, nil
}

// Digester computes the digests an EWF set stores, over the decoded image as
// it is read, so verification needs no second pass.
type Digester struct {
	want     *EWFMeta
	md5, sha hash.Hash
	n        int64
}

// NewDigester returns a Digester for m; with no stored digests it computes
// nothing.
func NewDigester(m Meta) *Digester {
	d := &Digester{want: m.EWF}
	if m.EWF != nil {
		if m.EWF.MD5 != nil {
			d.md5 = md5.New()
		}
		if m.EWF.SHA1 != nil {
			d.sha = sha1.New()
		}
	}
	return d
}

// Write feeds the next bytes of the image, in order.
func (d *Digester) Write(p []byte) (int, error) {
	if d.md5 != nil {
		d.md5.Write(p)
	}
	if d.sha != nil {
		d.sha.Write(p)
	}
	d.n += int64(len(p))
	return len(p), nil
}

// DigestResult is the outcome of verifying an image against its stored
// digests.
type DigestResult struct {
	Bytes                  int64
	MD5Checked, MD5Match   bool
	SHA1Checked, SHA1Match bool
}

// OK reports whether at least one stored digest was checked and none failed.
func (r DigestResult) OK() bool {
	checked := r.MD5Checked || r.SHA1Checked
	return checked && (!r.MD5Checked || r.MD5Match) && (!r.SHA1Checked || r.SHA1Match)
}

func (r DigestResult) String() string {
	var parts []string
	if r.MD5Checked {
		parts = append(parts, "md5 "+matchWord(r.MD5Match))
	}
	if r.SHA1Checked {
		parts = append(parts, "sha1 "+matchWord(r.SHA1Match))
	}
	if len(parts) == 0 {
		return "no stored digest"
	}
	return strings.Join(parts, ", ") + " over " + strconv.FormatInt(r.Bytes, 10) + " bytes"
}

func matchWord(ok bool) string {
	if ok {
		return "matches"
	}
	return "MISMATCH"
}

// Result compares what was written with the stored digests.
func (d *Digester) Result() DigestResult {
	r := DigestResult{Bytes: d.n}
	if d.md5 != nil {
		r.MD5Checked, r.MD5Match = true, bytes.Equal(d.md5.Sum(nil), d.want.MD5)
	}
	if d.sha != nil {
		r.SHA1Checked, r.SHA1Match = true, bytes.Equal(d.sha.Sum(nil), d.want.SHA1)
	}
	return r
}

// guardedReadAt reads through a container decoder, turning a panic into an
// error at the offset read.
func guardedReadAt(r io.ReaderAt, format string, p []byte, off int64) (int, error) {
	type result struct{ n int }
	out, err := safe.Value(safe.Site{Component: "parser/image", Op: "read-" + format, Offset: off}, func() (result, error) {
		n, err := r.ReadAt(p, off)
		return result{n}, err
	})
	return out.n, err
}
