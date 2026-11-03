package compression_product

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/ulikunitz/xz"

	"indicer/internal/duesarc"
)

// writeCompat writes a zip or tar archive of in's items to w. Every writer
// is closed in order and its error returned: a zip's central directory, a
// tar's footer, the gzip or xz trailer (CMP-2).
func writeCompat(ctx context.Context, w io.Writer, in *archiveInputs, format string, onBytes func(processed, total int64)) error {
	var written int64
	progress := func(n int64) {
		written += n
		if onBytes != nil {
			onBytes(written, in.bytes)
		}
	}
	switch format {
	case archiveFormatZip:
		return writeZipItems(ctx, w, in.items, progress)
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		return writeTarItems(ctx, w, in.items, format, progress)
	}
	return fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
}

func writeZipItems(ctx context.Context, w io.Writer, items []inputItem, progress func(int64)) error {
	zw := zip.NewWriter(w)
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if it.dir {
			hdr := &zip.FileHeader{Name: it.rel + "/", Method: zip.Store, Modified: it.mtime}
			hdr.SetMode(fs.ModeDir | defaultDirPerm)
			if _, err := zw.CreateHeader(hdr); err != nil {
				return err
			}
			continue
		}
		if err := addZipFile(ctx, zw, it.abs, it.rel, it.mtime, progress); err != nil {
			return err
		}
	}
	return zw.Close()
}

func addZipFile(ctx context.Context, zw *zip.Writer, abs, name string, mtime time.Time, progress func(int64)) error {
	src, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mtime}
	hdr.SetMode(info.Mode().Perm())
	wr, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = duesarc.Copy(ctx, wr, src, progress)
	return err
}

func writeTarItems(ctx context.Context, w io.Writer, items []inputItem, format string, progress func(int64)) error {
	var comp io.WriteCloser
	dst := w
	switch format {
	case archiveFormatTarGz:
		comp = gzip.NewWriter(w)
		dst = comp
	case archiveFormatTarXz:
		xw, err := xz.NewWriter(w)
		if err != nil {
			return err
		}
		comp = xw
		dst = comp
	}
	tw := tar.NewWriter(dst)
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if it.dir {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: it.rel + "/", Mode: defaultDirPerm, ModTime: it.mtime}); err != nil {
				return err
			}
			continue
		}
		if err := addTarFile(ctx, tw, it, progress); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if comp != nil {
		return comp.Close()
	}
	return nil
}

func addTarFile(ctx context.Context, tw *tar.Writer, it inputItem, progress func(int64)) error {
	src, err := os.Open(it.abs)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: it.rel, Size: info.Size(), Mode: int64(info.Mode().Perm()), ModTime: it.mtime}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := duesarc.Copy(ctx, tw, io.LimitReader(src, info.Size()), progress)
	if err == nil && n != info.Size() {
		err = fmt.Errorf("%s changed while it was archived", it.abs)
	}
	return err
}

// listCompat lists a zip or tar archive's entries.
func listCompat(ctx context.Context, inputPath, format string, onProgress func(processed, total int64)) ([]archiveEntry, error) {
	switch format {
	case archiveFormatZip:
		za, err := openZip(inputPath, false)
		if err != nil {
			return nil, err
		}
		defer za.Close()
		entries := make([]archiveEntry, 0, len(za.File))
		for i, f := range za.File {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			entries = append(entries, archiveEntry{Name: f.Name, CompressedBytes: f.CompressedSize64, OriginalBytes: f.UncompressedSize64, IsDir: f.Mode().IsDir()})
			if onProgress != nil {
				onProgress(int64(i+1), int64(len(za.File)))
			}
		}
		return entries, nil
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		ts, err := openTarStream(inputPath, format, false)
		if err != nil {
			return nil, err
		}
		defer ts.Close()
		tr := tar.NewReader(duesarc.DamageReader{R: ts})
		var entries []archiveEntry
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			hdr, err := tr.Next()
			if err == io.EOF {
				return entries, nil
			}
			if err != nil {
				return nil, damaged(err)
			}
			size := uint64(max(hdr.Size, 0))
			entries = append(entries, archiveEntry{Name: hdr.Name, CompressedBytes: size, OriginalBytes: size, IsDir: hdr.Typeflag == tar.TypeDir})
			if onProgress != nil {
				onProgress(ts.consumed(), ts.size)
			}
		}
	}
	return nil, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
}

type archiveVerifyStats struct {
	EntryCount    int
	FileCount     int
	DirCount      int
	VerifiedBytes int64
}

// verifyCompat reads every entry of a zip or tar archive to its end, so
// every checksum the format carries is checked.
func verifyCompat(ctx context.Context, inputPath, format string, onProgress func(processed, total int64)) (archiveVerifyStats, error) {
	var st archiveVerifyStats
	switch format {
	case archiveFormatZip:
		za, err := openZip(inputPath, true)
		if err != nil {
			return st, err
		}
		defer za.Close()
		var total int64
		for _, f := range za.File {
			total += int64(f.UncompressedSize64)
		}
		for _, f := range za.File {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			st.EntryCount++
			if f.Mode().IsDir() {
				st.DirCount++
				continue
			}
			st.FileCount++
			rc, err := f.Open()
			if err != nil {
				return st, &duesarc.DamagedError{Err: fmt.Errorf("%s: %w", f.Name, err)}
			}
			n, err := duesarc.Copy(ctx, io.Discard, duesarc.DamageReader{R: rc}, nil)
			_ = rc.Close()
			st.VerifiedBytes += n
			if err != nil {
				return st, err
			}
			if onProgress != nil {
				onProgress(st.VerifiedBytes, total)
			}
		}
		return st, nil
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		ts, err := openTarStream(inputPath, format, true)
		if err != nil {
			return st, err
		}
		defer ts.Close()
		tr := tar.NewReader(duesarc.DamageReader{R: ts})
		for {
			if err := ctx.Err(); err != nil {
				return st, err
			}
			hdr, err := tr.Next()
			if err == io.EOF {
				return st, nil
			}
			if err != nil {
				return st, damaged(err)
			}
			st.EntryCount++
			if hdr.Typeflag == tar.TypeDir {
				st.DirCount++
				continue
			}
			st.FileCount++
			n, err := duesarc.Copy(ctx, io.Discard, tr, nil)
			st.VerifiedBytes += n
			if err != nil {
				return st, damaged(err)
			}
			if onProgress != nil {
				onProgress(ts.consumed(), ts.size)
			}
		}
	}
	return st, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
}
