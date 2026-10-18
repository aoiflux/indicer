// Package evidence opens what an ingest stores. A disk image in a container
// DUES decodes (EWF, VHD/VHDX, a split raw set) is opened as the decoded
// device, so the stored evidence, its hashes and its index all refer to the
// same bytes (D5, ING-7); anything else is stored as the file's own bytes.
package evidence

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
	"indicer/lib/structs"
)

// Options tune Open.
type Options struct {
	// Decode stores a recognised container decoded (D5). Without it every
	// file is stored as its own bytes: the compression product archives
	// files, and must give back exactly the file it was given.
	Decode bool
}

// Index skip reasons a Source can carry.
const (
	SkipUnsupportedContainer = "unsupported_container"
	SkipNotDecoded           = "container_not_decoded"
	SkipEmpty                = "empty"
)

// Source is an evidence opened for ingest. It is safe for concurrent reads.
type Source struct {
	img       core.Image
	format    string // structs.Source*
	container string
	members   []structs.SourceMember
	ewf       *image.EWFMeta
	skip      string
	name      string
	path      string
}

// Open opens the evidence at path. A container that fails to decode (a
// missing segment or parent, a damaged header) is an error, never a fallback
// to its raw bytes: those bytes are not the evidence the examiner means.
func Open(ctx context.Context, path string, opts Options) (*Source, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		// Opening a FIFO blocks, and a device has no fixed content.
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	s := &Source{name: filepath.Base(abs), path: abs}
	if !opts.Decode {
		if err := s.openPlain(abs, info.Size()); err != nil {
			return nil, err
		}
		c, err := image.Container(abs)
		if err != nil {
			_ = s.img.Close()
			return nil, err
		}
		if c != "" {
			s.container, s.skip = c, SkipNotDecoded
		}
		return s.finish(), nil
	}

	src, err := image.OpenSource(ctx, abs, image.Options{})
	var unsupported *image.UnsupportedContainerError
	switch {
	case errors.As(err, &unsupported):
		if err := s.openPlain(abs, info.Size()); err != nil {
			return nil, err
		}
		s.format, s.container, s.skip = structs.SourceOpaque, unsupported.Format, SkipUnsupportedContainer
		return s.finish(), nil
	case err != nil:
		return nil, err
	}
	s.img = src.Image
	s.ewf = src.Meta.EWF
	switch src.Format {
	case core.FormatEWF:
		s.format, s.container = structs.SourceEWF, "ewf"
	case core.FormatVHD:
		s.format, s.container = structs.SourceVHD, vhdKind(abs)
	default:
		s.format = structs.SourceRaw
	}
	for _, f := range src.Files {
		m := structs.SourceMember{Path: f}
		if fi, err := os.Stat(f); err == nil {
			m.Size = fi.Size()
		}
		s.members = append(s.members, m)
	}
	return s.finish(), nil
}

func (s *Source) openPlain(path string, size int64) error {
	img, err := image.OpenRawFile(path)
	if err != nil {
		return err
	}
	s.img, s.format = img, structs.SourceRaw
	s.members = []structs.SourceMember{{Path: path, Size: size}}
	return nil
}

func (s *Source) finish() *Source {
	if s.skip == "" && s.img.Size() == 0 {
		s.skip = SkipEmpty
	}
	return s
}

func vhdKind(path string) string {
	switch ext := filepath.Ext(path); ext {
	case ".vhdx", ".VHDX", ".avhdx", ".AVHDX":
		return "vhdx"
	}
	return "vhd"
}

// ReadAt reads the stored bytes at off.
func (s *Source) ReadAt(p []byte, off int64) (int, error) { return s.img.ReadAt(p, off) }

// Size is the number of bytes the ingest stores.
func (s *Source) Size() int64 { return s.img.Size() }

// Image is the stored bytes as a device to scan.
func (s *Source) Image() core.Image { return s.img }

// Name is the evidence's display name, the base name of its path.
func (s *Source) Name() string { return s.name }

// Path is the absolute path the evidence was opened from.
func (s *Source) Path() string { return s.path }

// Info is the source as an evidence record keeps it.
func (s *Source) Info() structs.EvidenceSource {
	info := structs.EvidenceSource{Format: s.format, Container: s.container, Members: append([]structs.SourceMember(nil), s.members...)}
	if s.ewf != nil {
		info.EWF = &structs.EWFCheck{
			StoredMD5:          s.ewf.MD5,
			StoredSHA1:         s.ewf.SHA1,
			ChunkTablesInvalid: s.ewf.ChunkTablesInvalid,
		}
		for _, r := range s.ewf.AcquisitionErrors {
			info.EWF.AcquisitionErrors = append(info.EWF.AcquisitionErrors, structs.ByteRange{Offset: r.Offset, Length: r.Length})
		}
	}
	return info
}

// EWF returns what an EWF set records, or nil.
func (s *Source) EWF() *image.EWFMeta { return s.ewf }

// SkipIndex says why the source cannot be indexed, or "".
func (s *Source) SkipIndex() string { return s.skip }

// Parallel reports whether concurrent reads help: decoding a container costs
// CPU per chunk, while a raw file is fastest read in order.
func (s *Source) Parallel() bool {
	return s.format == structs.SourceEWF || s.format == structs.SourceVHD
}

// Close releases the source's files.
func (s *Source) Close() error { return s.img.Close() }

// FingerprintFullLimit is the size up to which a fingerprint covers the whole
// content, and so identifies it exactly.
const FingerprintFullLimit = 4 << 20

// fingerprintSample is the length of each sample a larger fingerprint reads.
const fingerprintSample = 1 << 20

// Fingerprint is a cheap identity of the stored bytes: a hash of the size and
// the whole content up to FingerprintFullLimit, or of three samples (head,
// middle, tail) beyond. Equal content always has equal fingerprints; equal
// fingerprints of large sources only nominate a candidate, which an ingest
// confirms with the content root (D1).
func (s *Source) Fingerprint(newHash func() hash.Hash) (string, error) {
	return Fingerprint(s.img, s.img.Size(), newHash)
}

// Fingerprint computes the fingerprint of the size bytes of r.
func Fingerprint(r io.ReaderAt, size int64, newHash func() hash.Hash) (string, error) {
	h := newHash()
	var hdr [16]byte
	copy(hdr[:8], "dues-fp1")
	binary.BigEndian.PutUint64(hdr[8:], uint64(size))
	h.Write(hdr[:])
	if size <= FingerprintFullLimit {
		if _, err := io.Copy(h, io.NewSectionReader(r, 0, size)); err != nil {
			return "", fmt.Errorf("fingerprint: %w", err)
		}
		return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
	}
	for _, off := range []int64{0, size/2 - fingerprintSample/2, size - fingerprintSample} {
		if _, err := io.Copy(h, io.NewSectionReader(r, off, fingerprintSample)); err != nil {
			return "", fmt.Errorf("fingerprint at %d: %w", off, err)
		}
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
