package compression_product

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"math"

	"github.com/ulikunitz/xz"

	"indicer/internal/duesarc"
)

// extractCompat extracts a zip or tar archive into out.
func extractCompat(ctx context.Context, inputPath, out, format, policy string, limits Limits, onBytes func(processed, total int64)) (commitResult, error) {
	switch format {
	case archiveFormatZip:
		za, err := openZip(inputPath, true)
		if err != nil {
			return commitResult{}, err
		}
		defer za.Close()
		x, err := newExtraction(out, limits, policy, nil)
		if err != nil {
			return commitResult{}, err
		}
		if err := extractZip(ctx, za.Reader, za.In.Size(), x, onBytes); err != nil {
			x.abort()
			return commitResult{}, err
		}
		return x.commit()
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		_, declared, err := measureTar(ctx, inputPath, format, limits)
		if err != nil {
			return commitResult{}, err
		}
		x, err := newExtraction(out, limits, policy, nil)
		if err != nil {
			return commitResult{}, err
		}
		if err := x.reserve(declared); err != nil {
			x.abort()
			return commitResult{}, err
		}
		if err := extractTar(ctx, inputPath, format, x, declared, onBytes); err != nil {
			x.abort()
			return commitResult{}, err
		}
		return x.commit()
	}
	return commitResult{}, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
}

// extractZip stages every entry of zr. size is the archive's size, which
// bounds the compressed bytes the ratio limit counts: entries that overlap
// in the file cannot multiply it.
func extractZip(ctx context.Context, zr *zip.Reader, size int64, x *extraction, onBytes func(processed, total int64)) error {
	var declared int64
	for _, f := range zr.File {
		if f.Mode().IsDir() {
			continue
		}
		if f.UncompressedSize64 > math.MaxInt64/2 {
			return &LimitError{Limit: "max_entry_bytes", Value: x.limits.MaxEntryBytes, What: "entry " + f.Name}
		}
		declared += int64(f.UncompressedSize64)
		if declared > x.limits.MaxTotalBytes {
			break
		}
	}
	if err := x.reserve(declared); err != nil {
		return err
	}
	var consumed, written int64
	x.guard.in = func() int64 { return min(consumed, size) }
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = x.addDir(f.Name, f.Modified)
		case !mode.IsRegular():
			err = x.skipEntry(f.Name, entryKind(mode))
		default:
			consumed += int64(f.CompressedSize64)
			err = x.addFile(f.Name, int64(f.UncompressedSize64), f.Modified, func(w io.Writer) error {
				rc, err := f.Open()
				if err != nil {
					return &duesarc.DamagedError{Err: err}
				}
				defer rc.Close()
				_, err = duesarc.Copy(ctx, w, duesarc.DamageReader{R: rc}, func(n int64) {
					written += n
					if onBytes != nil {
						onBytes(written, declared)
					}
				})
				return err
			})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func entryKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode&fs.ModeDevice != 0:
		return "device"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	}
	return "special file"
}

func tarKind(flag byte) string {
	switch flag {
	case tar.TypeSymlink:
		return "symbolic link"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock:
		return "device"
	case tar.TypeFifo:
		return "named pipe"
	}
	return fmt.Sprintf("entry type %q", flag)
}

func tarRegular(flag byte) bool {
	return flag == tar.TypeReg || flag == tar.TypeGNUSparse || flag == 0
}

type tarStream struct {
	io.Reader
	file    *countingReader
	size    int64 // compressed size
	closeFn func() error
}

func (t *tarStream) consumed() int64 { return t.file.n }
func (t *tarStream) Close() error    { return t.closeFn() }

