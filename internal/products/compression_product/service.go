package compression_product

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"indicer/cli"
	corecomp "indicer/internal/core/compression"
	coreerr "indicer/internal/core/errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
	"github.com/vmihailenco/msgpack/v5"
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

	archiveExt            = ".duesarc"
	archiveZipExt         = ".zip"
	archiveTarExt         = ".tar"
	archiveTgzExt         = ".tar.gz"
	archiveTxzExt         = ".tar.xz"
	archiveSplitSuffixFmt = ".%03d"
	compressedFileExt     = ".dues.zst"
	zstdExt               = ".zst"
	outExt                = ".out"

	archiveFormatDuesarc = "duesarc"
	archiveFormatZip     = "zip"
	archiveFormatTar     = "tar"
	archiveFormatTarGz   = "tar.gz"
	archiveFormatTarXz   = "tar.xz"

	manifestFileName       = "manifest.json"
	manifestVersionV1      = "v1"
	manifestPipelineStore  = "store"
	manifestDbRelativePath = "db"

	defaultChunkSizeKB = 256
	defaultDirPerm     = 0o755
	defaultFilePerm    = 0o644
	oneMiBBytes        = 1024 * 1024

	tempWorkDirPattern       = "duesarc-work-*"
	tempExtractPattern       = "duesarc-extract-*"
	tempAssemblePattern      = "duesarc-assemble-*"
	tempAssembledArchiveName = "assembled" + archiveExt

	errInvalidParams                = "invalid params"
	errInputPathRequired            = "input_path is required"
	errInvalidInputPath             = "invalid input_path"
	errInputPathStatFailed          = "input_path stat failed"
	errFailedToMeasureInput         = "failed to measure input"
	errInvalidOutputPath            = "invalid output_path"
	errCreateOutputDirFailed        = "failed to create output directory"
	errCreateWorkDirFailed          = "failed to create work directory"
	errDeriveKeyFailed              = "failed to derive key"
	errStoreRuntimeInitFailed       = "failed to initialize store runtime"
	errStorePipelineFailed          = "store pipeline failed"
	errManifestEncodeFailed         = "manifest encode failed"
	errManifestWriteFailed          = "manifest write failed"
	errArchivePackagingFailed       = "archive packaging failed"
	errSplitVolumeCreationFailed    = "split volume creation failed"
	errComputeArchiveSizeFailed     = "failed to compute archive size"
	errUnknownOperation             = "unknown operation for compression product"
	errDataBase64Required           = "data_base64 is required"
	errDataBase64Invalid            = "data_base64 must be valid base64"
	errCompressFailed               = "compression failed"
	errDecompressFailed             = "decompression failed"
	errReadInputFileFailed          = "failed to read input file"
	errWriteOutputFileFailed        = "failed to write output file"
	errExtractArchiveFailed         = "archive extraction failed"
	errExtractRestoreFailed         = "archive restore failed"
	errExtractManifestReadFailed    = "failed to read archive manifest"
	errExtractDbPathMissing         = "archive db path is missing"
	errInvalidSplitSizeFmt          = "invalid split_size_mb: %d"
	errNoSplitOutputGenerated       = "no split output generated"
	errPathTraversalDetected        = "path traversal detected"
	errOpenArchiveFailed            = "failed to open archive"
	errExtractOutputDirCreateFailed = "failed to create extract output directory"
	errExtractInputRequired         = "input_path is required"
	errExtractAssembleFailed        = "failed to assemble split archive"
	errListArchiveFailed            = "list archive failed"
	errVerifyArchiveFailed          = "verify archive failed"
	errManifestMissing              = "manifest missing from archive"
	errManifestInvalid              = "manifest is invalid"
	errInputMustBeFile              = "input_path must be a file"
	errInputPathsFilesOnly          = "input_paths only accepts files"
	errTaskIDRequired               = "task_id is required"
	errProgressTaskNotFound         = "progress task not found"
	errTaskNotCancelable            = "task is not cancelable"
	errTaskCanceled                 = "task canceled"
	errUnsupportedArchiveFormat     = "unsupported archive format"

	statusQueued    = "queued"
	statusRunning   = "running"
	statusCompleted = "completed"
	statusFailed    = "failed"
	statusCanceled  = "canceled"

	detailCause      = "cause"
	detailInputPath  = "input_path"
	detailOutputPath = "output_path"
	detailProduct    = "product"
	detailOperation  = "operation"

	responseStatusOK = "ok"

	progressTaskTTL = time.Hour
)

type Service struct{}

func New() *Service {
	return &Service{}
}

func (s *Service) Product() string {
	return Name
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
}

type decompressRes struct {
	CompressedBytes int    `json:"compressed_bytes"`
	OriginalBytes   int    `json:"original_bytes"`
	DataBase64      string `json:"data_base64"`
}

type fileReq struct {
	InputPath  string `json:"input_path"`
	OutputPath string `json:"output_path,omitempty"`
	Level      string `json:"level,omitempty"`
	Async      bool   `json:"async,omitempty"`
}

type fileRes struct {
	InputPath       string `json:"input_path"`
	OutputPath      string `json:"output_path"`
	OriginalBytes   int    `json:"original_bytes"`
	CompressedBytes int    `json:"compressed_bytes"`
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
}

type extractArchiveReq struct {
	InputPath  string `json:"input_path"`
	Format     string `json:"format,omitempty"`
	OutputPath string `json:"output_path,omitempty"`
	Password   string `json:"password,omitempty"`
	Async      bool   `json:"async,omitempty"`
}

type extractArchiveRes struct {
	InputPath      string `json:"input_path"`
	OutputPath     string `json:"output_path"`
	ExtractedFiles int    `json:"extracted_files"`
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

type cancelTaskRes struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type progressRes struct {
	TaskID         string         `json:"task_id"`
	Operation      string         `json:"operation"`
	Status         string         `json:"status"`
	Percent        int            `json:"percent"`
	Stage          string         `json:"stage,omitempty"`
	Message        string         `json:"message,omitempty"`
	Error          string         `json:"error,omitempty"`
	ErrorDetails   map[string]any `json:"error_details,omitempty"`
	Result         any            `json:"result,omitempty"`
	StartedAtUTC   string         `json:"started_at_utc"`
	UpdatedAtUTC   string         `json:"updated_at_utc"`
	CompletedAtUTC string         `json:"completed_at_utc,omitempty"`
}

type asyncStartRes struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

type progressReporter func(stage string, percent int, message string)

type progressTask struct {
	TaskID       string
	Operation    string
	Status       string
	Percent      int
	Stage        string
	Message      string
	Error        string
	ErrorDetails map[string]any
	Result       any
	StartedAt    time.Time
	UpdatedAt    time.Time
	CompletedAt  time.Time
	ExpiresAt    time.Time
	Cancel       context.CancelFunc
}

type progressTracker struct {
	mu    sync.RWMutex
	seq   uint64
	tasks map[string]*progressTask
}

var compressionProgress = &progressTracker{tasks: make(map[string]*progressTask)}

type archiveLogicalFile struct {
	ID             string `json:"id"`
	Hash           string `json:"hash"`
	Name           string `json:"name"`
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
	LogicalFileCount int                  `json:"logical_file_count"`
	LogicalFiles     []archiveLogicalFile `json:"logical_files"`
}

type verifyArchiveRes struct {
	InputPath        string `json:"input_path"`
	Format           string `json:"format,omitempty"`
	HasSplitInput    bool   `json:"has_split_input"`
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
}

type archiveVerifyStats struct {
	EntryCount    int
	FileCount     int
	DirCount      int
	VerifiedBytes int64
}

type archiveManifest struct {
	Version      string   `json:"version"`
	CreatedAtUTC string   `json:"created_at_utc"`
	InputPath    string   `json:"input_path"`
	InputPaths   []string `json:"input_paths,omitempty"`
	InputIsDir   bool     `json:"input_is_dir"`
	InputBytes   int64    `json:"input_bytes"`
	ChunkSizeKB  int      `json:"chunk_size_kb"`
	Pipeline     string   `json:"pipeline"`
	DbPath       string   `json:"db_path"`
}

func (s *Service) Handle(_ context.Context, operation string, params json.RawMessage) (any, *coreerr.Error) {
	switch operation {
	case opCapabilities:
		return capabilitiesRes{
			Product: Name,
			Operations: []string{
				opCapabilities,
				opCreateArchive,
				opExtractArchive,
				opListArchive,
				opVerifyArchive,
				opGetProgress,
				opCancelTask,
				opCompressBytes,
				opDecompressBytes,
				opCompressFile,
				opDecompressFile,
			},
			Levels:  []string{levelFast, levelDefault, levelBest},
			Formats: []string{archiveFormatDuesarc, archiveFormatZip, archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz},
		}, nil
	case opCreateArchive:
		var req createArchiveReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.Async {
			return s.startAsyncTask(opCreateArchive, func(ctx context.Context, report progressReporter) (any, *coreerr.Error) {
				return s.createArchiveWithProgressCtx(ctx, req, report)
			})
		}
		res, rerr := s.createArchiveWithProgress(req, nil)
		if rerr != nil {
			return nil, rerr
		}
		return res, nil
	case opExtractArchive:
		var req extractArchiveReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.Async {
			return s.startAsyncTask(opExtractArchive, func(ctx context.Context, report progressReporter) (any, *coreerr.Error) {
				return s.extractArchiveWithProgressCtx(ctx, req, report)
			})
		}
		return s.extractArchiveWithProgress(req, nil)
	case opListArchive:
		var req archiveReadReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.Async {
			return s.startAsyncTask(opListArchive, func(ctx context.Context, report progressReporter) (any, *coreerr.Error) {
				return s.listArchiveWithProgressCtx(ctx, req, report)
			})
		}
		return s.listArchiveWithProgress(req, nil)
	case opVerifyArchive:
		var req archiveReadReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.Async {
			return s.startAsyncTask(opVerifyArchive, func(ctx context.Context, report progressReporter) (any, *coreerr.Error) {
				return s.verifyArchiveWithProgressCtx(ctx, req, report)
			})
		}
		return s.verifyArchiveWithProgress(req, nil)
	case opGetProgress:
		var req progressReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.TaskID == "" {
			return nil, coreerr.New(coreerr.CodeInvalidRequest, errTaskIDRequired)
		}
		res, ok := compressionProgress.get(req.TaskID)
		if !ok {
			return nil, coreerr.New(coreerr.CodeInvalidRequest, errProgressTaskNotFound)
		}
		return res, nil
	case opCancelTask:
		var req progressReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.TaskID == "" {
			return nil, coreerr.New(coreerr.CodeInvalidRequest, errTaskIDRequired)
		}
		res, rerr := compressionProgress.cancel(req.TaskID)
		if rerr != nil {
			return nil, rerr
		}
		return res, nil
	case opCompressBytes:
		var req compressReq
		raw, derr := decodeBase64Params(params, &req)
		if derr != nil {
			return nil, derr
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
	case opDecompressBytes:
		var req decompressReq
		compressed, derr := decodeBase64Params(params, &req)
		if derr != nil {
			return nil, derr
		}
		raw, err := corecomp.Decompress(compressed)
		if err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInternal, errDecompressFailed, map[string]string{detailCause: err.Error()})
		}
		return decompressRes{
			CompressedBytes: len(compressed),
			OriginalBytes:   len(raw),
			DataBase64:      base64.StdEncoding.EncodeToString(raw),
		}, nil
	case opCompressFile:
		var req fileReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.Async {
			return s.startAsyncTask(opCompressFile, func(ctx context.Context, report progressReporter) (any, *coreerr.Error) {
				return s.compressFileWithProgressCtx(ctx, req, report)
			})
		}
		return s.compressFileWithProgress(req, nil)
	case opDecompressFile:
		var req fileReq
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidParams, map[string]string{detailCause: err.Error()})
		}
		if req.Async {
			return s.startAsyncTask(opDecompressFile, func(ctx context.Context, report progressReporter) (any, *coreerr.Error) {
				return s.decompressFileWithProgressCtx(ctx, req, report)
			})
		}
		return s.decompressFileWithProgress(req, nil)
	default:
		return nil, coreerr.NewWithDetails(coreerr.CodeUnknownOperation, errUnknownOperation, map[string]string{detailOperation: operation})
	}
}

