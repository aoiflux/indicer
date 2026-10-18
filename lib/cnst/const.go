package cnst

import (
	"crypto/sha3"
	"errors"
	"hash"
	"runtime"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/zeebo/blake3"
)

const (
	FILE_EXISTS         = "EXISTS"
	FILE_APPENDED       = "APPENDED"
	UnknownEvidenceType = "unknown"
	DefaultDBPath       = "./data"
	DirPerm             = 0o755
	FilePerm            = 0o644
	KVDBDIR             = "kvdb"
	GRAPHDIR            = "graph"
	BLOBSDIR            = "blob"
	FTSDIR              = "fts"
	UploadsDir          = "uploads"
	SHA3                = "sha3"
	BLAKE3              = "blake3"
	SyncHashStrategy    = "sync"
	AsyncHashStrategy   = "async"
	HochoHashStrategy   = "hocho"
	HochoModeBaseline   = "baseline"
	HochoModeReuse      = "reuse"
	HochoModePostDedup  = "postdedup"
	StorePipelineBatch  = "batch-owner"
	StorePipelineStaged = "staged"
	StoreIOEngineAuto   = "auto"
	StoreIOEngineStdlib = "stdlib"
	StoreIOEngineUring  = "io-uring"
)

const (
	B  int64 = 1
	KB       = B << 10
	MB       = KB << 10
	GB       = MB << 10
	TB       = GB << 10
)

const (
	CacheLimit              = GB
	SectorSize       uint64 = 512
	DefaultChonkSize        = 256 * KB
	KeySize                 = 32
)

var HASHALGO = BLAKE3
var ChonkSize = DefaultChonkSize
var MEMOPT bool
var QUICKOPT bool
var CONTAINERMODE bool

// LegacyBlockIndex is set for a database written in the retired hierarchical
// mode (D9): chunk locations not in C||| are looked up in its block index.
// Only the manifest sets it.
var LegacyBlockIndex bool
var ENABLESIMHASH = false  // compute and persist per-chunk simhash signatures during ingest (opt-in via --simhash)
var ENABLEREVREL = false   // persist reverse-relation writes during ingest (opt-in via --enable-revrel)
var CompressLevel = "best" // per-chunk zstd ingest level: fast | default | best
var StoreHashStrategy = SyncHashStrategy
var HochoMode = HochoModeBaseline
var StorePipelineMode = StorePipelineBatch
var StoreIOEngine = StoreIOEngineAuto
var StoreIOUringQueueDepth = 0 // io-uring SQ/CQ depth (0 = auto)
var StoreWorkerCount = 0
var StoreTaskQueueDepth = 0
var RestoreWorkerCount = 0
var RestoreTaskQueueDepth = 0
var RestoreProgressIntervalMs = 0
var RestoreWriteBufferMB = 0
var DB *badger.DB

const (
	EviFileNamespace                = "E|||:"
	EviFileHashLookupNamespace      = "EH|||:"
	PartiFileNamespace              = "P|||:"
	PartiFileHashLookupNamespace    = "PH|||:"
	IdxFileNamespace                = "I|||:"
	IdxFileHashLookupNamespace      = "IH|||:"
	HashLookupMemberNamespace       = "HM|||:" // one key per hash lookup member (STO-22)
	PartitionMemberNamespace        = "PX|||:" // one key per partition member (ING-21)
	RelationNamespace               = "R|||:"
	ReverseRelationNamespace        = "Я|||:"
	ReverseRelationAppendNamespace  = "RA|||:"
	ReverseRelationSegmentNamespace = "RAS|||:"
	ChonkNamespace                  = "C|||:"
	ChonkSimhashNamespace           = "S|||:"
	FileSimhashNamespace            = "F|||:"
	EviRootLookupNamespace          = "ER|||:" // content root to the evidence holding it (D1)
	EviQuickLookupNamespace         = "EQ|||:" // size and fingerprint to candidate evidence (D1)
	EviAliasNamespace               = "EA|||:" // later ingests of stored content (D1)
	EviAttemptNamespace             = "EP|||:" // source path to its unfinished attempt
	ObjectNamespace                 = "X|||:"  // materialised content objects (D10)
	ExtentPageNamespace             = "XE|||:" // extents of very fragmented indexed files
	PartitionReportNamespace        = "PR|||:" // a partition's index report
	NamespaceSeperator              = "|||:"
	RangeSeperator                  = "-"
	DataSeperator                   = "|||"
	PartitionIndexPrefix            = "p"
	ReverseRelationAppendShardCount = 16
)

