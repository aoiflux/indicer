package compression_product

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/duesarc"
)

const (
	minChunkSizeKB = 4
	maxChunkSizeKB = 64 << 10
)

// createArchive archives a file, a folder, or files given one by one.
func (s *Service) createArchive(ctx context.Context, req createArchiveReq, report progressReporter) (createArchiveRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return createArchiveRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating archive request")
	if req.InputPath == "" && len(req.InputPaths) == 0 {
		return createArchiveRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	chunk := req.ChunkSizeKB
	if chunk == 0 {
		chunk = defaultChunkSizeKB
	}
	if chunk < minChunkSizeKB || chunk > maxChunkSizeKB || chunk%4 != 0 {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "chunk_size_kb must be a multiple of 4 from 4 to 65536", map[string]string{"chunk_size_kb": strconv.Itoa(req.ChunkSizeKB)})
	}
	if req.SplitSizeMB < 0 {
		return createArchiveRes{}, coreerr.New(coreerr.CodeInvalidRequest, "split_size_mb must not be negative")
	}

	emitProgress(report, "preparing_inputs", 15, "resolving input paths")
	in, rerr := collectInputs(ctx, req)
	if rerr != nil {
		return createArchiveRes{}, rerr
	}
	format, rerr := resolveArchiveFormat(req.Format, req.OutputPath)
	if rerr != nil {
		return createArchiveRes{}, rerr
	}
	outputPath, rerr := archiveOutputPath(req, in, format)
	if rerr != nil {
		return createArchiveRes{}, rerr
	}

	res := createArchiveRes{
		InputPath: in.display, InputPaths: in.paths, Format: format, OutputPath: outputPath,
		InputIsDir: in.isDir, InputBytes: in.bytes, ChunkSizeKB: chunk, SplitSizeMB: req.SplitSizeMB,
		ArchivedFiles: in.files, ArchivedDirs: in.dirs, SkippedInputs: in.nskipped, Skipped: in.skipped,
	}
	partSize := int64(req.SplitSizeMB) * oneMiBBytes
	var err error
	if format == archiveFormatDuesarc {
		res.OutputFiles, res.WorkDir, err = createDuesarc(ctx, s.temp, req, in, outputPath, partSize, chunk, report)
	} else {
		emitProgress(report, "packaging", 35, "packaging compatibility archive")
		res.OutputFiles, err = duesarc.WriteOutput(ctx, outputPath, partSize, func(w io.Writer) error {
			return writeCompat(ctx, w, in, format, func(processed, total int64) {
				emitProgress(report, "packaging", progressFromBytes(processed, total, 35, 97), "packaging compatibility archive")
			})
		})
	}
	if err != nil {
		return createArchiveRes{}, opError(ctx, err, errArchivePackagingFailed)
	}
	if res.ArchiveBytes, err = sumFileSizes(res.OutputFiles); err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errComputeArchiveSizeFailed, map[string]string{detailCause: err.Error()})
	}
	res.Ratio, res.SavedPercent = sizeRatio(res.InputBytes, res.ArchiveBytes)
	emitProgress(report, "finalizing", 98, "finalizing archive")
	return res, nil
}

// archiveOutputPath is where an archive goes: output_path with the format's
// extension, or, without one, the input's name beside the input.
func archiveOutputPath(req createArchiveReq, in *archiveInputs, format string) (string, *coreerr.Error) {
	ext := archiveExtensionForFormat(format)
	out := req.OutputPath
	if out == "" {
		dir, base := filepath.Dir(in.display), filepath.Base(in.display)
		if len(in.paths) > 1 || in.display == "" {
			dir, base = in.display, "bundle"
			if dir == "" {
				dir = filepath.Dir(in.paths[0])
			}
		}
		out = filepath.Join(dir, base)
	}
	if !strings.HasSuffix(strings.ToLower(out), ext) {
		out += ext
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidOutputPath, map[string]string{detailCause: err.Error()})
	}
	if err := os.MkdirAll(filepath.Dir(abs), defaultDirPerm); err != nil {
		return "", coreerr.NewWithDetails(coreerr.CodeInternal, errCreateOutputDirFailed, map[string]string{detailCause: err.Error()})
	}
	return abs, nil
}

