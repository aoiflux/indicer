// Package compression_product is qwikpak: archives and compression for the
// FFI library and the compression command. A .duesarc archive holds a DUES
// database of its files' deduplicated, compressed chunks and a manifest
// mapping each path to its content; zip and tar formats are written and read
// for compatibility.
package compression_product

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	coreerr "indicer/internal/core/errors"
	"indicer/internal/duesarc"
	"indicer/lib/dbmeta"
)

const (
	Name       = "qwikpak"
	LegacyName = "compression_product"
)

const (
	opCapabilities    = "capabilities"
	opCreateArchive   = "create_archive"
	opExtractArchive  = "extract_archive"
	opListArchive     = "list_archive"
	opVerifyArchive   = "verify_archive"
	opGetProgress     = "get_progress"
	opCancelTask      = "cancel_task"
	opCompressBytes   = "compress_bytes"
	opDecompressBytes = "decompress_bytes"
	opCompressFile    = "compress_file"
	opDecompressFile  = "decompress_file"

	levelFast    = "fast"
	levelDefault = "default"
	levelBest    = "best"

	archiveExt        = ".duesarc"
	archiveZipExt     = ".zip"
	archiveTarExt     = ".tar"
	archiveTgzExt     = ".tar.gz"
	archiveTxzExt     = ".tar.xz"
	compressedFileExt = ".dues.zst"
	zstdExt           = ".zst"
	outExt            = ".out"

	archiveFormatDuesarc = "duesarc"
	archiveFormatZip     = "zip"
	archiveFormatTar     = "tar"
	archiveFormatTarGz   = "tar.gz"
	archiveFormatTarXz   = "tar.xz"

	defaultChunkSizeKB = 256
	defaultDirPerm     = 0o755
	defaultFilePerm    = 0o644
	oneMiBBytes        = 1024 * 1024

	errInvalidParams             = "invalid params"
	errInputPathRequired         = "input_path is required"
	errInvalidInputPath          = "invalid input_path"
	errInputPathStatFailed       = "input_path stat failed"
	errInvalidOutputPath         = "invalid output_path"
	errCreateOutputDirFailed     = "failed to create output directory"
	errArchivePackagingFailed    = "archive packaging failed"
	errComputeArchiveSizeFailed  = "failed to compute archive size"
	errUnknownOperation          = "unknown operation for compression product"
	errDataBase64Required        = "data_base64 is required"
	errDataBase64Invalid         = "data_base64 must be valid base64"
	errCompressFailed            = "compression failed"
	errDecompressFailed          = "decompression failed"
	errReadInputFileFailed       = "failed to read input file"
	errExtractArchiveFailed      = "archive extraction failed"
	errExtractManifestReadFailed = "failed to read archive manifest"
	errExtractDbPathMissing      = "archive db path is missing"
	errListArchiveFailed         = "list archive failed"
	errVerifyArchiveFailed       = "verify archive failed"
	errManifestMissing           = "manifest missing from archive"
	errInputMustBeFile           = "input_path must be a file"
	errInputPathsFilesOnly       = "input_paths only accepts files"
	errTaskIDRequired            = "task_id is required"
	errProgressTaskNotFound      = "progress task not found"
	errTaskNotCancelable         = "task is not cancelable"
	errTaskCanceled              = "task canceled"
	errUnsupportedArchiveFormat  = "unsupported archive format"

	statusQueued    = "queued"
	statusRunning   = "running"
	statusCompleted = "completed"
	statusFailed    = "failed"
	statusCanceled  = "canceled"

	detailCause     = "cause"
	detailInputPath = "input_path"
	detailOperation = "operation"
)

type Service struct {
	tasks *taskRunner
	temp  *tempRoot
}

// Options configure a Service.
type Options struct {
	// TempDir is where operations make their temporary work directories
	// (--temp-dir); empty means osdirs.Temp.
	TempDir string
}

// New is NewWithOptions with the defaults.
func New() *Service { return NewWithOptions(Options{}) }

func NewWithOptions(o Options) *Service {
	return &Service{tasks: newTaskRunner(), temp: &tempRoot{dir: o.TempDir}}
}

func (s *Service) Product() string {
	return Name
}