func (s *Service) startAsyncTask(operation string, fn func(context.Context, progressReporter) (any, *coreerr.Error)) (any, *coreerr.Error) {
	taskID := compressionProgress.start(operation)
	taskCtx, cancel := context.WithCancel(context.Background())
	compressionProgress.attachCancel(taskID, cancel)
	go func() {
		compressionProgress.update(taskID, statusRunning, 1, "starting", "operation started")
		report := func(stage string, percent int, message string) {
			compressionProgress.update(taskID, statusRunning, percent, stage, message)
		}

		result, rerr := fn(taskCtx, report)
		if rerr != nil {
			if taskCtx.Err() != nil || rerr.Message == errTaskCanceled {
				compressionProgress.markCanceled(taskID)
				return
			}
			details := map[string]any{}
			for k, v := range rerr.Details {
				details[k] = v
			}
			compressionProgress.fail(taskID, rerr.Message, details)
			return
		}
		compressionProgress.complete(taskID, result)
	}()

	return asyncStartRes{TaskID: taskID, Status: statusQueued}, nil
}

func emitProgress(report progressReporter, stage string, percent int, message string) {
	if report != nil {
		report(stage, percent, message)
	}
}

func ensureNotCanceled(ctx context.Context) *coreerr.Error {
	if ctx == nil {
		return nil
	}
	if ctx.Err() != nil {
		return canceledError()
	}
	return nil
}

func canceledError() *coreerr.Error {
	return coreerr.New(coreerr.CodeInvalidRequest, errTaskCanceled)
}

func progressFromBytes(processed, total int64, startPercent, endPercent int) int {
	if endPercent < startPercent {
		endPercent = startPercent
	}
	if total <= 0 {
		return startPercent
	}
	if processed < 0 {
		processed = 0
	}
	if processed > total {
		processed = total
	}
	span := endPercent - startPercent
	if span <= 0 {
		return startPercent
	}
	return startPercent + int((processed*int64(span))/total)
}

func copyWithProgress(ctx context.Context, dst io.Writer, src io.Reader, onChunk func(n int64)) (int64, error) {
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

func (s *Service) extractArchive(req extractArchiveReq) (extractArchiveRes, *coreerr.Error) {
	return s.extractArchiveWithProgress(req, nil)
}

func (s *Service) compressFileWithProgress(req fileReq, report progressReporter) (fileRes, *coreerr.Error) {
	return s.compressFileWithProgressCtx(context.Background(), req, report)
}

func (s *Service) compressFileWithProgressCtx(ctx context.Context, req fileReq, report progressReporter) (fileRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return fileRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating compress file request")
	if req.InputPath == "" {
		return fileRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	inPathAbs, err := filepath.Abs(req.InputPath)
	if err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	info, err := os.Stat(inPathAbs)
	if err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInputPathStatFailed, map[string]string{detailCause: err.Error(), detailInputPath: inPathAbs})
	}
	if info.IsDir() {
		return fileRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputMustBeFile)
	}

	archiveReq := createArchiveReq{
		InputPath:  inPathAbs,
		OutputPath: req.OutputPath,
	}
	archiveRes, rerr := s.createArchiveWithProgressCtx(ctx, archiveReq, report)
	if rerr != nil {
		return fileRes{}, rerr
	}

	return fileRes{
		InputPath:       archiveRes.InputPath,
		OutputPath:      archiveRes.OutputPath,
		OriginalBytes:   int(archiveRes.InputBytes),
		CompressedBytes: int(archiveRes.ArchiveBytes),
	}, nil
}

func (s *Service) decompressFileWithProgress(req fileReq, report progressReporter) (fileRes, *coreerr.Error) {
	return s.decompressFileWithProgressCtx(context.Background(), req, report)
}

func (s *Service) decompressFileWithProgressCtx(ctx context.Context, req fileReq, report progressReporter) (fileRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return fileRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating decompress file request")
	if req.InputPath == "" {
		return fileRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}
	if req.OutputPath == "" {
		req.OutputPath = defaultDecompressOutput(req.InputPath)
	}

	emitProgress(report, "reading", 20, "reading compressed input")
	compressedBytes, err := os.ReadFile(req.InputPath)
	if err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errReadInputFileFailed, map[string]string{detailCause: err.Error(), detailInputPath: req.InputPath})
	}
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return fileRes{}, rerr
	}

	emitProgress(report, "decompressing", 70, "decompressing file data")
	raw, err := corecomp.Decompress(compressedBytes)
	if err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errDecompressFailed, map[string]string{detailCause: err.Error()})
	}
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return fileRes{}, rerr
	}

	emitProgress(report, "writing", 90, "writing decompressed output")
	if err := os.WriteFile(req.OutputPath, raw, defaultFilePerm); err != nil {
		return fileRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errWriteOutputFileFailed, map[string]string{detailCause: err.Error(), detailOutputPath: req.OutputPath})
	}

	emitProgress(report, "finalizing", 98, "finalizing decompress file")
	return fileRes{
		InputPath:       req.InputPath,
		OutputPath:      req.OutputPath,
		OriginalBytes:   len(raw),
		CompressedBytes: len(compressedBytes),
	}, nil
}

func (s *Service) extractArchiveWithProgress(req extractArchiveReq, report progressReporter) (extractArchiveRes, *coreerr.Error) {
	return s.extractArchiveWithProgressCtx(context.Background(), req, report)
}

func (s *Service) extractArchiveWithProgressCtx(ctx context.Context, req extractArchiveReq, report progressReporter) (extractArchiveRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return extractArchiveRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating extract request")
	if req.InputPath == "" {
		return extractArchiveRes{}, coreerr.New(coreerr.CodeInvalidRequest, errExtractInputRequired)
	}
	inputPath, err := filepath.Abs(req.InputPath)
	if err != nil {
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	format, ferr := resolveArchiveReadFormat(req.Format, inputPath)
	if ferr != nil {
		return extractArchiveRes{}, ferr
	}
	extractPath := req.OutputPath
	if extractPath == "" {
		extractPath = defaultExtractOutput(inputPath)
	}
	extractPath, err = filepath.Abs(extractPath)
	if err != nil {
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidOutputPath, map[string]string{detailCause: err.Error()})
	}
	if err := os.MkdirAll(extractPath, defaultDirPerm); err != nil {
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errExtractOutputDirCreateFailed, map[string]string{detailCause: err.Error()})
	}
	if format != archiveFormatDuesarc {
		emitProgress(report, "extracting", 20, "extracting compatibility archive")
		extractedCount, xerr := extractCompatibilityArchiveCtx(ctx, inputPath, extractPath, format, func(processed, total int64) {
			p := progressFromBytes(processed, total, 20, 97)
			emitProgress(report, "extracting", p, "extracting compatibility archive")
		})
		if xerr != nil {
			if errors.Is(xerr, context.Canceled) {
				return extractArchiveRes{}, canceledError()
			}
			return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errExtractArchiveFailed, map[string]string{detailCause: xerr.Error()})
		}
		emitProgress(report, "finalizing", 98, "finalizing extraction")
		return extractArchiveRes{InputPath: inputPath, OutputPath: extractPath, ExtractedFiles: extractedCount}, nil
	}

	emitProgress(report, "assembling", 20, "preparing archive input")
	archivePath := inputPath
	cleanupPath := ""
	if isSplitVolumePath(inputPath) {
		assembledPath, aerr := assembleSplitArchive(inputPath)
		if aerr != nil {
			return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errExtractAssembleFailed, map[string]string{detailCause: aerr.Error()})
		}
		archivePath = assembledPath
		cleanupPath = assembledPath
	}
	if cleanupPath != "" {
		defer os.Remove(cleanupPath)
	}

	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return extractArchiveRes{}, rerr
	}

	stagingDir, err := os.MkdirTemp("", tempExtractPattern)
	if err != nil {
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errCreateWorkDirFailed, map[string]string{detailCause: err.Error()})
	}
	defer os.RemoveAll(stagingDir)

	emitProgress(report, "extracting", 40, "extracting archive entries")
	_, xerr := extractArchiveToDirWithProgressCtx(ctx, archivePath, stagingDir, func(processed, total int64) {
		p := progressFromBytes(processed, total, 40, 80)
		emitProgress(report, "extracting", p, "extracting archive entries")
	})
	if xerr != nil {
		if errors.Is(xerr, context.Canceled) {
			return extractArchiveRes{}, canceledError()
		}
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errExtractArchiveFailed, map[string]string{detailCause: xerr.Error()})
	}

	emitProgress(report, "reading_manifest", 55, "reading archive manifest")
	manifest, merr := readArchiveManifest(filepath.Join(stagingDir, manifestFileName))
	if merr != nil {
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errExtractManifestReadFailed, map[string]string{detailCause: merr.Error()})
	}

	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return extractArchiveRes{}, rerr
	}

	emitProgress(report, "restoring", 85, "restoring files")
	restoredCount, rerr := restoreArchiveFilesWithProgressCtx(ctx, stagingDir, extractPath, manifest, req.Password, func(processed, total, restored int) {
		percent := 85
		message := "restoring files"
		if total > 0 {
			percent = progressFromBytes(int64(processed), int64(total), 85, 97)
			message = fmt.Sprintf("restoring files (%d/%d, restored=%d)", processed, total, restored)
		}
		emitProgress(report, "restoring", percent, message)
	})
	if rerr != nil {
		if errors.Is(rerr, context.Canceled) {
			return extractArchiveRes{}, canceledError()
		}
		return extractArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errExtractRestoreFailed, map[string]string{detailCause: rerr.Error()})
	}

	emitProgress(report, "finalizing", 98, "finalizing extraction")
	return extractArchiveRes{InputPath: inputPath, OutputPath: extractPath, ExtractedFiles: restoredCount}, nil
}

func (s *Service) verifyArchiveWithProgress(req archiveReadReq, report progressReporter) (verifyArchiveRes, *coreerr.Error) {
	return s.verifyArchiveWithProgressCtx(context.Background(), req, report)
}