// openTarStream opens a tar archive, or a split set's parts, for reading in
// order. With verifyParts, a split set's parts are first checked against
// their sidecar.
func openTarStream(inputPath, format string, verifyParts bool) (*tarStream, error) {
	in, err := openArchiveInput(inputPath)
	if err != nil {
		return nil, err
	}
	if verifyParts {
		if err := in.Verify(context.Background()); err != nil {
			_ = in.Close()
			return nil, err
		}
	}
	counted := &countingReader{r: io.NewSectionReader(in, 0, in.Size())}
	closers := []io.Closer{in}
	var r io.Reader = counted
	switch format {
	case archiveFormatTar:
	case archiveFormatTarGz:
		gz, err := gzip.NewReader(counted)
		if err != nil {
			_ = in.Close()
			return nil, &duesarc.DamagedError{Err: err}
		}
		r = gz
		closers = append([]io.Closer{gz}, closers...)
	case archiveFormatTarXz:
		xr, err := xz.NewReader(counted)
		if err != nil {
			_ = in.Close()
			return nil, &duesarc.DamagedError{Err: err}
		}
		r = xr
	default:
		_ = in.Close()
		return nil, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
	}
	return &tarStream{Reader: r, file: counted, size: in.Size(), closeFn: func() error {
		var first error
		for _, c := range closers {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
		return first
	}}, nil
}

// measureTar reads a tar archive's headers to count its entries and the
// bytes it declares, stopping at the first limit passed. Skipping an entry
// decompresses it, so the ratio limit applies here too.
func measureTar(ctx context.Context, inputPath, format string, limits Limits) (entries, declared int64, err error) {
	ts, err := openTarStream(inputPath, format, true)
	if err != nil {
		return 0, 0, err
	}
	defer ts.Close()
	guard := &outputGuard{limits: limits, in: ts.consumed}
	tr := tar.NewReader(duesarc.DamageReader{R: ts})
	var pending int64
	for {
		if err := ctx.Err(); err != nil {
			return entries, declared, err
		}
		hdr, err := tr.Next()
		if gerr := guard.add(pending); gerr != nil {
			return entries, declared, gerr
		}
		pending = 0
		if err == io.EOF {
			return entries, declared, nil
		}
		if err != nil {
			return entries, declared, damaged(err)
		}
		entries++
		if entries > limits.MaxEntries {
			return entries, declared, &LimitError{Limit: "max_entries", Value: limits.MaxEntries, What: "entry count"}
		}
		if tarRegular(hdr.Typeflag) {
			if hdr.Size > limits.MaxEntryBytes {
				return entries, declared, &LimitError{Limit: "max_entry_bytes", Value: limits.MaxEntryBytes, What: "entry " + hdr.Name}
			}
			declared += hdr.Size
			if declared > limits.MaxTotalBytes {
				return entries, declared, &LimitError{Limit: "max_total_bytes", Value: limits.MaxTotalBytes, What: "declared output"}
			}
			pending = hdr.Size
		}
	}
}

// damaged marks err as damage to the archive unless it already says what
// went wrong.
func damaged(err error) error {
	if _, ok := err.(*duesarc.DamagedError); ok {
		return err
	}
	if _, ok := asLimitError(err); ok {
		return err
	}
	return &duesarc.DamagedError{Err: err}
}

// extractTar stages every entry of a tar archive.
func extractTar(ctx context.Context, inputPath, format string, x *extraction, declared int64, onBytes func(processed, total int64)) error {
	ts, err := openTarStream(inputPath, format, false)
	if err != nil {
		return err
	}
	defer ts.Close()
	x.guard.in = ts.consumed
	tr := tar.NewReader(duesarc.DamageReader{R: ts})
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return damaged(err)
		}
		switch {
		case hdr.Typeflag == tar.TypeDir:
			err = x.addDir(hdr.Name, hdr.ModTime)
		case tarRegular(hdr.Typeflag):
			err = x.addFile(hdr.Name, hdr.Size, hdr.ModTime, func(w io.Writer) error {
				_, err := duesarc.Copy(ctx, w, tr, func(n int64) {
					written += n
					if onBytes != nil {
						onBytes(written, declared)
					}
				})
				return err
			})
		default:
			err = x.skipEntry(hdr.Name, tarKind(hdr.Typeflag))
		}
		if err != nil {
			return err
		}
	}
}