// Shutdown cancels every async task, refuses new ones, and waits for them
// until ctx ends.
func (s *Service) Shutdown(ctx context.Context) (canceled, running int) {
	return s.tasks.Shutdown(ctx)
}

func (s *Service) startAsyncTask(ctx context.Context, operation string, fn taskFunc) (any, *coreerr.Error) {
	res, rerr := s.tasks.start(ctx, operation, fn)
	if rerr != nil {
		return nil, rerr
	}
	return res, nil
}

func emitProgress(report progressReporter, stage string, percent int, message string) {
	if report != nil {
		report(stage, percent, message)
	}
}

func ensureNotCanceled(ctx context.Context) *coreerr.Error {
	if ctx != nil && ctx.Err() != nil {
		return canceledError(ctx)
	}
	return nil
}

type compressReq struct {
	DataBase64 string `json:"data_base64"`
	Level      string `json:"level,omitempty"`
}

type compressRes struct {
	OriginalBytes   int    `json:"original_bytes"`
	CompressedBytes int    `json:"compressed_bytes"`
	DataBase64      string `json:"data_base64"`
}

type decompressReq struct {
	DataBase64 string `json:"data_base64"`
	// MaxOutputBytes bounds the decoded data: 64 MiB by default, at most
	// 256 MiB.
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"`
}

type decompressRes struct {
	CompressedBytes int    `json:"compressed_bytes"`
	OriginalBytes   int    `json:"original_bytes"`
	DataBase64      string `json:"data_base64"`
}

type fileReq struct {
	InputPath  string `json:"input_path"`
	OutputPath string `json:"output_path,omitempty"`
	// Level is accepted for compatibility; archives use the engine's level.
	Level    string `json:"level,omitempty"`
	Async    bool   `json:"async,omitempty"`
	Password string `json:"password,omitempty"`
	Limits   Limits `json:"limits,omitempty"`
}

type fileRes struct {
	InputPath       string `json:"input_path"`
	OutputPath      string `json:"output_path"`
	OriginalBytes   int    `json:"original_bytes"`
	CompressedBytes int    `json:"compressed_bytes"`
	// Format is what decompress_file found: "zstd" or "duesarc".
	Format string `json:"format,omitempty"`
	// Ratio and SavedPercent compare the two sizes (sizeRatio).
	Ratio        *float64 `json:"ratio,omitempty"`
	SavedPercent *float64 `json:"saved_percent,omitempty"`
}

type capabilitiesRes struct {
	Product    string   `json:"product"`
	Operations []string `json:"operations"`
	Levels     []string `json:"levels"`
	Formats    []string `json:"formats,omitempty"`
}

type createArchiveReq struct {
	InputPath   string   `json:"input_path"`
	InputPaths  []string `json:"input_paths,omitempty"`
	Format      string   `json:"format,omitempty"`
	Async       bool     `json:"async,omitempty"`
	OutputPath  string   `json:"output_path,omitempty"`
	WorkDir     string   `json:"work_dir,omitempty"`
	ChunkSizeKB int      `json:"chunk_size_kb,omitempty"`
	Password    string   `json:"password,omitempty"`
	SplitSizeMB int      `json:"split_size_mb,omitempty"`
	KeepWorkDir bool     `json:"keep_work_dir,omitempty"`
}

type createArchiveRes struct {
	InputPath    string   `json:"input_path"`
	InputPaths   []string `json:"input_paths,omitempty"`
	Format       string   `json:"format,omitempty"`
	OutputPath   string   `json:"output_path"`
	OutputFiles  []string `json:"output_files"`
	InputIsDir   bool     `json:"input_is_dir"`
	InputBytes   int64    `json:"input_bytes"`
	ArchiveBytes int64    `json:"archive_bytes"`
	ChunkSizeKB  int      `json:"chunk_size_kb"`
	SplitSizeMB  int      `json:"split_size_mb"`
	WorkDir      string   `json:"work_dir,omitempty"`
	// ArchivedFiles and ArchivedDirs count what the archive holds.
	ArchivedFiles int `json:"archived_files"`
	ArchivedDirs  int `json:"archived_dirs"`
	// SkippedInputs are links and special files under the input, which are
	// not archived; Skipped names the first of them.
	SkippedInputs int      `json:"skipped_inputs"`
	Skipped       []string `json:"skipped,omitempty"`
	// Ratio and SavedPercent compare InputBytes with ArchiveBytes
	// (sizeRatio).
	Ratio        *float64 `json:"ratio,omitempty"`
	SavedPercent *float64 `json:"saved_percent,omitempty"`
}