func (s *Service) listArchiveWithProgress(req archiveReadReq, report progressReporter) (listArchiveRes, *coreerr.Error) {
	return s.listArchiveWithProgressCtx(context.Background(), req, report)
}

func (s *Service) listArchiveWithProgressCtx(ctx context.Context, req archiveReadReq, report progressReporter) (listArchiveRes, *coreerr.Error) {
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
	format, ferr := resolveArchiveReadFormat(req.Format, inputPath)
	if ferr != nil {
		return listArchiveRes{}, ferr
	}
	if format != archiveFormatDuesarc {
		entries, lerr := listCompatibilityArchiveEntriesCtx(ctx, inputPath, format, func(processed, total int64) {
			p := progressFromBytes(processed, total, 10, 97)
			emitProgress(report, "scanning_entries", p, "scanning archive entries")
		})
		if lerr != nil {
			if errors.Is(lerr, context.Canceled) {
				return listArchiveRes{}, canceledError()
			}
			return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errListArchiveFailed, map[string]string{detailCause: lerr.Error()})
		}
		emitProgress(report, "finalizing", 98, "finalizing list result")
		return listArchiveRes{InputPath: inputPath, EntryCount: len(entries), Entries: entries, HasManifest: false, LogicalFileCount: 0, LogicalFiles: []archiveLogicalFile{}}, nil
	}

	emitProgress(report, "opening", 10, "opening archive")
	zr, cleanup, err := openArchiveReader(inputPath)
	if err != nil {
		return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errListArchiveFailed, map[string]string{detailCause: err.Error()})
	}
	defer cleanup()

	entries := make([]archiveEntry, 0, len(zr.File))
	hasManifest := false
	entryCount := len(zr.File)
	for idx, f := range zr.File {
		if rerr := ensureNotCanceled(ctx); rerr != nil {
			return listArchiveRes{}, rerr
		}
		if f.Name == manifestFileName {
			hasManifest = true
		}
		entries = append(entries, archiveEntry{
			Name:            f.Name,
			CompressedBytes: f.CompressedSize64,
			OriginalBytes:   f.UncompressedSize64,
			IsDir:           f.FileInfo().IsDir(),
		})
		if entryCount > 0 {
			p := 10 + int((int64(idx+1)*30)/int64(entryCount))
			emitProgress(report, "scanning_entries", p, "scanning archive entries")
		}
	}

	logicalFiles := make([]archiveLogicalFile, 0)
	if hasManifest {
		if rerr := ensureNotCanceled(ctx); rerr != nil {
			return listArchiveRes{}, rerr
		}

		archivePath := inputPath
		cleanupPath := ""
		if isSplitVolumePath(inputPath) {
			emitProgress(report, "assembling", 45, "assembling split archive")
			assembledPath, aerr := assembleSplitArchive(inputPath)
			if aerr != nil {
				return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errListArchiveFailed, map[string]string{detailCause: aerr.Error()})
			}
			archivePath = assembledPath
			cleanupPath = assembledPath
		}
		if cleanupPath != "" {
			defer os.Remove(cleanupPath)
		}

		stagingDir, err := os.MkdirTemp("", tempExtractPattern)
		if err != nil {
			return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errCreateWorkDirFailed, map[string]string{detailCause: err.Error()})
		}
		defer os.RemoveAll(stagingDir)

		emitProgress(report, "extracting", 55, "extracting manifest and metadata")
		if _, xerr := extractArchiveToDirWithProgressCtx(ctx, archivePath, stagingDir, func(processed, total int64) {
			p := progressFromBytes(processed, total, 55, 85)
			emitProgress(report, "extracting", p, "extracting manifest and metadata")
		}); xerr != nil {
			if errors.Is(xerr, context.Canceled) {
				return listArchiveRes{}, canceledError()
			}
			return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errListArchiveFailed, map[string]string{detailCause: xerr.Error()})
		}

		emitProgress(report, "reading_manifest", 88, "reading archive manifest")
		manifest, merr := readArchiveManifest(filepath.Join(stagingDir, manifestFileName))
		if merr != nil {
			return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errListArchiveFailed, map[string]string{detailCause: merr.Error()})
		}

		emitProgress(report, "scanning_logical", 92, "reading logical file metadata")
		logicalFiles, err = listArchiveLogicalFilesCtx(ctx, stagingDir, manifest, req.Password)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return listArchiveRes{}, canceledError()
			}
			return listArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errListArchiveFailed, map[string]string{detailCause: err.Error()})
		}
	}

	emitProgress(report, "finalizing", 98, "finalizing list result")
	return listArchiveRes{
		InputPath:        inputPath,
		EntryCount:       len(entries),
		Entries:          entries,
		HasManifest:      hasManifest,
		LogicalFileCount: len(logicalFiles),
		LogicalFiles:     logicalFiles,
	}, nil
}

func (s *Service) verifyArchiveWithProgressCtx(ctx context.Context, req archiveReadReq, report progressReporter) (verifyArchiveRes, *coreerr.Error) {
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
	format, ferr := resolveArchiveReadFormat(req.Format, inputPath)
	if ferr != nil {
		return verifyArchiveRes{}, ferr
	}
	hasSplitInput := isSplitVolumePath(inputPath)
	inputBytes, _ := measureArchiveInputBytes(inputPath)
	if format != archiveFormatDuesarc {
		stats, verr := verifyCompatibilityArchiveCtx(ctx, inputPath, format, func(processed, total int64) {
			emitProgress(report, "verifying", progressFromBytes(processed, total, 10, 95), "verifying archive entries")
		})
		if verr != nil {
			if errors.Is(verr, context.Canceled) {
				return verifyArchiveRes{}, canceledError()
			}
			return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: verr.Error()})
		}
		emitProgress(report, "finalizing", 98, "finalizing verification")
		return verifyArchiveRes{
			InputPath:     inputPath,
			Format:        format,
			HasSplitInput: hasSplitInput,
			InputBytes:    inputBytes,
			Valid:         true,
			EntryCount:    stats.EntryCount,
			FileCount:     stats.FileCount,
			DirCount:      stats.DirCount,
			ManifestFound: false,
			ManifestValid: false,
			VerifiedBytes: stats.VerifiedBytes,
		}, nil
	}

	emitProgress(report, "opening", 10, "opening archive")
	zr, cleanup, err := openArchiveReader(inputPath)
	if err != nil {
		return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: err.Error()})
	}
	defer cleanup()

	manifestFound := false
	manifestValid := false
	manifestVersion := ""
	manifestPipeline := ""
	manifestDBPath := ""
	logicalFileCount := 0
	var totalBytes int64
	var totalExpectedBytes int64
	fileCount := 0
	dirCount := 0
	for _, f := range zr.File {
		totalExpectedBytes += int64(f.UncompressedSize64)
	}
	for _, f := range zr.File {
		if rerr := ensureNotCanceled(ctx); rerr != nil {
			return verifyArchiveRes{}, rerr
		}
		rc, err := f.Open()
		if err != nil {
			return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: err.Error()})
		}
		n, copyErr := copyWithProgress(ctx, io.Discard, rc, nil)
		closeErr := rc.Close()
		if copyErr != nil {
			if errors.Is(copyErr, context.Canceled) {
				return verifyArchiveRes{}, canceledError()
			}
			return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: copyErr.Error()})
		}
		if closeErr != nil {
			return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: closeErr.Error()})
		}
		totalBytes += n
		if f.FileInfo().IsDir() {
			dirCount++
		} else {
			fileCount++
		}

		if f.Name == manifestFileName {
			manifestFound = true
			manifest, merr := readManifestFromZipFile(f)
			if merr != nil {
				return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: merr.Error()})
			}
			manifestVersion = manifest.Version
			manifestPipeline = manifest.Pipeline
			manifestDBPath = manifest.DbPath
			if valid, vErr := validateManifest(zr, f); vErr != nil {
				return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: vErr.Error()})
			} else {
				manifestValid = valid
			}
		}

		emitProgress(report, "verifying", progressFromBytes(totalBytes, totalExpectedBytes, 10, 95), "verifying archive entries")
	}

	if !manifestFound {
		return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: errManifestMissing})
	}

	if !manifestValid {
		return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: errManifestInvalid})
	}

	logicalFiles, lfErr := listDuearcLogicalFilesFromInputCtx(ctx, inputPath, req.Password)
	if lfErr != nil {
		if errors.Is(lfErr, context.Canceled) {
			return verifyArchiveRes{}, canceledError()
		}
		return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: lfErr.Error()})
	}
	logicalFileCount = len(logicalFiles)
	fileCount = logicalFileCount

	emitProgress(report, "finalizing", 98, "finalizing verification")
	return verifyArchiveRes{
		InputPath:        inputPath,
		Format:           archiveFormatDuesarc,
		HasSplitInput:    hasSplitInput,
		InputBytes:       inputBytes,
		Valid:            true,
		EntryCount:       len(zr.File),
		FileCount:        fileCount,
		DirCount:         dirCount,
		ManifestFound:    manifestFound,
		ManifestValid:    manifestValid,
		ManifestVersion:  manifestVersion,
		ManifestPipeline: manifestPipeline,
		ManifestDBPath:   manifestDBPath,
		VerifiedBytes:    totalBytes,
	}, nil
}

func (s *Service) createArchive(req createArchiveReq) (createArchiveRes, *coreerr.Error) {
	return s.createArchiveWithProgress(req, nil)
}

func (s *Service) createArchiveWithProgress(req createArchiveReq, report progressReporter) (createArchiveRes, *coreerr.Error) {
	return s.createArchiveWithProgressCtx(context.Background(), req, report)
}