func (s *Service) extractArchive(ctx context.Context, req extractArchiveReq, report progressReporter) (extractArchiveRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return extractArchiveRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating extract request")
	if req.InputPath == "" {
		return extractArchiveRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	policy, rerr := resolveConflictPolicy(req.OnConflict)
	if rerr != nil {
		return extractArchiveRes{}, rerr
	}
	limits, rerr := req.Limits.resolve()
	if rerr != nil {
		return extractArchiveRes{}, rerr
	}
	inputPath, err := filepath.Abs(req.InputPath)
	if err != nil {
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	format, rerr := resolveArchiveReadFormat(req.Format, inputPath)
	if rerr != nil {
		return extractArchiveRes{}, rerr
	}
	out := req.OutputPath
	if out == "" {
		out = defaultExtractOutput(inputPath)
	}
	if out, err = filepath.Abs(out); err != nil {
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidOutputPath, map[string]string{detailCause: err.Error()})
	}

	var res commitResult
	if format == archiveFormatDuesarc {
		res, err = extractDuesarc(ctx, s.temp, inputPath, out, req.Password, policy, limits, report)
	} else {
		emitProgress(report, "extracting", 20, "extracting compatibility archive")
		res, err = extractCompat(ctx, inputPath, out, format, policy, limits, func(processed, total int64) {
			emitProgress(report, "extracting", progressFromBytes(processed, total, 20, 97), "extracting compatibility archive")
		})
	}
	if err != nil {
		return extractArchiveRes{}, opError(ctx, err, errExtractArchiveFailed)
	}
	emitProgress(report, "finalizing", 98, "finalizing extraction")
	return extractResult(inputPath, out, res), nil
}

func (s *Service) listArchive(ctx context.Context, req archiveReadReq, report progressReporter) (listArchiveRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return listArchiveRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating list request")
	if req.InputPath == "" {
		return listArchiveRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	inputPath, err := filepath.Abs(req.InputPath)
	if err != nil {
		return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	format, rerr := resolveArchiveReadFormat(req.Format, inputPath)
	if rerr != nil {
		return listArchiveRes{}, rerr
	}
	res := listArchiveRes{InputPath: inputPath, Entries: []archiveEntry{}, LogicalFiles: []archiveLogicalFile{}}
	if format != archiveFormatDuesarc {
		entries, err := listCompat(ctx, inputPath, format, func(processed, total int64) {
			emitProgress(report, "scanning_entries", progressFromBytes(processed, total, 10, 97), "scanning archive entries")
		})
		if err != nil {
			return listArchiveRes{}, opError(ctx, err, errListArchiveFailed)
		}
		res.Entries, res.EntryCount = entries, len(entries)
		emitProgress(report, "finalizing", 98, "finalizing list result")
		return res, nil
	}

	emitProgress(report, "opening", 10, "opening archive")
	za, err := openZip(inputPath, false)
	if err != nil {
		return listArchiveRes{}, opError(ctx, err, errListArchiveFailed)
	}
	for _, f := range za.File {
		if f.Name == manifestFileName {
			res.HasManifest = true
		}
		res.Entries = append(res.Entries, archiveEntry{Name: f.Name, CompressedBytes: f.CompressedSize64, OriginalBytes: f.UncompressedSize64, IsDir: f.Mode().IsDir()})
	}
	za.Close()
	res.EntryCount = len(res.Entries)
	if res.HasManifest {
		m, files, err := listDuesarc(ctx, s.temp, inputPath, req.Password, report)
		if err != nil {
			return listArchiveRes{}, opError(ctx, err, errListArchiveFailed)
		}
		res.ManifestVersion = m.Version
		res.LogicalFiles, res.LogicalFileCount = files, len(files)
	}
	emitProgress(report, "finalizing", 98, "finalizing list result")
	return res, nil
}

func (s *Service) verifyArchive(ctx context.Context, req archiveReadReq, report progressReporter) (verifyArchiveRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return verifyArchiveRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating verify request")
	if req.InputPath == "" {
		return verifyArchiveRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	inputPath, err := filepath.Abs(req.InputPath)
	if err != nil {
		return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	format, rerr := resolveArchiveReadFormat(req.Format, inputPath)
	if rerr != nil {
		return verifyArchiveRes{}, rerr
	}
	res := verifyArchiveRes{InputPath: inputPath, Format: format}
	in, err := openArchiveInput(inputPath)
	if err != nil {
		return verifyArchiveRes{}, opError(ctx, err, errVerifyArchiveFailed)
	}
	res.InputBytes, res.HasSplitInput, res.SplitVerified = in.Size(), in.Split(), in.Verified()
	_ = in.Close()

	emitProgress(report, "verifying", 10, "verifying archive entries")
	stats, err := verifyCompat(ctx, inputPath, compatFormatOf(format), func(processed, total int64) {
		emitProgress(report, "verifying", progressFromBytes(processed, total, 10, 60), "verifying archive entries")
	})
	if err != nil {
		return verifyArchiveRes{}, opError(ctx, err, errVerifyArchiveFailed)
	}
	res.EntryCount, res.FileCount, res.DirCount, res.VerifiedBytes = stats.EntryCount, stats.FileCount, stats.DirCount, stats.VerifiedBytes
	if format == archiveFormatDuesarc {
		m, verified, err := verifyDuesarcContent(ctx, s.temp, inputPath, req.Password, report)
		if err != nil {
			return verifyArchiveRes{}, opError(ctx, err, errVerifyArchiveFailed)
		}
		res.ManifestFound, res.ManifestValid = true, true
		res.ManifestVersion, res.ManifestPipeline, res.ManifestDBPath = m.Version, m.Pipeline, m.DbPath
		res.VerifiedEvidence = verified
		res.FileCount = 0
		for _, e := range m.Entries {
			if e.Type == entryTypeFile {
				res.FileCount++
			}
		}
		if m.Version == manifestVersionV1 {
			res.FileCount = verified
		}
	}
	res.Valid = true
	emitProgress(report, "finalizing", 98, "finalizing verification")
	return res, nil
}

// compatFormatOf is the container format a format's entries are read with:
// a .duesarc archive is a zip.
func compatFormatOf(format string) string {
	if format == archiveFormatDuesarc {
		return archiveFormatZip
	}
	return format
}

func describeCount(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}
