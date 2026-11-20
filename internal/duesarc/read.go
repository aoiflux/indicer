package duesarc

import (
	"archive/zip"
	"context"
	"io"
)

// DamagedError marks an error reading the archive itself, as opposed to
// writing the output.
type DamagedError struct{ Err error }

func (e *DamagedError) Error() string { return "archive data is damaged: " + e.Err.Error() }
func (e *DamagedError) Unwrap() error { return e.Err }

// DamageReader marks every error from R, except io.EOF, as damage.
type DamageReader struct{ R io.Reader }

func (d DamageReader) Read(p []byte) (int, error) {
	n, err := d.R.Read(p)
	if err != nil && err != io.EOF {
		err = &DamagedError{Err: err}
	}
	return n, err
}

// Copy copies src to dst until EOF, checking ctx between reads and passing
// each write's length to onChunk.
func Copy(ctx context.Context, dst io.Writer, src io.Reader, onChunk func(n int64)) (int64, error) {
	buf := make([]byte, 256*1024)
	var total int64
	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return total, err
			}
		}
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw > 0 {
				total += int64(nw)
				if onChunk != nil {
					onChunk(int64(nw))
				}
			}
			if ew != nil {
				return total, ew
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return total, nil
		}
		if er != nil {
			return total, er
		}
	}
}

// Zip is a zip archive, or the archive a split set's parts make, read in
// place.
type Zip struct {
	*zip.Reader
	In *Input
}

// Close closes the archive's files.
func (z *Zip) Close() { _ = z.In.Close() }

// OpenZip opens a zip archive, or the archive a split set's parts make.
// With verifyParts, a split set's parts are first checked against their
// sidecar. archiveName is as for SplitBase.
func OpenZip(path string, verifyParts bool, archiveName func(string) bool) (*Zip, error) {
	in, err := OpenInput(path, archiveName)
	if err != nil {
		return nil, err
	}
	if verifyParts {
		if err := in.Verify(context.Background()); err != nil {
			_ = in.Close()
			return nil, err
		}
	}
	zr, err := zip.NewReader(in, in.size)
	if err != nil {
		_ = in.Close()
		return nil, &DamagedError{Err: err}
	}
	return &Zip{Reader: zr, In: in}, nil
}