const (
	BLOBEXT     = ".blob"
	BLOBZSTEXT  = ".blob.zst"
	FileNameLen = 25
)

var (
	ErrHashNotFound           = errors.New("must provide file hash")
	ErrFileNotFound           = errors.New("must provide a file to save")
	ErrIncompleteFile         = errors.New("incomplete file")
	ErrUnableToParseFile      = errors.New("unable to find partitions/parse image file")
	ErrIncompatibleFile       = errors.New("unknown/incompatible file detected")
	ErrIncompatibleFileSystem = errors.New("unknown file system")
	ErrUnknownFileType        = errors.New("unknown file type. please try one of the following: evidence|partition|indexed")
	ErrIncorrectOption        = errors.New("indicer near <in|out> <hash|file_path> [deep]\n\tUse option in to get NeAR of files inside the database, provide a hash string\n\tUse option out to get NeAR of files outside of the database, provide a path")
	ErrNilBatch               = errors.New("call SetBatch first, batch is nil. cannot work with nil batch")
	ErrSmallQuery             = errors.New("search query too small. query requires at least 2 characters")
	ErrTooManySplits          = errors.New("too many splits: %v")
)

const (
	EXFAT = 0x07
)

const (
	CmdTui      = "tui"
	CmdStore    = "store"
	CmdList     = "list"
	CmdStats    = "stats"
	CmdRepair   = "repair"
	CmdRestore  = "restore"
	CmdNear     = "near"
	CmdReset    = "reset"
	CmdPurge    = "purge"
	CmdDelete   = "delete"
	CmdDestroy  = "destroy"
	SubCmdIn    = "in"
	SubCmdOut   = "out"
	CmdSearch   = "search"
	CmdMicro    = "micro"
	CmdEnrich   = "enrich"
	CmdServer   = "server"
	CmdVeresion = "version"

	FlagDBPath                      = "dbpath"
	FlagDBPathShort                 = 'd'
	FlagPassword                    = "password"
	FlagPasswordShort               = 'p'
	FlagNearOption                  = "nearoption"
	FlagNearOptionShort             = 'n'
	FlagDeep                        = "deep"
	FlagDeepShort                   = 'e'
	FlagChonkSize                   = "chonksize"
	FlagChonkSizeShort              = 'c'
	FlagRestoreFilePath             = "filepath"
	FlagRestoreFilePathShort        = 'f'
	FlagLowResource                 = "low"
	FlagLowResourceShort            = 'l'
	FlagFastMode                    = "quick"
	FlagFastModeShort               = 'q'
	FlagContainerMode               = "container"
	FlagContainerModeShort          = 'x'
	FlagHierarchicalIndex           = "hierarchical"
	FlagHierarchicalShort           = 'i'
	FlagPreset                      = "preset"
	FlagPresetShort                 = 'P'
	FlagQuickMode                   = "quick-mode"
	FlagQuickModeShort              = 'Q'
	FlagPerformanceMode             = "performance-mode"
	FlagPerformanceModeShort        = 'o'
	FlagLowResourceMode             = "low-resource-mode"
	FlagLowResourceModeShort        = 'L'
	FlagSyncIndex                   = "sync"
	FlagSyncIndexShort              = 's'
	FlagNoIndex                     = "no-index"
	FlagNoIndexShort                = 'N'
	FlagHashAlgo                    = "hash-algo"
	FlagHashAlgoShort               = 'g'
	FlagHashStrategy                = "hash-strategy"
	FlagHashStrategyShort           = 'S'
	FlagStorePipeline               = "pipeline"
	FlagStorePipelineShort          = 'Y'
	FlagStoreAsyncFileWrite         = "async-fio"
	FlagStoreAsyncFileWriteShort    = 'A'
	FlagStoreIOEngine               = "io-engine"
	FlagStoreIOEngineShort          = 'I'
	FlagStoreIOUringQueueDepth      = "io-uring-queue-depth"
	FlagStoreIOUringQueueDepthShort = 'j'
	FlagHochoMode                   = "hocho-mode"
	FlagHochoModeShort              = 'M'
	FlagExplainExact                = "explain-exact"
	FlagExplainExactShort           = 't'
	FlagAdvancedDeep                = "advanced-deep"
	FlagAdvancedDeepShort           = 'a'
	FlagRepairFix                   = "fix"
	FlagRepairFixShort              = 'J'
	FlagRepairUpgrade               = "upgrade"
	FlagRepairVerify                = "verify"
	FlagRepairMigrateRevRel         = "migrate-revrel"
	FlagTopK                        = "top-k"
	FlagTopKShort                   = 'k'
	FlagEnableFts                   = "fts"
	FlagEnableFtsShort              = 'F'
	FlagEnableEnrichment            = "enrich"
	FlagEnableEnrichmentShort       = 'E'
	FlagListStatus                  = "status"
	FlagListStatusShort             = 'u'
	FlagCompressLevel               = "compress-level"
	FlagCompressLevelShort          = 'z'
	FlagSimhash                     = "simhash"
	FlagSimhashShort                = 'H'
	FlagEnableRevRel                = "revrel"
	FlagEnableRevRelShort           = 'R'
	FlagStoreWorkers                = "store-workers"
	FlagStoreWorkersShort           = 'w'
	FlagStoreQueue                  = "store-queue"
	FlagStoreQueueShort             = 'W'
	FlagRestoreWorkers              = "restore-workers"
	FlagRestoreWorkersShort         = 'y'
	FlagRestoreQueue                = "restore-queue"
	FlagRestoreQueueShort           = 'U'
	FlagRestoreProgressMs           = "restore-progress-ms"
	FlagRestoreProgressMsShort      = 'm'
	FlagRestoreBufferMB             = "restore-buffer-mb"
	FlagRestoreBufferMBShort        = 'b'
	FlagRankAlpha                   = "rank-alpha"
	FlagRankAlphaShort              = 'r'
	FlagMicroExtractForceShort      = 'V'
	FlagMicroExtractTopKShort       = 'K'

	PresetQuick       = "quick"
	PresetPerformance = "performance"
	PresetLowResource = "low-resource"

	OperandFile  = "FILE"
	OperandHash  = "HASH"
	OperandQuery = "QUERY"
)