func (s *Service) createArchiveWithProgressCtx(ctx context.Context, req createArchiveReq, report progressReporter) (createArchiveRes, *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return createArchiveRes{}, rerr
	}
	emitProgress(report, "validating", 5, "validating archive request")
	if req.InputPath == "" && len(req.InputPaths) == 0 {
		return createArchiveRes{}, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}

	emitProgress(report, "preparing_inputs", 15, "resolving input paths")
	storeInputPath, inputPathForManifest, inputPathsAbs, inputIsDir, inputBytes, cleanupInput, cerr := resolveArchiveInputsCtx(ctx, req)
	if cerr != nil {
		return createArchiveRes{}, cerr
	}
	if cleanupInput != nil {
		defer cleanupInput()
	}
	format, ferr := resolveArchiveFormat(req.Format, req.OutputPath)
	if ferr != nil {
		return createArchiveRes{}, ferr
	}

	outputPath := req.OutputPath
	if outputPath == "" {
		base := filepath.Base(inputPathForManifest)
		if len(inputPathsAbs) > 1 {
			base = "bundle"
		}
		outputPath = base + archiveExtensionForFormat(format)
	}
	if !strings.HasSuffix(strings.ToLower(outputPath), strings.ToLower(archiveExtensionForFormat(format))) {
		outputPath += archiveExtensionForFormat(format)
	}
	outputPathAbs, absErr := filepath.Abs(outputPath)
	if absErr != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidOutputPath, map[string]string{detailCause: absErr.Error()})
	}
	outputPath = outputPathAbs
	if err := os.MkdirAll(filepath.Dir(outputPath), defaultDirPerm); err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errCreateOutputDirFailed, map[string]string{detailCause: err.Error()})
	}

	chunkSize := req.ChunkSizeKB
	if chunkSize <= 0 {
		chunkSize = defaultChunkSizeKB
	}

	if format != archiveFormatDuesarc {
		emitProgress(report, "packaging", 35, "packaging compatibility archive")
		totalBytes, _ := measureInputBytes(storeInputPath)
		if err := createCompatibilityArchiveCtx(ctx, storeInputPath, outputPath, format, func(processed, total int64) {
			if total <= 0 {
				total = totalBytes
			}
			p := progressFromBytes(processed, total, 35, 97)
			emitProgress(report, "packaging", p, "packaging compatibility archive")
		}); err != nil {
			if errors.Is(err, context.Canceled) {
				return createArchiveRes{}, canceledError()
			}
			return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errArchivePackagingFailed, map[string]string{detailCause: err.Error()})
		}

		outputFiles := []string{outputPath}
		if req.SplitSizeMB > 0 {
			emitProgress(report, "splitting", 92, "creating split volumes")
			parts, serr := splitArchiveCtx(ctx, outputPath, req.SplitSizeMB)
			if serr != nil {
				if errors.Is(serr, context.Canceled) {
					return createArchiveRes{}, canceledError()
				}
				return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errSplitVolumeCreationFailed, map[string]string{detailCause: serr.Error()})
			}
			outputFiles = parts
		}

		archiveBytes, err := sumFileSizes(outputFiles)
		if err != nil {
			return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errComputeArchiveSizeFailed, map[string]string{detailCause: err.Error()})
		}

		emitProgress(report, "finalizing", 98, "finalizing archive")
		return createArchiveRes{
			InputPath:    inputPathForManifest,
			InputPaths:   inputPathsAbs,
			Format:       format,
			OutputPath:   outputPath,
			OutputFiles:  outputFiles,
			InputIsDir:   inputIsDir,
			InputBytes:   inputBytes,
			ArchiveBytes: archiveBytes,
			ChunkSizeKB:  chunkSize,
			SplitSizeMB:  req.SplitSizeMB,
		}, nil
	}

	workDir, createdTemp, err := resolveWorkDir(req.WorkDir)
	if err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errCreateWorkDirFailed, map[string]string{detailCause: err.Error()})
	}
	if createdTemp && !req.KeepWorkDir {
		defer os.RemoveAll(workDir)
	}

	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return createArchiveRes{}, rerr
	}

	dbPath := filepath.Join(workDir, manifestDbRelativePath)

	key, err := util.HashPassword(req.Password)
	if err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errDeriveKeyFailed, map[string]string{detailCause: err.Error()})
	}

	// Reuse existing DUES store pipeline for dedup+compress USP.
	emitProgress(report, "ingesting", 35, "ingesting input into dedup store")
	if err := ensureStoreRuntime(); err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errStoreRuntimeInitFailed, map[string]string{detailCause: err.Error()})
	}
	if err := cli.StoreDataWithContext(ctx, chunkSize, dbPath, storeInputPath, key, false, true, false, false); err != nil {
		if errors.Is(err, context.Canceled) {
			return createArchiveRes{}, canceledError()
		}
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errStorePipelineFailed, map[string]string{detailCause: err.Error()})
	}
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return createArchiveRes{}, rerr
	}
	emitProgress(report, "writing_manifest", 72, "writing archive manifest")

	manifest := archiveManifest{
		Version:      manifestVersionV1,
		CreatedAtUTC: time.Now().UTC().Format(time.RFC3339),
		InputPath:    inputPathForManifest,
		InputPaths:   inputPathsAbs,
		InputIsDir:   inputIsDir,
		InputBytes:   inputBytes,
		ChunkSizeKB:  chunkSize,
		Pipeline:     manifestPipelineStore,
		DbPath:       manifestDbRelativePath,
	}
	manifestPath := filepath.Join(workDir, manifestFileName)
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errManifestEncodeFailed, map[string]string{detailCause: err.Error()})
	}
	if err := os.WriteFile(manifestPath, manifestBytes, defaultFilePerm); err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errManifestWriteFailed, map[string]string{detailCause: err.Error()})
	}

	emitProgress(report, "packaging", 82, "packaging archive")
	totalZipBytes, _ := measureInputBytes(workDir)
	if err := zipDirectoryWithProgressCtx(ctx, workDir, outputPath, func(processed, total int64) {
		if total <= 0 {
			total = totalZipBytes
		}
		p := progressFromBytes(processed, total, 82, 97)
		emitProgress(report, "packaging", p, "packaging archive")
	}); err != nil {
		if errors.Is(err, context.Canceled) {
			return createArchiveRes{}, canceledError()
		}
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errArchivePackagingFailed, map[string]string{detailCause: err.Error()})
	}

	outputFiles := []string{outputPath}
	if req.SplitSizeMB > 0 {
		emitProgress(report, "splitting", 92, "creating split volumes")
		parts, serr := splitArchiveCtx(ctx, outputPath, req.SplitSizeMB)
		if serr != nil {
			if errors.Is(serr, context.Canceled) {
				return createArchiveRes{}, canceledError()
			}
			return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errSplitVolumeCreationFailed, map[string]string{detailCause: serr.Error()})
		}
		outputFiles = parts
	}

	archiveBytes, err := sumFileSizes(outputFiles)
	if err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errComputeArchiveSizeFailed, map[string]string{detailCause: err.Error()})
	}

	res := createArchiveRes{
		InputPath:    inputPathForManifest,
		InputPaths:   inputPathsAbs,
		Format:       archiveFormatDuesarc,
		OutputPath:   outputPath,
		OutputFiles:  outputFiles,
		InputIsDir:   inputIsDir,
		InputBytes:   inputBytes,
		ArchiveBytes: archiveBytes,
		ChunkSizeKB:  chunkSize,
		SplitSizeMB:  req.SplitSizeMB,
	}
	if req.KeepWorkDir || req.WorkDir != "" {
		res.WorkDir = workDir
	}

	emitProgress(report, "finalizing", 98, "finalizing archive")
	return res, nil
}

func (pt *progressTracker) start(operation string) string {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.pruneLocked(time.Now())

	pt.seq++
	now := time.Now().UTC()
	taskID := fmt.Sprintf("cp-%d-%d", now.UnixNano(), pt.seq)
	pt.tasks[taskID] = &progressTask{
		TaskID:    taskID,
		Operation: operation,
		Status:    statusQueued,
		Percent:   0,
		StartedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(progressTaskTTL),
	}

	return taskID
}

func (pt *progressTracker) attachCancel(taskID string, cancel context.CancelFunc) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if t, ok := pt.tasks[taskID]; ok {
		t.Cancel = cancel
	}
}

func (pt *progressTracker) get(taskID string) (progressRes, bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.pruneLocked(time.Now())

	t, ok := pt.tasks[taskID]
	if !ok {
		return progressRes{}, false
	}

	res := progressRes{
		TaskID:       t.TaskID,
		Operation:    t.Operation,
		Status:       t.Status,
		Percent:      t.Percent,
		Stage:        t.Stage,
		Message:      t.Message,
		Error:        t.Error,
		ErrorDetails: t.ErrorDetails,
		Result:       t.Result,
		StartedAtUTC: t.StartedAt.Format(time.RFC3339),
		UpdatedAtUTC: t.UpdatedAt.Format(time.RFC3339),
	}
	if !t.CompletedAt.IsZero() {
		res.CompletedAtUTC = t.CompletedAt.Format(time.RFC3339)
	}

	return res, true
}

func (pt *progressTracker) update(taskID, status string, percent int, stage, message string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	t, ok := pt.tasks[taskID]
	if !ok {
		return
	}
	if status == "" {
		status = t.Status
	}
	if t.Status == statusCompleted || t.Status == statusFailed || t.Status == statusCanceled {
		return
	}
	if status != statusCompleted && status != statusFailed {
		if percent < t.Percent {
			percent = t.Percent
		}
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	t.Status = status
	t.Percent = percent
	t.Stage = stage
	t.Message = message
	t.UpdatedAt = time.Now().UTC()
	t.ExpiresAt = t.UpdatedAt.Add(progressTaskTTL)
}

func (pt *progressTracker) complete(taskID string, result any) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	t, ok := pt.tasks[taskID]
	if !ok {
		return
	}
	if t.Status == statusCanceled {
		return
	}
	now := time.Now().UTC()
	t.Status = statusCompleted
	t.Percent = 100
	t.Stage = "completed"
	t.Message = "operation completed"
	t.Result = result
	t.UpdatedAt = now
	t.CompletedAt = now
	t.ExpiresAt = now.Add(progressTaskTTL)
	t.Cancel = nil
}

func (pt *progressTracker) fail(taskID, message string, details map[string]any) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	t, ok := pt.tasks[taskID]
	if !ok {
		return
	}
	if t.Status == statusCanceled {
		return
	}
	now := time.Now().UTC()
	t.Status = statusFailed
	if t.Percent > 99 {
		t.Percent = 99
	}
	t.Stage = "failed"
	t.Message = "operation failed"
	t.Error = message
	t.ErrorDetails = details
	t.UpdatedAt = now
	t.CompletedAt = now
	t.ExpiresAt = now.Add(progressTaskTTL)
	t.Cancel = nil
}

func (pt *progressTracker) markCanceled(taskID string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	t, ok := pt.tasks[taskID]
	if !ok {
		return
	}
	if t.Status == statusCompleted || t.Status == statusFailed || t.Status == statusCanceled {
		return
	}
	now := time.Now().UTC()
	t.Status = statusCanceled
	t.Stage = "canceled"
	t.Message = "operation canceled"
	t.Error = errTaskCanceled
	t.UpdatedAt = now
	t.CompletedAt = now
	t.ExpiresAt = now.Add(progressTaskTTL)
	t.Cancel = nil
}

func (pt *progressTracker) cancel(taskID string) (cancelTaskRes, *coreerr.Error) {
	pt.mu.Lock()
	t, ok := pt.tasks[taskID]
	if !ok {
		pt.mu.Unlock()
		return cancelTaskRes{}, coreerr.New(coreerr.CodeInvalidRequest, errProgressTaskNotFound)
	}
	if t.Status == statusCompleted || t.Status == statusFailed || t.Status == statusCanceled {
		res := cancelTaskRes{TaskID: taskID, Status: t.Status, Message: errTaskNotCancelable}
		pt.mu.Unlock()
		return res, nil
	}
	cancel := t.Cancel
	pt.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	pt.markCanceled(taskID)
	return cancelTaskRes{TaskID: taskID, Status: statusCanceled}, nil
}

func (pt *progressTracker) pruneLocked(now time.Time) {
	for id, task := range pt.tasks {
		if now.After(task.ExpiresAt) {
			delete(pt.tasks, id)
		}
	}
}

