package cnst

import (
	"errors"
	"strings"
)

const (
	FILE_EXISTS         = "EXISTS"
	FILE_APPENDED       = "APPENDED"
	UnknownEvidenceType = "unknown"
	DefaultDBPath       = "./data"
	// DirPerm is for directories DUES creates in a data root, BlobPerm for the
	// chunk blobs and containers in it: they hold evidence content, in the
	// clear in an unencrypted database, so only the owner may read them
	// (STO-19). FilePerm is for outputs the user names (reports, restored
	// files) and the manifest, which holds no evidence.
	DirPerm             = 0o700
	BlobPerm            = 0o600
	FilePerm            = 0o644
	KVDBDIR             = "kvdb"
	GRAPHDIR            = "graph"
	BLOBSDIR            = "blob"
	FTSDIR              = "fts"
	UploadsDir          = "uploads"
	ExportsDir          = "exports"   // packages written into the root; packages leave it out
	KEYSDIR             = "dues.keys" // actors' signing keys and keyring (lib/graph/signer)
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
	FlagStoreJobs                   = "store-jobs"
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

// ParseHashAlgo returns the chunk hash algorithm algo names (SHA3 or
// BLAKE3, the default).
func ParseHashAlgo(algo string) (string, error) {
	normalized := NormalizeHashAlgo(algo)
	if normalized == "" {
		return "", errors.New("invalid hash algorithm: must be sha3 or blake3")
	}
	return normalized, nil
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

// ParseHashStrategy returns the hash strategy strategy names (sync, the
// default, async or hocho).
func ParseHashStrategy(strategy string) (string, error) {
	normalized := NormalizeHashStrategy(strategy)
	if normalized == "" {
		return "", errors.New("invalid hash strategy: must be sync, async, or hocho")
	}
	return normalized, nil
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

// ParseStorePipeline returns the store pipeline mode names (batch-owner,
// the default, or staged).
func ParseStorePipeline(mode string) (string, error) {
	normalized := NormalizeStorePipeline(mode)
	if normalized == "" {
		return "", errors.New("invalid store pipeline: must be batch-owner or staged")
	}
	return normalized, nil
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

// ParseStoreIOEngine returns the store io engine engine names (auto, the
// default, stdlib or io-uring).
func ParseStoreIOEngine(engine string) (string, error) {
	normalized := NormalizeStoreIOEngine(engine)
	if normalized == "" {
		return "", errors.New("invalid store io engine: must be auto, stdlib, or io-uring")
	}
	return normalized, nil
}

// CheckIOUringQueueDepth refuses a negative io-uring queue depth; 0 is auto.
func CheckIOUringQueueDepth(depth int) error {
	if depth < 0 {
		return errors.New("invalid io-uring queue depth: must be >= 0")
	}
	return nil
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

// MaxPendingBatchTxns bounds a write batch's pending transactions: each one
// holds a transaction's worth of memory until it commits.
const MaxPendingBatchTxns = 64

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
