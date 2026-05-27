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
var HIERARCHICALINDEX bool
var ENABLESIMHASH = false  // compute and persist per-chunk simhash signatures during ingest (opt-in via --simhash)
var CompressLevel = "best" // per-chunk zstd ingest level: fast | default | best
var StoreHashStrategy = SyncHashStrategy
var HochoMode = HochoModeBaseline
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
	RelationNamespace               = "R|||:"
	ReverseRelationNamespace        = "Я|||:"
	ReverseRelationAppendNamespace  = "RA|||:"
	ReverseRelationSegmentNamespace = "RAS|||:"
	ChonkNamespace                  = "C|||:"
	ChonkSimhashNamespace           = "S|||:"
	FileSimhashNamespace            = "F|||:"
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

	FlagDBPath               = "dbpath"
	FlagDBPathShort          = 'd'
	FlagPassword             = "password"
	FlagPasswordShort        = 'p'
	FlagNearOption           = "nearoption"
	FlagNearOptionShort      = 'n'
	FlagDeep                 = "deep"
	FlagDeepShort            = 'e'
	FlagChonkSize            = "chonksize"
	FlagChonkSizeShort       = 'c'
	FlagRestoreFilePath      = "filepath"
	FlagRestoreFilePathShort = 'f'
	FlagLowResource          = "low"
	FlagLowResourceShort     = 'l'
	FlagFastMode             = "quick"
	FlagFastModeShort        = 'q'
	FlagContainerMode        = "container"
	FlagContainerModeShort   = 'x'
	FlagHierarchicalIndex    = "hierarchical"
	FlagHierarchicalShort    = 'i'
	FlagSyncIndex            = "sync"
	FlagSyncIndexShort       = 's'
	FlagNoIndex              = "no-index"
	FlagNoIndexShort         = 'n'
	FlagHashAlgo             = "hash-algo"
	FlagHashAlgoShort        = 'g'
	FlagHashStrategy         = "hash-strategy"
	FlagHochoMode            = "hocho-mode"
	FlagExplainExact         = "explain-exact"
	FlagExplainExactShort    = 't'
	FlagAdvancedDeep         = "advanced-deep"
	FlagAdvancedDeepShort    = 'a'
	FlagRepairFix            = "fix"
	FlagRepairMigrateRevRel  = "migrate-revrel"
	FlagTopK                 = "top-k"
	FlagTopKShort            = 'k'
	FlagEnableFts            = "enable-fts"
	FlagEnableEnrichment     = "enrich"
	FlagListStatus           = "status"
	FlagCompressLevel        = "compress-level"
	FlagSimhash              = "simhash"
	FlagStoreWorkers         = "store-workers"
	FlagStoreQueue           = "store-queue"
	FlagRestoreWorkers       = "restore-workers"
	FlagRestoreQueue         = "restore-queue"
	FlagRestoreProgressMs    = "restore-progress-ms"
	FlagRestoreBufferMB      = "restore-buffer-mb"

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

// GetStorePipelineTuning returns worker count and task queue depth for the
// batch-owner ingest pipeline. Non-container mode benefits from higher worker
// parallelism for chunk materialization, while container/hierarchical modes
// involve additional serialized components and are tuned for steadier flow.
func GetStorePipelineTuning(containerMode bool, hierarchicalMode bool) (int, int) {
	workers := GetMaxThreadCount()
	queueDepth := workers * 2

	if containerMode {
		workers = workers / 2
		if workers < 2 {
			workers = 2
		}
		queueDepth = workers * 4
		if hierarchicalMode {
			queueDepth = workers * 6
		}
	}

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

// GetRestorePipelineTuning returns worker count and queue depth for restore.
// Restore is read/decode heavy, so defaults are moderately parallel and bounded.
func GetRestorePipelineTuning() (int, int) {
	workers := runtime.NumCPU()
	if MEMOPT {
		workers = workers / 2
	}
	if workers < 2 {
		workers = 2
	}
	if workers > 16 {
		workers = 16
	}

	queueDepth := workers * 4

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
func GetMaxBatchCount() (int, error) {
	if MEMOPT {
		return 16, nil
	}
	vmemstat, err := mem.VirtualMemory()
	if err != nil {
		return int(IgnoreVar), err
	}
	limit := vmemstat.Available / 4
	batchCount := limit / uint64(ChonkSize)
	return int(batchCount), nil
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