const IgnoreVar int64 = -1

var DECODER *zstd.Decoder
var ENCODER *zstd.Encoder

func NormalizeHashAlgo(algo string) string {
	switch strings.ToLower(strings.TrimSpace(algo)) {
	case SHA3:
		return SHA3
	case BLAKE3, "":
		return BLAKE3
	default:
		return ""
	}
}

func SetHashAlgo(algo string) error {
	normalized := NormalizeHashAlgo(algo)
	if normalized == "" {
		return errors.New("invalid hash algorithm: must be sha3 or blake3")
	}
	HASHALGO = normalized
	return nil
}

func NormalizeHashStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case SyncHashStrategy, "", "synchash":
		return SyncHashStrategy
	case AsyncHashStrategy, "asynchash":
		return AsyncHashStrategy
	case HochoHashStrategy, "hochohash":
		return HochoHashStrategy
	default:
		return ""
	}
}

func SetStoreHashStrategy(strategy string) error {
	normalized := NormalizeHashStrategy(strategy)
	if normalized == "" {
		return errors.New("invalid hash strategy: must be sync, async, or hocho")
	}
	StoreHashStrategy = normalized
	return nil
}

func NormalizeStorePipeline(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case StorePipelineBatch, "":
		return StorePipelineBatch
	case StorePipelineStaged:
		return StorePipelineStaged
	default:
		return ""
	}
}

func SetStorePipeline(mode string) error {
	normalized := NormalizeStorePipeline(mode)
	if normalized == "" {
		return errors.New("invalid store pipeline: must be batch-owner or staged")
	}
	StorePipelineMode = normalized
	return nil
}