func resolveArchiveInputs(req createArchiveReq) (storeInputPath string, inputPathForManifest string, inputPathsAbs []string, inputIsDir bool, inputBytes int64, cleanup func(), rerr *coreerr.Error) {
	return resolveArchiveInputsCtx(context.Background(), req)
}

func resolveArchiveInputsCtx(ctx context.Context, req createArchiveReq) (storeInputPath string, inputPathForManifest string, inputPathsAbs []string, inputIsDir bool, inputBytes int64, cleanup func(), rerr *coreerr.Error) {
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return "", "", nil, false, 0, nil, rerr
	}
	if len(req.InputPaths) > 0 {
		paths, totalBytes, err := normalizeFileInputsCtx(ctx, req.InputPaths)
		if err != nil {
			return "", "", nil, false, 0, nil, err
		}
		stagingRoot, stageErr := stageMultiFileInputsCtx(ctx, paths)
		if stageErr != nil {
			if errors.Is(stageErr, context.Canceled) {
				return "", "", nil, false, 0, nil, canceledError()
			}
			return "", "", nil, false, 0, nil, coreerr.NewWithDetails(coreerr.CodeInternal, errCreateWorkDirFailed, map[string]string{detailCause: stageErr.Error()})
		}
		return stagingRoot, stagingRoot, paths, true, totalBytes, func() { _ = os.RemoveAll(stagingRoot) }, nil
	}

	inPathAbs, err := filepath.Abs(req.InputPath)
	if err != nil {
		return "", "", nil, false, 0, nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
	}
	info, err := os.Stat(inPathAbs)
	if err != nil {
		return "", "", nil, false, 0, nil, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInputPathStatFailed, map[string]string{detailCause: err.Error(), detailInputPath: inPathAbs})
	}
	if rerr := ensureNotCanceled(ctx); rerr != nil {
		return "", "", nil, false, 0, nil, rerr
	}
	totalBytes, err := measureInputBytes(inPathAbs)
	if err != nil {
		return "", "", nil, false, 0, nil, coreerr.NewWithDetails(coreerr.CodeInternal, errFailedToMeasureInput, map[string]string{detailCause: err.Error()})
	}
	return inPathAbs, inPathAbs, []string{inPathAbs}, info.IsDir(), totalBytes, nil, nil
}

func normalizeFileInputs(paths []string) ([]string, int64, *coreerr.Error) {
	return normalizeFileInputsCtx(context.Background(), paths)
}

func normalizeFileInputsCtx(ctx context.Context, paths []string) ([]string, int64, *coreerr.Error) {
	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	var totalBytes int64

	for _, p := range paths {
		if rerr := ensureNotCanceled(ctx); rerr != nil {
			return nil, 0, rerr
		}
		if strings.TrimSpace(p) == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, 0, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidInputPath, map[string]string{detailCause: err.Error()})
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, 0, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInputPathStatFailed, map[string]string{detailCause: err.Error(), detailInputPath: abs})
		}
		if info.IsDir() {
			return nil, 0, coreerr.New(coreerr.CodeInvalidRequest, errInputPathsFilesOnly)
		}
		normalized = append(normalized, abs)
		seen[abs] = struct{}{}
		totalBytes += info.Size()
	}

	if len(normalized) == 0 {
		return nil, 0, coreerr.New(coreerr.CodeInvalidRequest, errInputPathRequired)
	}

	return normalized, totalBytes, nil
}

func stageMultiFileInputs(inputFiles []string) (string, error) {
	return stageMultiFileInputsCtx(context.Background(), inputFiles)
}

func stageMultiFileInputsCtx(ctx context.Context, inputFiles []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stagingRoot, err := os.MkdirTemp("", "duesarc-multi-input-*")
	if err != nil {
		return "", err
	}

	commonRoot := commonDirPath(inputFiles)
	usedTargets := make(map[string]struct{}, len(inputFiles))
	for idx, src := range inputFiles {
		if err := ctx.Err(); err != nil {
			_ = os.RemoveAll(stagingRoot)
			return "", err
		}
		rel, relErr := filepath.Rel(commonRoot, src)
		if relErr != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			rel = filepath.Join(fmt.Sprintf("input_%03d", idx+1), filepath.Base(src))
		}
		rel = strings.TrimLeft(filepath.Clean(rel), `\\/`)
		if rel == "" || rel == "." {
			rel = filepath.Join(fmt.Sprintf("input_%03d", idx+1), filepath.Base(src))
		}

		targetRel := rel
		if _, exists := usedTargets[targetRel]; exists {
			targetRel = filepath.Join(fmt.Sprintf("input_%03d", idx+1), filepath.Base(src))
		}
		usedTargets[targetRel] = struct{}{}

		dst := filepath.Join(stagingRoot, targetRel)
		if err := os.MkdirAll(filepath.Dir(dst), defaultDirPerm); err != nil {
			_ = os.RemoveAll(stagingRoot)
			return "", err
		}
		if err := copyFileCtx(ctx, src, dst); err != nil {
			_ = os.RemoveAll(stagingRoot)
			return "", err
		}
	}

	return stagingRoot, nil
}

func commonDirPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	root := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		next := filepath.Dir(p)
		for {
			rel, err := filepath.Rel(root, next)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return root
			}
			root = parent
		}
	}
	return root
}

func copyFile(src, dst string) error {
	return copyFileCtx(context.Background(), src, dst)
}

func copyFileCtx(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := copyWithProgress(ctx, out, in, nil); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return nil
}

type base64Params interface {
	getDataBase64() string
}

func (r *compressReq) getDataBase64() string   { return r.DataBase64 }
func (r *decompressReq) getDataBase64() string { return r.DataBase64 }

func decodeBase64Params[T base64Params](params json.RawMessage, req T) ([]byte, *coreerr.Error) {
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

func defaultDecompressOutput(inputPath string) string {
	if before, ok := strings.CutSuffix(inputPath, compressedFileExt); ok {
		return before
	}
	if before, ok := strings.CutSuffix(inputPath, zstdExt); ok {
		return before
	}
	ext := filepath.Ext(inputPath)
	if ext == "" {
		return inputPath + outExt
	}
	return strings.TrimSuffix(inputPath, ext) + outExt
}

func defaultExtractOutput(inputPath string) string {
	if strings.HasSuffix(strings.ToLower(inputPath), archiveExt) {
		return strings.TrimSuffix(inputPath, archiveExt)
	}
	if strings.HasSuffix(strings.ToLower(inputPath), archiveZipExt) {
		return strings.TrimSuffix(inputPath, archiveZipExt)
	}
	if strings.HasSuffix(strings.ToLower(inputPath), archiveTgzExt) {
		return strings.TrimSuffix(inputPath, archiveTgzExt)
	}
	if strings.HasSuffix(strings.ToLower(inputPath), ".tgz") {
		return strings.TrimSuffix(inputPath, ".tgz")
	}
	if strings.HasSuffix(strings.ToLower(inputPath), archiveTxzExt) {
		return strings.TrimSuffix(inputPath, archiveTxzExt)
	}
	if strings.HasSuffix(strings.ToLower(inputPath), ".txz") {
		return strings.TrimSuffix(inputPath, ".txz")
	}
	if strings.HasSuffix(strings.ToLower(inputPath), ".xz") {
		return strings.TrimSuffix(inputPath, ".xz")
	}
	if strings.HasSuffix(strings.ToLower(inputPath), archiveTarExt) {
		return strings.TrimSuffix(inputPath, archiveTarExt)
	}
	return inputPath + outExt
}

func archiveExtensionForFormat(format string) string {
	switch format {
	case archiveFormatDuesarc:
		return archiveExt
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
	f := strings.ToLower(strings.TrimSpace(format))
	switch f {
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

func detectArchiveFormatFromPath(path string) string {
	lower := strings.ToLower(path)
	if isSplitVolumePath(lower) {
		lower = strings.TrimSuffix(lower, filepath.Ext(lower))
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

func resolveArchiveFormat(requested, path string) (string, *coreerr.Error) {
	f := normalizeArchiveFormat(requested)
	if f == "" && strings.TrimSpace(requested) != "" {
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: requested})
	}
	if f != "" {
		return f, nil
	}

	detected := detectArchiveFormatFromPath(path)
	if detected != "" {
		return detected, nil
	}

	// No explicit format and no inferable extension:
	// - For create flows with empty output path, default to duesarc.
	// - For explicit unknown extensions, reject instead of silently using duesarc.
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return archiveFormatDuesarc, nil
	}

	lower := strings.ToLower(trimmedPath)
	if isSplitVolumePath(lower) {
		lower = strings.TrimSuffix(lower, filepath.Ext(lower))
	}
	if ext := filepath.Ext(lower); ext != "" {
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: trimmedPath})
	}

	return archiveFormatDuesarc, nil
}

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
		return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: err.Error()})
	}
	if detected != "" {
		return detected, nil
	}

	return "", coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errUnsupportedArchiveFormat, map[string]string{detailCause: inputPath})
}

func detectArchiveFormatFromFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, rerr := io.ReadFull(f, buf)
	if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
		return "", rerr
	}
	buf = buf[:n]

	if isZipSignature(buf) {
		zipFmt, zerr := detectZipContainerFormat(path)
		if zerr == nil && zipFmt != "" {
			return zipFmt, nil
		}
		return archiveFormatZip, nil
	}

	if isGzipSignature(buf) {
		return archiveFormatTarGz, nil
	}

	if isXzSignature(buf) {
		return archiveFormatTarXz, nil
	}

	if isTarHeader(buf) {
		return archiveFormatTar, nil
	}

	return "", nil
}

func detectZipContainerFormat(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()

	hasManifest := false
	hasDBPrefix := false
	for _, f := range zr.File {
		if f.Name == manifestFileName {
			hasManifest = true
		}
		if strings.HasPrefix(f.Name, manifestDbRelativePath+"/") {
			hasDBPrefix = true
		}
		if hasManifest && hasDBPrefix {
			return archiveFormatDuesarc, nil
		}
	}

	if hasManifest {
		for _, f := range zr.File {
			if f.Name != manifestFileName {
				continue
			}
			valid, vErr := validateManifest(zr, f)
			if vErr == nil && valid {
				return archiveFormatDuesarc, nil
			}
			break
		}
	}

	return archiveFormatZip, nil
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
	if len(buf) < 6 {
		return false
	}
	return bytes.Equal(buf[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0x00})
}

func isTarHeader(buf []byte) bool {
	if len(buf) < 262 {
		return false
	}
	return bytes.Equal(buf[257:262], []byte("ustar"))
}

func resolveWorkDir(workDir string) (string, bool, error) {
	if workDir != "" {
		abs, err := filepath.Abs(workDir)
		if err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(abs, defaultDirPerm); err != nil {
			return "", false, err
		}
		return abs, false, nil
	}
	tmp, err := os.MkdirTemp("", tempWorkDirPattern)
	if err != nil {
		return "", false, err
	}
	return tmp, true, nil
}

func measureInputBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}

	var total int64
	err = filepath.WalkDir(path, func(curr string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		f, err := d.Info()
		if err != nil {
			return err
		}
		total += f.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func zipDirectory(sourceDir, outputPath string) error {
	return zipDirectoryWithProgressCtx(context.Background(), sourceDir, outputPath, nil)
}

func createCompatibilityArchiveCtx(ctx context.Context, inputPath, outputPath, format string, onBytes func(processed, total int64)) error {
	switch format {
	case archiveFormatZip:
		return zipPathWithProgressCtx(ctx, inputPath, outputPath, onBytes)
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		return tarPathWithProgressCtx(ctx, inputPath, outputPath, format, onBytes)
	default:
		return fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
	}
}

func listCompatibilityArchiveEntriesCtx(ctx context.Context, inputPath, format string, onProgress func(processed, total int64)) ([]archiveEntry, error) {
	switch format {
	case archiveFormatZip:
		zr, cleanup, err := openArchiveReader(inputPath)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		total := len(zr.File)
		entries := make([]archiveEntry, 0, total)
		for idx, f := range zr.File {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			entries = append(entries, archiveEntry{
				Name:            f.Name,
				CompressedBytes: f.CompressedSize64,
				OriginalBytes:   f.UncompressedSize64,
				IsDir:           f.FileInfo().IsDir(),
			})
			if onProgress != nil {
				onProgress(int64(idx+1), int64(total))
			}
		}
		return entries, nil
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		totalEntries, _, err := measureTarArchiveCtx(ctx, inputPath, format)
		if err != nil {
			return nil, err
		}

		rc, err := openTarReadStream(inputPath, format)
		if err != nil {
			return nil, err
		}
		defer rc.Close()

		tr := tar.NewReader(rc)
		entries := make([]archiveEntry, 0, totalEntries)
		processed := 0
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if hdr == nil {
				continue
			}
			entries = append(entries, archiveEntry{
				Name:            hdr.Name,
				CompressedBytes: uint64(maxInt64(hdr.Size, 0)),
				OriginalBytes:   uint64(maxInt64(hdr.Size, 0)),
				IsDir:           hdr.FileInfo().IsDir(),
			})
			processed++
			if onProgress != nil {
				onProgress(int64(processed), int64(totalEntries))
			}
		}
		return entries, nil
	default:
		return nil, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
	}
}

func verifyCompatibilityArchiveCtx(ctx context.Context, inputPath, format string, onProgress func(processed, total int64)) (archiveVerifyStats, error) {
	switch format {
	case archiveFormatZip:
		zr, cleanup, err := openArchiveReader(inputPath)
		if err != nil {
			return archiveVerifyStats{}, err
		}
		defer cleanup()

		var totalExpected int64
		for _, f := range zr.File {
			totalExpected += int64(f.UncompressedSize64)
		}

		var verified int64
		fileCount := 0
		dirCount := 0
		for _, f := range zr.File {
			if err := ctx.Err(); err != nil {
				return archiveVerifyStats{}, err
			}
			rc, err := f.Open()
			if err != nil {
				return archiveVerifyStats{}, err
			}
			n, copyErr := copyWithProgress(ctx, io.Discard, rc, nil)
			closeErr := rc.Close()
			if copyErr != nil {
				return archiveVerifyStats{}, copyErr
			}
			if closeErr != nil {
				return archiveVerifyStats{}, closeErr
			}
			verified += n
			if f.FileInfo().IsDir() {
				dirCount++
			} else {
				fileCount++
			}
			if onProgress != nil {
				onProgress(verified, totalExpected)
			}
		}
		return archiveVerifyStats{EntryCount: len(zr.File), FileCount: fileCount, DirCount: dirCount, VerifiedBytes: verified}, nil
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		totalEntries, totalBytes, err := measureTarArchiveCtx(ctx, inputPath, format)
		if err != nil {
			return archiveVerifyStats{}, err
		}

		rc, err := openTarReadStream(inputPath, format)
		if err != nil {
			return archiveVerifyStats{}, err
		}
		defer rc.Close()

		tr := tar.NewReader(rc)
		entries := 0
		var verified int64
		fileCount := 0
		dirCount := 0
		for {
			if err := ctx.Err(); err != nil {
				return archiveVerifyStats{}, err
			}
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return archiveVerifyStats{}, err
			}
			if hdr == nil {
				continue
			}
			entries++
			if hdr.FileInfo().IsDir() {
				dirCount++
			} else {
				fileCount++
			}
			if hdr.FileInfo().Mode().IsRegular() {
				n, copyErr := copyWithProgress(ctx, io.Discard, tr, nil)
				if copyErr != nil {
					return archiveVerifyStats{}, copyErr
				}
				verified += n
			}
			if onProgress != nil {
				total := totalBytes
				if total <= 0 {
					total = int64(totalEntries)
					onProgress(int64(entries), total)
				} else {
					onProgress(verified, total)
				}
			}
		}
		return archiveVerifyStats{EntryCount: entries, FileCount: fileCount, DirCount: dirCount, VerifiedBytes: verified}, nil
	default:
		return archiveVerifyStats{}, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
	}
}

func measureArchiveInputBytes(inputPath string) (int64, error) {
	if !isSplitVolumePath(inputPath) {
		info, err := os.Stat(inputPath)
		if err != nil {
			return 0, err
		}
		return info.Size(), nil
	}

	base := strings.TrimSuffix(inputPath, filepath.Ext(inputPath))
	total := int64(0)
	parts := make([]string, 0, 8)
	for idx := 1; ; idx++ {
		partPath := fmt.Sprintf("%s"+archiveSplitSuffixFmt, base, idx)
		info, err := os.Stat(partPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			return 0, err
		}
		total += info.Size()
		parts = append(parts, partPath)
	}
	if len(parts) == 0 {
		return 0, fmt.Errorf("%s", errOpenArchiveFailed)
	}
	return total, nil
}

func readManifestFromZipFile(manifestFile *zip.File) (archiveManifest, error) {
	rc, err := manifestFile.Open()
	if err != nil {
		return archiveManifest{}, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return archiveManifest{}, err
	}

	var manifest archiveManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return archiveManifest{}, err
	}

	return manifest, nil
}

func extractCompatibilityArchiveCtx(ctx context.Context, inputPath, outputPath, format string, onBytes func(processed, total int64)) (int, error) {
	switch format {
	case archiveFormatZip:
		return extractArchiveToDirWithProgressCtx(ctx, inputPath, outputPath, onBytes)
	case archiveFormatTar, archiveFormatTarGz, archiveFormatTarXz:
		_, totalBytes, err := measureTarArchiveCtx(ctx, inputPath, format)
		if err != nil {
			return 0, err
		}

		rc, err := openTarReadStream(inputPath, format)
		if err != nil {
			return 0, err
		}
		defer rc.Close()

		tr := tar.NewReader(rc)
		var processed int64
		extracted := 0
		for {
			if err := ctx.Err(); err != nil {
				return extracted, err
			}
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return extracted, err
			}
			if hdr == nil {
				continue
			}

			targetPath, err := safeArchiveJoin(outputPath, hdr.Name)
			if err != nil {
				return extracted, err
			}

			if hdr.FileInfo().IsDir() {
				if err := os.MkdirAll(targetPath, defaultDirPerm); err != nil {
					return extracted, err
				}
				continue
			}
			if !hdr.FileInfo().Mode().IsRegular() {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(targetPath), defaultDirPerm); err != nil {
				return extracted, err
			}
			out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, defaultFilePerm)
			if err != nil {
				return extracted, err
			}
			_, copyErr := copyWithProgress(ctx, out, tr, func(n int64) {
				processed += n
				if onBytes != nil {
					onBytes(processed, totalBytes)
				}
			})
			closeErr := out.Close()
			if copyErr != nil {
				return extracted, copyErr
			}
			if closeErr != nil {
				return extracted, closeErr
			}
			extracted++
		}
		return extracted, nil
	default:
		return 0, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
	}
}

