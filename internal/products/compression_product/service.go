package compression_product

import (
	"archive/zip"
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
	"github.com/vmihailenco/msgpack/v5"
)

const Name = "compression_product"

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
	archiveSplitSuffixFmt = ".%03d"
	compressedFileExt     = ".dues.zst"
	zstdExt               = ".zst"
	outExt                = ".out"

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
}

type createArchiveReq struct {
	InputPath   string   `json:"input_path"`
	InputPaths  []string `json:"input_paths,omitempty"`
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
	ID        string `json:"id"`
	Hash      string `json:"hash"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Completed bool   `json:"completed"`
	Failed    bool   `json:"failed"`
	Status    string `json:"status"`
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
	InputPath     string `json:"input_path"`
	Valid         bool   `json:"valid"`
	EntryCount    int    `json:"entry_count"`
	ManifestFound bool   `json:"manifest_found"`
	ManifestValid bool   `json:"manifest_valid"`
	VerifiedBytes int64  `json:"verified_bytes"`
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
			Levels: []string{levelFast, levelDefault, levelBest},
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
	restoredCount, rerr := restoreArchiveFiles(stagingDir, extractPath, manifest, req.Password)
	if rerr != nil {
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

	emitProgress(report, "opening", 10, "opening archive")
	zr, cleanup, err := openArchiveReader(inputPath)
	if err != nil {
		return verifyArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errVerifyArchiveFailed, map[string]string{detailCause: err.Error()})
	}
	defer cleanup()

	manifestFound := false
	manifestValid := false
	var totalBytes int64
	var totalExpectedBytes int64
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

		if f.Name == manifestFileName {
			manifestFound = true
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

	emitProgress(report, "finalizing", 98, "finalizing verification")
	return verifyArchiveRes{
		InputPath:     inputPath,
		Valid:         true,
		EntryCount:    len(zr.File),
		ManifestFound: manifestFound,
		ManifestValid: manifestValid,
		VerifiedBytes: totalBytes,
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
	storeInputPath, inputPathForManifest, inputPathsAbs, inputIsDir, inputBytes, cleanupInput, cerr := resolveArchiveInputs(req)
	if cerr != nil {
		return createArchiveRes{}, cerr
	}
	if cleanupInput != nil {
		defer cleanupInput()
	}

	outputPath := req.OutputPath
	if outputPath == "" {
		base := filepath.Base(inputPathForManifest)
		if len(inputPathsAbs) > 1 {
			base = "bundle"
		}
		outputPath = base + archiveExt
	}
	if !strings.HasSuffix(strings.ToLower(outputPath), archiveExt) {
		outputPath += archiveExt
	}
	outputPathAbs, absErr := filepath.Abs(outputPath)
	if absErr != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, errInvalidOutputPath, map[string]string{detailCause: absErr.Error()})
	}
	outputPath = outputPathAbs
	if err := os.MkdirAll(filepath.Dir(outputPath), defaultDirPerm); err != nil {
		return createArchiveRes{}, coreerr.NewWithDetails(coreerr.CodeInternal, errCreateOutputDirFailed, map[string]string{detailCause: err.Error()})
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
	chunkSize := req.ChunkSizeKB
	if chunkSize <= 0 {
		chunkSize = defaultChunkSizeKB
	}

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
		parts, serr := splitArchive(outputPath, req.SplitSizeMB)
		if serr != nil {
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
	if len(req.InputPaths) > 0 {
		paths, totalBytes, err := normalizeFileInputs(req.InputPaths)
		if err != nil {
			return "", "", nil, false, 0, nil, err
		}
		stagingRoot, stageErr := stageMultiFileInputs(paths)
		if stageErr != nil {
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
	totalBytes, err := measureInputBytes(inPathAbs)
	if err != nil {
		return "", "", nil, false, 0, nil, coreerr.NewWithDetails(coreerr.CodeInternal, errFailedToMeasureInput, map[string]string{detailCause: err.Error()})
	}
	return inPathAbs, inPathAbs, []string{inPathAbs}, info.IsDir(), totalBytes, nil, nil
}

func normalizeFileInputs(paths []string) ([]string, int64, *coreerr.Error) {
	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	var totalBytes int64

	for _, p := range paths {
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
	stagingRoot, err := os.MkdirTemp("", "duesarc-multi-input-*")
	if err != nil {
		return "", err
	}

	commonRoot := commonDirPath(inputFiles)
	usedTargets := make(map[string]struct{}, len(inputFiles))
	for idx, src := range inputFiles {
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
		if err := copyFile(src, dst); err != nil {
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

	if _, err := io.Copy(out, in); err != nil {
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
	if strings.HasSuffix(inputPath, compressedFileExt) {
		return strings.TrimSuffix(inputPath, compressedFileExt)
	}
	if strings.HasSuffix(inputPath, zstdExt) {
		return strings.TrimSuffix(inputPath, zstdExt)
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
	return inputPath + outExt
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
		partPath := fmt.Sprintf("%s"+archiveSplitSuffixFmt, path, partIndex)
		out, err := os.Create(partPath)
		if err != nil {
			return nil, err
		}

		written := int64(0)
		for written < maxPartBytes {
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

	db, _, err := cli.Common(chunkSize, dbPath, key)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	if err := remapArchiveChunkPaths(db, util.BlobPath(dbPath)); err != nil {
		return 0, err
	}

	evidenceKeys, err := listEvidenceKeys(db)
	if err != nil {
		return 0, err
	}

	restored := 0
	for _, eviKey := range evidenceKeys {
		evidenceFile, err := dbio.GetEvidenceFile(eviKey, db)
		if err != nil {
			return restored, err
		}
		if !evidenceFile.Completed || evidenceFile.Failed {
			continue
		}

		rawID := dbio.EvidenceRawID(eviKey)
		hashB64 := base64.StdEncoding.EncodeToString(rawID)
		fallbackName := safeFallbackName(hashB64)
		targetPath, err := resolveRestoredFilePath(outputRoot, manifest, evidenceFile.Name, fallbackName)
		if err != nil {
			return restored, err
		}
		if err := restoreEvidenceToPath(hashB64, targetPath, db); err != nil {
			return restored, err
		}
		restored++
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

	evidenceKeys, err := listEvidenceKeys(db)
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
			ID:        idB64,
			Hash:      hash,
			Name:      evidenceFile.Name,
			Size:      evidenceFile.Size,
			Completed: evidenceFile.Completed,
			Failed:    evidenceFile.Failed,
			Status:    status,
		})
	}

	return logicalFiles, nil
}

func listEvidenceKeys(db *badger.DB) ([][]byte, error) {
	return listKeysByPrefix(db, []byte(cnst.EviFileNamespace))
}

func listKeysByPrefix(db *badger.DB, prefix []byte) ([][]byte, error) {
	keys := make([][]byte, 0)
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
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
	chunkKeys, err := listKeysByPrefix(db, []byte(cnst.ChonkNamespace))
	if err != nil {
		return err
	}

	for _, key := range chunkKeys {
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

	if err := store.Restore(hashB64, tempFile, db); err != nil {
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