func NormalizeStoreIOEngine(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case StoreIOEngineAuto, "":
		return StoreIOEngineAuto
	case StoreIOEngineStdlib:
		return StoreIOEngineStdlib
	case StoreIOEngineUring:
		return StoreIOEngineUring
	default:
		return ""
	}
}

func SetStoreIOEngine(engine string) error {
	normalized := NormalizeStoreIOEngine(engine)
	if normalized == "" {
		return errors.New("invalid store io engine: must be auto, stdlib, or io-uring")
	}
	StoreIOEngine = normalized
	return nil
}

func SetStoreIOUringQueueDepth(depth int) error {
	if depth < 0 {
		return errors.New("invalid io-uring queue depth: must be >= 0")
	}
	StoreIOUringQueueDepth = depth
	return nil
}

// GetStoreIOUringQueueDepth returns queue depth for io-uring.
// A value of 0 auto-tunes based on CPU count and bounds the result.
func GetStoreIOUringQueueDepth() int {
	if StoreIOUringQueueDepth > 0 {
		return StoreIOUringQueueDepth
	}

	depth := runtime.NumCPU() * 16
	if depth < 256 {
		depth = 256
	}
	if depth > 4096 {
		depth = 4096
	}
	return depth
}

func NormalizeHochoMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case HochoModeReuse:
		return HochoModeReuse
	case HochoModeBaseline, "":
		return HochoModeBaseline
	case HochoModePostDedup:
		return HochoModePostDedup
	default:
		return ""
	}
}

func SetHochoMode(mode string) error {
	normalized := NormalizeHochoMode(mode)
	if normalized == "" {
		return errors.New("invalid hocho mode: must be baseline, reuse, or postdedup")
	}
	HochoMode = normalized
	return nil
}

func GetHashAlgo(bigFile ...bool) hash.Hash {
	flag := false
	if len(bigFile) > 0 {
		flag = bigFile[0]
	}

	switch HASHALGO {
	case SHA3:
		if flag {
			return sha3.New256()
		}
		return sha3.New512()
	case BLAKE3:
		return blake3.New()
	}

	return blake3.New()
}

func GetMaxThreadCount() int {
	if MEMOPT {
		return 1
	}
	// High performance mode (default): Use CPU * 2 workers
	// This maximizes throughput since workers are I/O-bound
	// (waiting on file writes, DB operations, compression, etc.)
	return runtime.NumCPU() * 2
}

// DefaultStoreTuning returns the batch-owner ingest pipeline's worker count
// and task queue depth for a machine with cpus CPUs, before any override.
// Non-container mode benefits from higher worker parallelism for chunk
// materialization, while container mode involves an additional serialized
// writer and is tuned for steadier flow.
func DefaultStoreTuning(cpus int, memOpt, containerMode bool) (int, int) {
	workers := cpus * 2
	if memOpt {
		workers = 1
	}
	queueDepth := workers * 2

	if containerMode {
		workers = workers / 2
		if workers < 2 {
			workers = 2
		}
		queueDepth = workers * 4
	}
	return workers, queueDepth
}

// GetStorePipelineTuning returns worker count and task queue depth for the
// batch-owner ingest pipeline: the defaults for this machine, replaced by
// StoreWorkerCount and StoreTaskQueueDepth when those are set.
func GetStorePipelineTuning(containerMode bool) (int, int) {
	workers, queueDepth := DefaultStoreTuning(runtime.NumCPU(), MEMOPT, containerMode)

	if StoreWorkerCount > 0 {
		workers = StoreWorkerCount
	}
	if StoreTaskQueueDepth > 0 {
		queueDepth = StoreTaskQueueDepth
	}

	if workers < 1 {
		workers = 1
	}
	if queueDepth < workers {
		queueDepth = workers
	}

	return workers, queueDepth
}