func zipPathWithProgressCtx(ctx context.Context, inputPath, outputPath string, onBytes func(processed, total int64)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	totalBytes, _ := measureInputBytes(inputPath)
	var processed int64

	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	zw := zip.NewWriter(outFile)
	defer zw.Close()

	return walkArchiveInputCtx(ctx, inputPath, func(absPath, relPath string, info os.FileInfo) error {
		zipPath := filepath.ToSlash(relPath)
		if info.IsDir() {
			_, err := zw.Create(zipPath + "/")
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = zipPath
		header.Method = zip.Deflate
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(absPath)
		if err != nil {
			return err
		}
		_, copyErr := copyWithProgress(ctx, writer, src, func(n int64) {
			processed += n
			if onBytes != nil {
				onBytes(processed, totalBytes)
			}
		})
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func tarPathWithProgressCtx(ctx context.Context, inputPath, outputPath, format string, onBytes func(processed, total int64)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	totalBytes, _ := measureInputBytes(inputPath)
	var processed int64

	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	var writeCloser io.WriteCloser = outFile
	switch format {
	case archiveFormatTar:
		// no compression layer
	case archiveFormatTarGz:
		gz := gzip.NewWriter(outFile)
		writeCloser = gz
		defer gz.Close()
	case archiveFormatTarXz:
		xzw, err := xz.NewWriter(outFile)
		if err != nil {
			return err
		}
		writeCloser = xzw
		defer xzw.Close()
	default:
		return fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
	}

	tw := tar.NewWriter(writeCloser)
	defer tw.Close()

	return walkArchiveInputCtx(ctx, inputPath, func(absPath, relPath string, info os.FileInfo) error {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)
		if info.IsDir() {
			return tw.WriteHeader(header)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		src, err := os.Open(absPath)
		if err != nil {
			return err
		}
		_, copyErr := copyWithProgress(ctx, tw, src, func(n int64) {
			processed += n
			if onBytes != nil {
				onBytes(processed, totalBytes)
			}
		})
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func walkArchiveInputCtx(ctx context.Context, inputPath string, fn func(absPath, relPath string, info os.FileInfo) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	info, err := os.Stat(inputPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fn(inputPath, filepath.Base(inputPath), info)
	}

	return filepath.WalkDir(inputPath, func(curr string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if curr == inputPath {
			return nil
		}
		rel, err := filepath.Rel(inputPath, curr)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(curr, rel, info)
	})
}

type streamReadCloser struct {
	io.Reader
	closeFn func() error
}

func (s *streamReadCloser) Close() error {
	if s.closeFn == nil {
		return nil
	}
	return s.closeFn()
}

func openTarReadStream(inputPath, format string) (io.ReadCloser, error) {
	archivePath := inputPath
	cleanupPath := ""
	if isSplitVolumePath(inputPath) {
		assembledPath, err := assembleSplitArchive(inputPath)
		if err != nil {
			return nil, err
		}
		archivePath = assembledPath
		cleanupPath = assembledPath
	}

	file, err := os.Open(archivePath)
	if err != nil {
		if cleanupPath != "" {
			_ = os.Remove(cleanupPath)
		}
		return nil, err
	}

	reader := io.Reader(file)
	closers := make([]io.Closer, 0, 2)
	closers = append(closers, file)

	switch format {
	case archiveFormatTar:
		// no compression layer
	case archiveFormatTarGz:
		gz, err := gzip.NewReader(file)
		if err != nil {
			_ = file.Close()
			if cleanupPath != "" {
				_ = os.Remove(cleanupPath)
			}
			return nil, err
		}
		reader = gz
		closers = append([]io.Closer{gz}, closers...)
	case archiveFormatTarXz:
		xzr, err := xz.NewReader(file)
		if err != nil {
			_ = file.Close()
			if cleanupPath != "" {
				_ = os.Remove(cleanupPath)
			}
			return nil, err
		}
		reader = xzr
	default:
		_ = file.Close()
		if cleanupPath != "" {
			_ = os.Remove(cleanupPath)
		}
		return nil, fmt.Errorf("%s: %s", errUnsupportedArchiveFormat, format)
	}

	return &streamReadCloser{
		Reader: reader,
		closeFn: func() error {
			var firstErr error
			for _, c := range closers {
				if err := c.Close(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			if cleanupPath != "" {
				if err := os.Remove(cleanupPath); err != nil && !os.IsNotExist(err) && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		},
	}, nil
}

func measureTarArchiveCtx(ctx context.Context, inputPath, format string) (int, int64, error) {
	rc, err := openTarReadStream(inputPath, format)
	if err != nil {
		return 0, 0, err
	}
	defer rc.Close()

	tr := tar.NewReader(rc)
	count := 0
	var totalBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return count, totalBytes, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, totalBytes, err
		}
		if hdr == nil {
			continue
		}
		count++
		if hdr.FileInfo().Mode().IsRegular() {
			totalBytes += hdr.Size
		}
	}
	return count, totalBytes, nil
}

func safeArchiveJoin(root, name string) (string, error) {
	rel := strings.TrimLeft(filepath.FromSlash(name), `\\/`)
	if rel == "" || rel == "." {
		return "", fmt.Errorf(errPathTraversalDetected)
	}
	target := filepath.Join(root, rel)
	cleanTarget := filepath.Clean(target)
	cleanRoot := filepath.Clean(root)
	rootPrefix := cleanRoot + string(os.PathSeparator)
	if cleanTarget != cleanRoot && !strings.HasPrefix(cleanTarget, rootPrefix) {
		return "", fmt.Errorf(errPathTraversalDetected)
	}
	return cleanTarget, nil
}

func maxInt64(v, min int64) int64 {
	if v < min {
		return min
	}
	return v
}

func zipDirectoryWithProgressCtx(ctx context.Context, sourceDir, outputPath string, onBytes func(processed, total int64)) error {
	if ctx == nil {
		ctx = context.Background()
	}

	totalBytes, _ := measureInputBytes(sourceDir)
	var processedBytes int64

	outFile, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	zw := zip.NewWriter(outFile)
	defer zw.Close()

	return filepath.WalkDir(sourceDir, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == sourceDir {
			return nil
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		zipPath := filepath.ToSlash(rel)
		if d.IsDir() {
			_, err = zw.Create(zipPath + "/")
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = zipPath
		header.Method = zip.Deflate

		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}

		_, copyErr := copyWithProgress(ctx, writer, src, func(n int64) {
			processedBytes += n
			if onBytes != nil {
				onBytes(processedBytes, totalBytes)
			}
		})
		closeErr := src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}

		if onBytes != nil {
			onBytes(processedBytes, totalBytes)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	})
}

func splitArchive(path string, splitSizeMB int) ([]string, error) {
	return splitArchiveCtx(context.Background(), path, splitSizeMB)
}

func splitArchiveCtx(ctx context.Context, path string, splitSizeMB int) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if splitSizeMB <= 0 {
		return []string{path}, nil
	}
	maxPartBytes := int64(splitSizeMB) * oneMiBBytes
	if maxPartBytes <= 0 {
		return nil, fmt.Errorf(errInvalidSplitSizeFmt, splitSizeMB)
	}

	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()

	buf := make([]byte, oneMiBBytes)
	partIndex := 1
	var outputs []string

	for {
		if err := ctx.Err(); err != nil {
			for _, outPath := range outputs {
				_ = os.Remove(outPath)
			}
			return nil, err
		}
		partPath := fmt.Sprintf("%s"+archiveSplitSuffixFmt, path, partIndex)
		out, err := os.Create(partPath)
		if err != nil {
			return nil, err
		}

		written := int64(0)
		for written < maxPartBytes {
			if err := ctx.Err(); err != nil {
				_ = out.Close()
				_ = os.Remove(partPath)
				for _, outPath := range outputs {
					_ = os.Remove(outPath)
				}
				return nil, err
			}
			remaining := maxPartBytes - written
			readSize := int64(len(buf))
			if remaining < readSize {
				readSize = remaining
			}
			n, rerr := in.Read(buf[:readSize])
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					out.Close()
					return nil, werr
				}
				written += int64(n)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				out.Close()
				return nil, rerr
			}
		}

		if err := out.Close(); err != nil {
			return nil, err
		}
		if written == 0 {
			_ = os.Remove(partPath)
			break
		}
		outputs = append(outputs, partPath)
		if written < maxPartBytes {
			break
		}
		partIndex++
	}

	if len(outputs) == 0 {
		return nil, fmt.Errorf(errNoSplitOutputGenerated)
	}
	if err := in.Close(); err != nil {
		return nil, err
	}

	if err := os.Remove(path); err != nil {
		return nil, err
	}

	return outputs, nil
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

func extractArchiveToDir(archivePath, outputPath string) (int, error) {
	return extractArchiveToDirWithProgressCtx(context.Background(), archivePath, outputPath, nil)
}

func extractArchiveToDirWithProgressCtx(ctx context.Context, archivePath, outputPath string, onBytes func(processed, total int64)) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	var totalBytes int64
	for _, file := range zr.File {
		if !file.FileInfo().IsDir() {
			totalBytes += int64(file.UncompressedSize64)
		}
	}
	var processedBytes int64

	count := 0
	for _, file := range zr.File {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		targetPath := filepath.Join(outputPath, filepath.FromSlash(file.Name))
		cleanOutputRoot := filepath.Clean(outputPath) + string(os.PathSeparator)
		if !strings.HasPrefix(filepath.Clean(targetPath), cleanOutputRoot) {
			return 0, fmt.Errorf(errPathTraversalDetected)
		}

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, defaultDirPerm); err != nil {
				return 0, err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), defaultDirPerm); err != nil {
			return 0, err
		}

		src, err := file.Open()
		if err != nil {
			return 0, err
		}
		dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, defaultFilePerm)
		if err != nil {
			src.Close()
			return 0, err
		}
		_, copyErr := copyWithProgress(ctx, dst, src, func(n int64) {
			processedBytes += n
			if onBytes != nil {
				onBytes(processedBytes, totalBytes)
			}
		})
		if copyErr != nil {
			dst.Close()
			src.Close()
			return 0, copyErr
		}
		if err := dst.Close(); err != nil {
			src.Close()
			return 0, err
		}
		if err := src.Close(); err != nil {
			return 0, err
		}
		count++
	}

	return count, nil
}

func readArchiveManifest(path string) (archiveManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return archiveManifest{}, err
	}
	var manifest archiveManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return archiveManifest{}, err
	}
	if manifest.DbPath == "" {
		return archiveManifest{}, fmt.Errorf(errExtractDbPathMissing)
	}
	return manifest, nil
}

func restoreArchiveFiles(stagingDir, outputRoot string, manifest archiveManifest, password string) (int, error) {
	return restoreArchiveFilesCtx(context.Background(), stagingDir, outputRoot, manifest, password)
}

func restoreArchiveFilesCtx(ctx context.Context, stagingDir, outputRoot string, manifest archiveManifest, password string) (int, error) {
	return restoreArchiveFilesWithProgressCtx(ctx, stagingDir, outputRoot, manifest, password, nil)
}

func restoreArchiveFilesWithProgressCtx(ctx context.Context, stagingDir, outputRoot string, manifest archiveManifest, password string, onProgress func(processed, total, restored int)) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := ensureStoreRuntime(); err != nil {
		return 0, err
	}

	dbPath := filepath.Join(stagingDir, filepath.FromSlash(manifest.DbPath))
	if info, err := os.Stat(dbPath); err != nil || !info.IsDir() {
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf(errExtractDbPathMissing)
	}

	key, err := util.HashPassword(password)
	if err != nil {
		return 0, err
	}

	chunkSize := manifest.ChunkSizeKB
	if chunkSize <= 0 {
		chunkSize = defaultChunkSizeKB
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	db, _, err := cli.Common(chunkSize, dbPath, key)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	if err := remapArchiveChunkPathsCtx(ctx, db, util.BlobPath(dbPath)); err != nil {
		return 0, err
	}

	evidenceKeys, err := listEvidenceKeysCtx(ctx, db)
	if err != nil {
		return 0, err
	}

	total := len(evidenceKeys)
	processed := 0
	restored := 0
	if onProgress != nil {
		onProgress(processed, total, restored)
	}
	for _, eviKey := range evidenceKeys {
		if err := ctx.Err(); err != nil {
			return restored, err
		}
		processed++
		evidenceFile, err := dbio.GetEvidenceFile(eviKey, db)
		if err != nil {
			return restored, err
		}
		if !evidenceFile.Completed || evidenceFile.Failed {
			if onProgress != nil {
				onProgress(processed, total, restored)
			}
			continue
		}

		rawID := dbio.EvidenceRawID(eviKey)
		hashB64 := base64.StdEncoding.EncodeToString(rawID)
		fallbackName := safeFallbackName(hashB64)
		targetPath, err := resolveRestoredFilePath(outputRoot, manifest, evidenceFile.Name, fallbackName)
		if err != nil {
			return restored, err
		}
		if err := restoreEvidenceToPathCtx(ctx, hashB64, targetPath, db); err != nil {
			return restored, err
		}
		restored++
		if onProgress != nil {
			onProgress(processed, total, restored)
		}
	}

	return restored, nil
}

func listArchiveLogicalFiles(stagingDir string, manifest archiveManifest, password string) ([]archiveLogicalFile, error) {
	return listArchiveLogicalFilesCtx(context.Background(), stagingDir, manifest, password)
}

func listArchiveLogicalFilesCtx(ctx context.Context, stagingDir string, manifest archiveManifest, password string) ([]archiveLogicalFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureStoreRuntime(); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(stagingDir, filepath.FromSlash(manifest.DbPath))
	if info, err := os.Stat(dbPath); err != nil || !info.IsDir() {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(errExtractDbPathMissing)
	}

	key, err := util.HashPassword(password)
	if err != nil {
		return nil, err
	}

	chunkSize := manifest.ChunkSizeKB
	if chunkSize <= 0 {
		chunkSize = defaultChunkSizeKB
	}

	db, _, err := cli.Common(chunkSize, dbPath, key)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	evidenceKeys, err := listEvidenceKeysCtx(ctx, db)
	if err != nil {
		return nil, err
	}

	logicalFiles := make([]archiveLogicalFile, 0, len(evidenceKeys))
	for _, eviKey := range evidenceKeys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		evidenceFile, err := dbio.GetEvidenceFile(eviKey, db)
		if err != nil {
			return nil, err
		}
		compressedSize, err := computeLogicalFileCompressedSize(ctx, eviKey, evidenceFile, db)
		if err != nil {
			return nil, err
		}

		status := "pending"
		if evidenceFile.Completed {
			status = "completed"
		}
		if evidenceFile.Failed {
			status = "failed"
		}

		idB64 := base64.StdEncoding.EncodeToString(dbio.EvidenceRawID(eviKey))
		hash := evidenceFile.FileHash
		if hash == "" {
			hash = idB64
		}

		logicalFiles = append(logicalFiles, archiveLogicalFile{
			ID:             idB64,
			Hash:           hash,
			Name:           evidenceFile.Name,
			Size:           evidenceFile.Size,
			TotalSize:      evidenceFile.Size,
			CompressedSize: compressedSize,
			Completed:      evidenceFile.Completed,
			Failed:         evidenceFile.Failed,
			Status:         status,
		})
	}

	return logicalFiles, nil
}