// sizeRatio compares an original size with its archived size: how many
// times smaller the archive is (3 decimals), and the share of the original
// it saves, in percent (1 decimal, negative when the archive is larger).
// Both are nil unless both sizes are positive.
func sizeRatio(original, archived int64) (ratio, savedPercent *float64) {
	if original <= 0 || archived <= 0 {
		return nil, nil
	}
	r := math.Round(float64(original)/float64(archived)*1000) / 1000
	s := math.Round((1-float64(archived)/float64(original))*1000) / 10
	return &r, &s
}

type extractArchiveReq struct {
	InputPath  string `json:"input_path"`
	Format     string `json:"format,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
	Password   string `json:"password,omitempty"`
	Async      bool   `json:"async,omitempty"`
	// OnConflict is what an entry does to an existing file: overwrite (the
	// default), skip, or error.
	OnConflict string `json:"on_conflict,omitempty"`
	Limits     Limits `json:"limits,omitempty"`
}

type extractArchiveRes struct {
	InputPath      string `json:"input_path"`
	OutputPath     string `json:"output_path"`
	ExtractedFiles int    `json:"extracted_files"`
	ExtractedDirs  int    `json:"extracted_dirs"`
	// ReplacedFiles were overwritten; KeptExisting were left as they were
	// (on_conflict skip).
	ReplacedFiles int `json:"replaced_files"`
	KeptExisting  int `json:"kept_existing"`
	// SkippedEntries are links, devices and other entries that are not
	// extracted; Skipped names the first of them.
	SkippedEntries int      `json:"skipped_entries"`
	Skipped        []string `json:"skipped,omitempty"`
}

func extractResult(inputPath, outputPath string, c commitResult) extractArchiveRes {
	return extractArchiveRes{
		InputPath: inputPath, OutputPath: outputPath,
		ExtractedFiles: c.files, ExtractedDirs: c.dirs,
		ReplacedFiles: c.replaced, KeptExisting: c.kept,
		SkippedEntries: c.skipped, Skipped: c.skippedEntries,
	}
}

type archiveReadReq struct {
	InputPath string `json:"input_path"`
	Format    string `json:"format,omitempty"`
	Password  string `json:"password,omitempty"`
	Async     bool   `json:"async,omitempty"`
}

type progressReq struct {
	TaskID string `json:"task_id"`
}

type archiveLogicalFile struct {
	ID             string `json:"id"`
	Hash           string `json:"hash"`
	Name           string `json:"name"`
	Path           string `json:"path,omitempty"`
	Size           int64  `json:"size"`
	TotalSize      int64  `json:"total_size"`
	CompressedSize int64  `json:"compressed_size"`
	Completed      bool   `json:"completed"`
	Failed         bool   `json:"failed"`
	Status         string `json:"status"`
}

type archiveEntry struct {
	Name            string `json:"name"`
	CompressedBytes uint64 `json:"compressed_bytes"`
	OriginalBytes   uint64 `json:"original_bytes"`
	IsDir           bool   `json:"is_dir"`
}

type listArchiveRes struct {
	InputPath        string               `json:"input_path"`
	EntryCount       int                  `json:"entry_count"`
	Entries          []archiveEntry       `json:"entries"`
	HasManifest      bool                 `json:"has_manifest"`
	ManifestVersion  string               `json:"manifest_version,omitempty"`
	LogicalFileCount int                  `json:"logical_file_count"`
	LogicalFiles     []archiveLogicalFile `json:"logical_files"`
}

type verifyArchiveRes struct {
	InputPath        string `json:"input_path"`
	Format           string `json:"format,omitempty"`
	HasSplitInput    bool   `json:"has_split_input"`
	SplitVerified    bool   `json:"split_verified"`
	InputBytes       int64  `json:"input_bytes"`
	Valid            bool   `json:"valid"`
	EntryCount       int    `json:"entry_count"`
	FileCount        int    `json:"file_count"`
	DirCount         int    `json:"dir_count"`
	ManifestFound    bool   `json:"manifest_found"`
	ManifestValid    bool   `json:"manifest_valid"`
	ManifestVersion  string `json:"manifest_version,omitempty"`
	ManifestPipeline string `json:"manifest_pipeline,omitempty"`
	ManifestDBPath   string `json:"manifest_db_path,omitempty"`
	VerifiedBytes    int64  `json:"verified_bytes"`
	// VerifiedEvidence counts the stored objects whose every chunk was read
	// back and matched its hash.
	VerifiedEvidence int `json:"verified_evidence"`
}

func decodeParams[T any](params json.RawMessage, req *T) *coreerr.Error {
	if err := json.Unmarshal(params, req); err != nil {
		return coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
	}
	return nil
}

// run runs op now, or as an async task when async is set.
func run[R any](s *Service, ctx context.Context, operation string, async bool, op func(context.Context, progressReporter) (R, *coreerr.Error)) (any, *coreerr.Error) {
	if async {
		return s.startAsyncTask(ctx, operation, func(ctx context.Context, report progressReporter) (any, *coreerr.Error) {
			res, rerr := op(ctx, report)
			if rerr != nil {
				return nil, rerr
			}
			return res, nil
		})
	}
	res, rerr := op(ctx, nil)
	if rerr != nil {
		return nil, rerr
	}
	return res, nil
}

func (s *Service) Handle(ctx context.Context, operation string, params json.RawMessage) (any, *coreerr.Error) {
	switch operation {
	case opCapabilities:
		return capabilitiesRes{
			Product: Name,
			Operations: []string{
				opCapabilities, opCreateArchive, opExtractArchive, opListArchive, opVerifyArchive,
				opGetProgress, opCancelTask, opCompressBytes, opDecompressBytes, opCompressFile, opDecompressFile,
			},
			Levels:  []string{levelFast, levelDefault, levelBest},
			Formats: []string{archiveFormatDuesarc, archiveFormatZip, archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz},
		}, nil
	case opCreateArchive:
		var req createArchiveReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		return run(s, ctx, operation, req.Async, func(ctx context.Context, report progressReporter) (createArchiveRes, *coreerr.Error) {
			return s.createArchive(ctx, req, report)
		})
	case opExtractArchive:
		var req extractArchiveReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		return run(s, ctx, operation, req.Async, func(ctx context.Context, report progressReporter) (extractArchiveRes, *coreerr.Error) {
			return s.extractArchive(ctx, req, report)
		})
	case opListArchive:
		var req archiveReadReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		return run(s, ctx, operation, req.Async, func(ctx context.Context, report progressReporter) (listArchiveRes, *coreerr.Error) {
			return s.listArchive(ctx, req, report)
		})
	case opVerifyArchive:
		var req archiveReadReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		return run(s, ctx, operation, req.Async, func(ctx context.Context, report progressReporter) (verifyArchiveRes, *coreerr.Error) {
			return s.verifyArchive(ctx, req, report)
		})
	case opGetProgress:
		var req progressReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		if req.TaskID == "" {
			return nil, coreerr.New(coreerr.CodeInvalidRequest, errTaskIDRequired)
		}
		res, ok := s.tasks.get(req.TaskID)
		if !ok {
			return nil, coreerr.New(coreerr.CodeInvalidRequest, errProgressTaskNotFound)
		}
		return res, nil
	case opCancelTask:
		var req progressReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		if req.TaskID == "" {
			return nil, coreerr.New(coreerr.CodeInvalidRequest, errTaskIDRequired)
		}
		res, rerr := s.tasks.cancel(req.TaskID)
		if rerr != nil {
			return nil, rerr
		}
		return res, nil
	case opCompressBytes:
		return compressBytes(params)
	case opDecompressBytes:
		return decompressBytes(params)
	case opCompressFile:
		var req fileReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		return run(s, ctx, operation, req.Async, func(ctx context.Context, report progressReporter) (fileRes, *coreerr.Error) {
			return s.compressFile(ctx, req, report)
		})
	case opDecompressFile:
		var req fileReq
		if rerr := decodeParams(params, &req); rerr != nil {
			return nil, rerr
		}
		return run(s, ctx, operation, req.Async, func(ctx context.Context, report progressReporter) (fileRes, *coreerr.Error) {
			return s.decompressFile(ctx, req, report)
		})
	default:
		return nil, coreerr.NewWithDetails(coreerr.CodeUnknownOperation, errUnknownOperation, map[string]string{detailOperation: operation})
	}
}

func progressFromBytes(processed, total int64, startPercent, endPercent int) int {
	if endPercent < startPercent {
		endPercent = startPercent
	}
	if total <= 0 {
		return startPercent
	}
	processed = min(max(processed, 0), total)
	span := endPercent - startPercent
	if span <= 0 {
		return startPercent
	}
	return startPercent + int((processed*int64(span))/total)
}

// opError maps an archive operation's failure to the caller's error.
func opError(ctx context.Context, err error, msg string) *coreerr.Error {
	if ctx.Err() != nil {
		return canceledError(ctx)
	}
	var rerr *coreerr.Error
	switch {
	case errors.As(err, &rerr):
		return rerr
	case errors.Is(err, dbmeta.ErrPasswordRequired):
		return coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "the archive is encrypted; a password is required", map[string]string{"reason": "password_required"})
	case errors.Is(err, dbmeta.ErrWrongPassword):
		return coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "wrong password for this archive", map[string]string{"reason": "wrong_password"})
	case errors.Is(err, dbmeta.ErrNewerFormat):
		return coreerr.NewWithDetails(coreerr.CodeInvalidArchive, "the archive was written by a newer version", map[string]string{detailCause: err.Error()})
	case errors.Is(err, fs.ErrNotExist):
		return coreerr.NewWithDetails(coreerr.CodeInvalidRequest, msg, map[string]string{detailCause: err.Error()})
	}
	return extractionError(err, msg)
}

func decodeBase64Params[T interface{ getDataBase64() string }](params json.RawMessage, req T) ([]byte, *coreerr.Error) {
	if err := json.Unmarshal(params, req); err != nil {
		return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
	}
	if req.getDataBase64() == "" {
		return nil, coreerr.New(coreerr.CodeInvalidRequest, errDataBase64Required)
	}
	raw, err := base64.StdEncoding.DecodeString(req.getDataBase64())
	if err != nil {
		return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errDataBase64Invalid, map[string]string{detailCause: err.Error()})
	}
	return raw, nil
}

func (r *compressReq) getDataBase64() string   { return r.DataBase64 }
func (r *decompressReq) getDataBase64() string { return r.DataBase64 }

func defaultDecompressOutput(inputPath string) string {
	for _, ext := range []string{compressedFileExt, zstdExt, archiveExt} {
		if before, ok := duesarc.CutSuffixFold(inputPath, ext); ok && before != "" {
			return before
		}
	}
	ext := filepath.Ext(inputPath)
	if ext == "" {
		return inputPath + outExt
	}
	return strings.TrimSuffix(inputPath, ext) + outExt
}

func defaultExtractOutput(inputPath string) string {
	if base, ok := splitBase(inputPath); ok {
		inputPath = base
	}
	for _, ext := range []string{archiveExt, archiveZipExt, archiveTgzExt, ".tgz", archiveTxzExt, ".txz", ".xz", archiveTarExt} {
		if before, ok := duesarc.CutSuffixFold(inputPath, ext); ok && before != "" {
			return before
		}
	}
	return inputPath + outExt
}

func archiveExtensionForFormat(format string) string {
	switch format {
	case archiveFormatZip:
		return archiveZipExt
	case archiveFormatTar:
		return archiveTarExt
	case archiveFormatTarGz:
		return archiveTgzExt
	case archiveFormatTarXz:
		return archiveTxzExt
	default:
		return archiveExt
	}
}

func normalizeArchiveFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case archiveFormatDuesarc:
		return archiveFormatDuesarc
	case archiveFormatZip:
		return archiveFormatZip
	case archiveFormatTar:
		return archiveFormatTar
	case archiveFormatTarGz, "tgz":
		return archiveFormatTarGz
	case archiveFormatTarXz, "txz", "xz":
		return archiveFormatTarXz
	default:
		return ""
	}
}

// detectArchiveFormatFromPath infers a format from a name's extension,
// looking through a split part's number or a sidecar's suffix.
func detectArchiveFormatFromPath(path string) string {
	lower := strings.ToLower(path)
	if base, ok := duesarc.CutSuffixFold(lower, duesarc.SidecarExt); ok {
		lower = base
	} else if base, ok := duesarc.PartBase(lower); ok {
		lower = base
	}
	switch {
	case strings.HasSuffix(lower, archiveExt):
		return archiveFormatDuesarc
	case strings.HasSuffix(lower, archiveZipExt):
		return archiveFormatZip
	case strings.HasSuffix(lower, archiveTgzExt), strings.HasSuffix(lower, ".tgz"):
		return archiveFormatTarGz
	case strings.HasSuffix(lower, archiveTxzExt), strings.HasSuffix(lower, ".txz"), strings.HasSuffix(lower, ".xz"):
		return archiveFormatTarXz
	case strings.HasSuffix(lower, archiveTarExt):
		return archiveFormatTar
	default:
		return ""
	}
}

// resolveArchiveFormat chooses the format of an archive to create.
func resolveArchiveFormat(requested, path string) (string, *coreerr.Error) {
	f := normalizeArchiveFormat(requested)
	if f == "" && strings.TrimSpace(requested) != "" {
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: requested})
	}
	if f != "" {
		return f, nil
	}
	if detected := detectArchiveFormatFromPath(path); detected != "" {
		return detected, nil
	}
	// No format and no recognised extension: duesarc, unless the name has
	// the extension of something else.
	trimmed := strings.TrimSpace(path)
	if trimmed != "" && filepath.Ext(trimmed) != "" {
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: trimmed})
	}
	return archiveFormatDuesarc, nil
}

// resolveArchiveReadFormat chooses the format of an archive to read: as
// requested, from its extension, or from its first bytes.
func resolveArchiveReadFormat(requested, inputPath string) (string, *coreerr.Error) {
	f := normalizeArchiveFormat(requested)
	if f == "" && strings.TrimSpace(requested) != "" {
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: requested})
	}
	if f != "" {
		return f, nil
	}
	if detected := detectArchiveFormatFromPath(inputPath); detected != "" {
		return detected, nil
	}
	detected, err := detectArchiveFormatFromFile(inputPath)
	if err != nil {
		var de *duesarc.DamagedError
		if errors.As(err, &de) {
			return "", coreerr.NewWithDetails(coreerr.CodeInvalidArchive, de.Error(), map[string]string{detailCause: de.Err.Error()})
		}
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	if detected == "" {
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: inputPath})
	}
	return detected, nil
}

func detectArchiveFormatFromFile(path string) (string, error) {
	in, err := openArchiveInput(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	buf := make([]byte, 512)
	n, err := in.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	buf = buf[:n]
	switch {
	case isZipSignature(buf):
		if isDuesarc(path) {
			return archiveFormatDuesarc, nil
		}
		return archiveFormatZip, nil
	case isGzipSignature(buf):
		return archiveFormatTarGz, nil
	case isXzSignature(buf):
		return archiveFormatTarXz, nil
	case isTarHeader(buf):
		return archiveFormatTar, nil
	}
	return "", nil
}

// isDuesarc reports whether the zip at path is a .duesarc archive: it holds
// a valid manifest and the database it names.
func isDuesarc(path string) bool {
	za, err := openZip(path, false)
	if err != nil {
		return false
	}
	defer za.Close()
	_, found, err := zipManifest(za.Reader)
	return found && err == nil
}

func isZipSignature(buf []byte) bool {
	if len(buf) < 4 {
		return false
	}
	return bytes.Equal(buf[:4], []byte{'P', 'K', 0x03, 0x04}) ||
		bytes.Equal(buf[:4], []byte{'P', 'K', 0x05, 0x06}) ||
		bytes.Equal(buf[:4], []byte{'P', 'K', 0x07, 0x08})
}

func isGzipSignature(buf []byte) bool {
	return len(buf) >= 2 && buf[0] == 0x1f && buf[1] == 0x8b
}

func isXzSignature(buf []byte) bool {
	return len(buf) >= 6 && bytes.Equal(buf[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00})
}

func isZstdSignature(buf []byte) bool {
	return len(buf) >= 4 && bytes.Equal(buf[:4], []byte{0x28, 0xb5, 0x2f, 0xfd})
}

func isTarHeader(buf []byte) bool {
	return len(buf) >= 262 && bytes.Equal(buf[257:262], []byte("ustar"))
}

func sumFileSizes(paths []string) (int64, error) {
	var total int64
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}