// DefaultRestoreTuning returns restore's worker count and queue depth for a
// machine with cpus CPUs, before any override. Restore is read/decode heavy,
// so defaults are moderately parallel and bounded.
func DefaultRestoreTuning(cpus int, memOpt bool) (int, int) {
	workers := cpus
	if memOpt {
		workers = workers / 2
	}
	if workers < 2 {
		workers = 2
	}
	if workers > 16 {
		workers = 16
	}
	return workers, workers * 4
}

// GetRestorePipelineTuning returns worker count and queue depth for restore:
// the defaults for this machine, replaced by RestoreWorkerCount and
// RestoreTaskQueueDepth when those are set.
func GetRestorePipelineTuning() (int, int) {
	workers, queueDepth := DefaultRestoreTuning(runtime.NumCPU(), MEMOPT)

	if RestoreWorkerCount > 0 {
		workers = RestoreWorkerCount
	}
	if RestoreTaskQueueDepth > 0 {
		queueDepth = RestoreTaskQueueDepth
	}

	if workers < 1 {
		workers = 1
	}
	if queueDepth < workers {
		queueDepth = workers
	}

	return workers, queueDepth
}

func GetRestoreProgressInterval() time.Duration {
	if RestoreProgressIntervalMs <= 0 {
		return 120 * time.Millisecond
	}
	if RestoreProgressIntervalMs < 20 {
		return 20 * time.Millisecond
	}
	if RestoreProgressIntervalMs > 2000 {
		return 2000 * time.Millisecond
	}
	return time.Duration(RestoreProgressIntervalMs) * time.Millisecond
}

func GetRestoreWriteBufferSize() int {
	if RestoreWriteBufferMB <= 0 {
		if MEMOPT {
			return 32 * 1024 * 1024
		}
		return 256 * 1024 * 1024
	}
	if RestoreWriteBufferMB < 1 {
		return 1 * 1024 * 1024
	}
	if RestoreWriteBufferMB > 256 {
		return 256 * 1024 * 1024
	}
	return RestoreWriteBufferMB * 1024 * 1024
}

func GetCacheLimit() (int64, error) {
	if MEMOPT {
		return 64 * MB, nil
	}

	const (
		cacheFallback = 256 * MB
		cacheMin      = 64 * MB
		cacheMax      = 8 * GB
	)

	vmemstat, err := mem.VirtualMemory()
	if err != nil {
		// Reliability first: use a sane fallback when memory probing fails.
		return cacheFallback, nil
	}

	cacheLimit := int64(vmemstat.Available / 4)
	if cacheLimit < cacheMin {
		cacheLimit = cacheMin
	}
	if cacheLimit > cacheMax {
		cacheLimit = cacheMax
	}

	return cacheLimit, nil
}

// MaxPendingBatchTxns bounds a write batch's pending transactions: each one
// holds a transaction's worth of memory until it commits.
const MaxPendingBatchTxns = 64

// GetMaxBatchCount returns how many transactions a write batch may have
// pending: a quarter of available memory in chunks, between 1 (0 deadlocks
// badger) and MaxPendingBatchTxns.
func GetMaxBatchCount() (int, error) {
	if MEMOPT {
		return 16, nil
	}
	vmemstat, err := mem.VirtualMemory()
	if err != nil || ChonkSize <= 0 {
		// Reliability first: a modest limit when memory or the chunk size
		// is unknown.
		return 16, nil
	}
	n := vmemstat.Available / 4 / uint64(ChonkSize)
	return int(max(1, min(n, MaxPendingBatchTxns))), nil
}

const GRAPH_START = `<!DOCTYPE html>
<html lang="en">

<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Artefact Relation Graph</title>
    <script src="./vis.min.js"></script>
    <style>
        #nw {
            width: 100%;
            height: 100vh;
            border: 1px solid lightgray;
        }
    </style>
</head>

<body>
    <div id="nw"></div>
    <script>
        `
const GRAPH_END = `
        var container = document.getElementById("nw");
        var data = {
            nodes: nodes,
            edges: edges
        };
        var options = {
            edges: {
                scaling: {
                    min: 1,
                    max: 5,
                    label: {
                        enabled: true,
                        min: 10,
                        max: 15
                    }
                },
            }
        };
        var network = new vis.Network(container, data, options);
    </script>
</body>

</html>
`
