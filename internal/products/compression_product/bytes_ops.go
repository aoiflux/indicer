package compression_product

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	corecomp "indicer/internal/core/compression"
	coreerr "indicer/internal/core/errors"
	"indicer/lib/fsx"
)

func compressBytes(params json.RawMessage) (any, *coreerr.Error) {
	var req compressReq
	raw, rerr := decodeBase64Params(params, &req)
	if rerr != nil {
		return nil, rerr
	}
	compressed, err := corecomp.Compress(raw, req.Level)
	if err != nil {
		return nil, coreerr.NewWithDetails(coreerr.CodeInternal, errCompressFailed, map[string]string{detailCause: err.Error()})
	}
	return compressRes{
		OriginalBytes:   len(raw),
		CompressedBytes: len(compressed),
		DataBase64:      base64.StdEncoding.EncodeToString(compressed),
	}, nil
}

func decompressBytes(params json.RawMessage) (any, *coreerr.Error) {
	var req decompressReq
	compressed, rerr := decodeBase64Params(params, &req)
	if rerr != nil {
		return nil, rerr
	}
	limit := req.MaxOutputBytes
	if limit == 0 {
		limit = defaultBytesOutputLimit
	}
	if limit < 0 || limit > maxBytesOutputLimit {
		return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "max_output_bytes out of range", map[string]string{"max": strconv.Itoa(maxBytesOutputLimit)})
	}
	raw, err := corecomp.DecompressLimited(compressed, limit)
	if errors.Is(err, corecomp.ErrLimitExceeded) {
		return nil, limitExceeded(&LimitError{Limit: "max_output_bytes", Value: limit, What: "decompressed data"})
	}
	if err != nil {
		return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errDecompressFailed, map[string]string{detailCause: err.Error()})
	}
	return decompressRes{
		CompressedBytes: len(compressed),
		OriginalBytes:   len(raw),
		DataBase64:      base64.StdEncoding.EncodeToString(raw),
	}, nil
}

// compressFile archives one file as a .duesarc archive, beside it unless
// output_path says otherwise.
func (s *Service) compressFile(ctx context.Context, req fileReq, report progressReporter) (fileRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return fileRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating compress file request")
	if req.InputPath == "" {
		return fileRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	abs, err := filepath.Abs(req.InputPath)
	if err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInputPathStatFailed, map[string]string{detailCause: err.Error(), detailInputPath: abs})
	}
	if info.IsDir() {
		return fileRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputMustBeFile)
	}
	out := req.OutputPath
	if out == "" {
		out = abs + archiveExt
	}
	res, rerr := s.createArchive(ctx, createArchiveReq{InputPath: abs, OutputPath: out, Format: archiveFormatDuesarc, Password: req.Password}, report)
	if rerr != nil {
		return fileRes{}, rerr
	}
	return fileRes{
		InputPath:       res.InputPath,
		OutputPath:      res.OutputPath,
		OriginalBytes:   int(res.InputBytes),
		CompressedBytes: int(res.ArchiveBytes),
		Format:          archiveFormatDuesarc,
	}, nil
}

// decompressFile undoes compress_file: it reads a .duesarc archive holding
// one file, or a zstd file (.dues.zst, .zst), found by their first bytes,
// and writes the file to output_path, replacing it atomically.
func (s *Service) decompressFile(ctx context.Context, req fileReq, report progressReporter) (fileRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return fileRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating decompress file request")
	if req.InputPath == "" {
		return fileRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	limits, rerr := req.Limits.resolve()
	if rerr != nil {
		return fileRes{}, rerr
	}
	input, err := filepath.Abs(req.InputPath)
	if err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	out := req.OutputPath
	if out == "" {
		out = defaultDecompressOutput(input)
	}
	if out, err = filepath.Abs(out); err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidOutputPath, map[string]string{detailCause: err.Error()})
	}

	in, err := openArchiveInput(input)
	if err != nil {
		return fileRes{}, opError(ctx, err, errReadInputFileFailed)
	}
	head := make([]byte, 512)
	n, err := in.ReadAt(head, 0)
	size := in.size
	_ = in.Close()
	if err != nil && err != io.EOF {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errReadInputFileFailed, map[string]string{detailCause: err.Error(), detailInputPath: input})
	}
	head = head[:n]
	switch {
	case isZstdSignature(head):
		written, err := decompressZstdFile(ctx, input, out, size, limits, report)
		if err != nil {
			return fileRes{}, opError(ctx, err, errDecompressFailed)
		}
		emitProgress(report, "finalizing", 98, "finalizing decompress file")
		return fileRes{InputPath: input, OutputPath: out, OriginalBytes: int(written), CompressedBytes: int(size), Format: "zstd"}, nil
	case isZipSignature(head) && isDuesarc(input):
		written, err := decompressDuesarcFile(ctx, s.temp, input, out, req.Password, limits, report)
		if err != nil {
			return fileRes{}, opError(ctx, err, errDecompressFailed)
		}
		emitProgress(report, "finalizing", 98, "finalizing decompress file")
		return fileRes{InputPath: input, OutputPath: out, OriginalBytes: int(written), CompressedBytes: int(size), Format: archiveFormatDuesarc}, nil
	}
	return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "decompress_file reads zstd files and .duesarc archives of one file; use extract_archive for other archives", map[string]string{detailInputPath: input})
}

func decompressZstdFile(ctx context.Context, input, out string, size int64, limits Limits, report progressReporter) (int64, error) {
	f, err := os.Open(input)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	counted := &countingReader{r: f}
	dec, err := corecomp.NewLimitedReader(counted, limits.MaxTotalBytes)
	if err != nil {
		return 0, err
	}
	defer dec.Close()
	guard := &outputGuard{limits: limits, in: func() int64 { return counted.n }}
	var written int64
	err = fsx.WriteAtomic(out, defaultFilePerm, fsx.Options{RemoveStale: true}, func(f *os.File) error {
		n, err := copyWithProgress(ctx, guardedWriter{w: f, g: guard}, damageReader{dec}, func(int64) {
			emitProgress(report, "decompressing", progressFromBytes(counted.n, size, 20, 97), "decompressing file data")
		})
		written = n
		return err
	})
	if errors.Is(err, corecomp.ErrLimitExceeded) {
		return 0, &LimitError{Limit: "max_total_bytes", Value: limits.MaxTotalBytes, What: "output"}
	}
	return written, err
}

// decompressDuesarcFile extracts a .duesarc archive's only file to out.
func decompressDuesarcFile(ctx context.Context, temp *tempRoot, input, out, password string, limits Limits, report progressReporter) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(out), defaultDirPerm); err != nil {
		return 0, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(out), ".qwikpak-decompress-*")
	if err != nil {
		return 0, err
	}
	defer func() { _ = fsx.RetryBusy(func() error { return os.RemoveAll(tmp) }) }()
	res, err := extractDuesarc(ctx, temp, input, tmp, password, conflictError, limits, report)
	if err != nil {
		return 0, err
	}
	if res.files != 1 || res.dirs != 0 {
		return 0, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "the archive holds "+describeCount(res.files, "file")+" and "+describeCount(res.dirs, "directory entry")+"; use extract_archive", map[string]string{detailInputPath: input})
	}
	var file string
	var size int64
	err = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			file, size = p, info.Size()
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	if err := fsx.ReplaceFile(file, out, true); err != nil {
		return 0, err
	}
	return size, nil
}