func listEvidenceKeys(db *badger.DB) ([][]byte, error) {
	return listEvidenceKeysCtx(context.Background(), db)
}

func computeLogicalFileCompressedSize(ctx context.Context, eviKey []byte, evidenceFile structs.EvidenceFile, db *badger.DB) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	rawID := dbio.EvidenceRawID(eviKey)
	start := evidenceFile.Start
	size := evidenceFile.Size
	if size <= 0 {
		return 0, nil
	}

	var dbstart int64
	if start > 0 {
		dbstart = util.GetDBStartOffset(start)
	}
	end := start + size

	var totalStored int64
	for restoreIndex := dbstart; restoreIndex < end; restoreIndex += cnst.ChonkSize {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, rawID, cnst.DataSeperator, restoreIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return 0, err
		}
		chunkKey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		metadata, err := dbio.GetNode(chunkKey, db)
		if err != nil {
			return 0, err
		}
		if stored, ok := dbio.GetStoredChonkSealedSize(metadata); ok {
			totalStored += stored
			continue
		}
		chunkStored, err := dbio.GetChonkSize(restoreIndex, start, size, dbstart, end, chunkKey, db)
		if err != nil {
			return 0, err
		}
		totalStored += chunkStored
	}

	return totalStored, nil
}

func listDuearcLogicalFilesFromInputCtx(ctx context.Context, inputPath, password string) ([]archiveLogicalFile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	archivePath := inputPath
	cleanupPath := ""
	if isSplitVolumePath(inputPath) {
		assembledPath, err := assembleSplitArchive(inputPath)
		if err != nil {
			return nil, err
		}
		archivePath = assembledPath
		cleanupPath = assembledPath
	}
	if cleanupPath != "" {
		defer os.Remove(cleanupPath)
	}

	stagingDir, err := os.MkdirTemp("", tempExtractPattern)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stagingDir)

	if _, err := extractArchiveToDirWithProgressCtx(ctx, archivePath, stagingDir, nil); err != nil {
		return nil, err
	}

	manifest, err := readArchiveManifest(filepath.Join(stagingDir, manifestFileName))
	if err != nil {
		return nil, err
	}

	return listArchiveLogicalFilesCtx(ctx, stagingDir, manifest, password)
}

func listEvidenceKeysCtx(ctx context.Context, db *badger.DB) ([][]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return listKeysByPrefixCtx(ctx, db, []byte(cnst.EviFileNamespace))
}

func listKeysByPrefix(db *badger.DB, prefix []byte) ([][]byte, error) {
	return listKeysByPrefixCtx(context.Background(), db, prefix)
}

func listKeysByPrefixCtx(ctx context.Context, db *badger.DB, prefix []byte) ([][]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	keys := make([][]byte, 0)
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := it.Item()
			key := item.KeyCopy(nil)
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func remapArchiveChunkPaths(db *badger.DB, blobRoot string) error {
	return remapArchiveChunkPathsCtx(context.Background(), db, blobRoot)
}

func remapArchiveChunkPathsCtx(ctx context.Context, db *badger.DB, blobRoot string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	chunkKeys, err := listKeysByPrefixCtx(ctx, db, []byte(cnst.ChonkNamespace))
	if err != nil {
		return err
	}

	for _, key := range chunkKeys {
		if err := ctx.Err(); err != nil {
			return err
		}
		metadata, err := dbio.GetNode(key, db)
		if err != nil {
			return err
		}

		var chunkMeta structs.ChonkMetadata
		if err := msgpack.Unmarshal(metadata, &chunkMeta); err != nil {
			return err
		}
		if chunkMeta.Path == "" {
			continue
		}
		if _, err := os.Stat(chunkMeta.Path); err == nil {
			continue
		}

		candidatePath := filepath.Join(blobRoot, filepath.Base(chunkMeta.Path))
		if _, err := os.Stat(candidatePath); err != nil {
			continue
		}

		chunkMeta.Path = candidatePath
		updated, err := msgpack.Marshal(chunkMeta)
		if err != nil {
			return err
		}
		if err := dbio.SetNode(key, updated, db); err != nil {
			return err
		}
	}

	return nil
}

func resolveRestoredFilePath(outputRoot string, manifest archiveManifest, originalName, fallbackName string) (string, error) {
	if fallbackName == "" {
		fallbackName = "restored"
	}

	cleanOriginal := filepath.Clean(originalName)
	if cleanOriginal == "." || cleanOriginal == "" {
		cleanOriginal = fallbackName
	}

	var relative string
	if manifest.InputIsDir {
		inputRoot := filepath.Clean(manifest.InputPath)
		rel, err := filepath.Rel(inputRoot, cleanOriginal)
		if err == nil && rel != "." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, "..") {
			relative = rel
		} else {
			relative = filepath.Base(cleanOriginal)
		}
	} else {
		relative = filepath.Base(manifest.InputPath)
		if relative == "" || relative == "." || relative == string(os.PathSeparator) {
			relative = filepath.Base(cleanOriginal)
		}
	}

	if relative == "" || relative == "." || relative == string(os.PathSeparator) {
		relative = fallbackName
	}

	if vol := filepath.VolumeName(relative); vol != "" {
		relative = strings.TrimPrefix(relative, vol)
	}
	relative = strings.TrimLeft(filepath.Clean(relative), "\\/")
	if relative == "" || relative == "." {
		relative = fallbackName
	}

	targetPath := filepath.Join(outputRoot, relative)
	cleanTarget := filepath.Clean(targetPath)
	cleanRoot := filepath.Clean(outputRoot)
	rootPrefix := cleanRoot + string(os.PathSeparator)
	if cleanTarget != cleanRoot && !strings.HasPrefix(cleanTarget, rootPrefix) {
		return "", fmt.Errorf(errPathTraversalDetected)
	}

	return cleanTarget, nil
}

func safeFallbackName(hashB64 string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", "+", "-", "=", "")
	fallback := replacer.Replace(hashB64)
	if fallback == "" {
		return "restored"
	}
	return fallback
}

func restoreEvidenceToPath(hashB64, targetPath string, db *badger.DB) (err error) {
	return restoreEvidenceToPathCtx(context.Background(), hashB64, targetPath, db)
}

func restoreEvidenceToPathCtx(ctx context.Context, hashB64, targetPath string, db *badger.DB) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), defaultDirPerm); err != nil {
		return err
	}

	tempFile, err := os.CreateTemp(filepath.Dir(targetPath), filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer func() {
		if tempFile != nil {
			_ = tempFile.Close()
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.Restore(hashB64, tempFile, db); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tempFile.Sync(); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	tempFile = nil

	return replaceFileAtomically(tempPath, targetPath)
}

func replaceFileAtomically(tempPath, targetPath string) error {
	if _, err := os.Stat(targetPath); err != nil {
		if os.IsNotExist(err) {
			return os.Rename(tempPath, targetPath)
		}
		return err
	}

	backupPath := targetPath + ".bak"
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(targetPath, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		if rollbackErr := os.Rename(backupPath, targetPath); rollbackErr != nil {
			return fmt.Errorf("move temp into place: %w; rollback failed: %v", err, rollbackErr)
		}
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	return nil
}

func openArchiveReader(inputPath string) (*zip.ReadCloser, func(), error) {
	archivePath := inputPath
	cleanupPath := ""
	if isSplitVolumePath(inputPath) {
		assembledPath, err := assembleSplitArchive(inputPath)
		if err != nil {
			return nil, func() {}, err
		}
		archivePath = assembledPath
		cleanupPath = assembledPath
	}

	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		if cleanupPath != "" {
			_ = os.Remove(cleanupPath)
		}
		return nil, func() {}, err
	}

	cleanup := func() {
		_ = zr.Close()
		if cleanupPath != "" {
			_ = os.Remove(cleanupPath)
		}
	}

	return zr, cleanup, nil
}

func validateManifest(zr *zip.ReadCloser, manifestFile *zip.File) (bool, error) {
	rc, err := manifestFile.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return false, err
	}

	var m archiveManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return false, nil
	}
	if m.Version != manifestVersionV1 {
		return false, nil
	}
	if m.DbPath == "" {
		return false, nil
	}

	dbPrefix := filepath.ToSlash(strings.TrimSuffix(m.DbPath, "/") + "/")
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, dbPrefix) {
			return true, nil
		}
	}

	return false, nil
}

func assembleSplitArchive(firstPartPath string) (string, error) {
	dir := filepath.Dir(firstPartPath)
	baseName := strings.TrimSuffix(firstPartPath, filepath.Ext(firstPartPath))
	tmpDir, err := os.MkdirTemp("", tempAssemblePattern)
	if err != nil {
		return "", err
	}
	assembled := filepath.Join(tmpDir, tempAssembledArchiveName)
	out, err := os.Create(assembled)
	if err != nil {
		return "", err
	}

	for idx := 1; ; idx++ {
		partPath := fmt.Sprintf("%s"+archiveSplitSuffixFmt, baseName, idx)
		if !filepath.IsAbs(partPath) {
			partPath = filepath.Join(dir, filepath.Base(partPath))
		}
		in, err := os.Open(partPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if idx == 1 {
					out.Close()
					return "", err
				}
				break
			}
			out.Close()
			return "", err
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			return "", err
		}
		if err := in.Close(); err != nil {
			out.Close()
			return "", err
		}
	}

	if err := out.Close(); err != nil {
		return "", err
	}
	return assembled, nil
}

func isSplitVolumePath(path string) bool {
	ext := filepath.Ext(path)
	if len(ext) != 4 || ext[0] != '.' {
		return false
	}
	for _, c := range ext[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

var storeRuntimeMu sync.Mutex

func ensureStoreRuntime() error {
	storeRuntimeMu.Lock()
	defer storeRuntimeMu.Unlock()

	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			return err
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encLevel := zstd.SpeedBestCompression
		switch strings.ToLower(cnst.CompressLevel) {
		case levelFast:
			encLevel = zstd.SpeedFastest
		case levelDefault:
			encLevel = zstd.SpeedDefault
		case levelBest, "":
			encLevel = zstd.SpeedBestCompression
		}
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(encLevel))
		if err != nil {
			return err
		}
		cnst.ENCODER = encoder
	}
	return nil
}
