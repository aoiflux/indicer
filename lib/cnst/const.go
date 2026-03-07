package cnst

import (
	"errors"
	"runtime"

	"github.com/klauspost/compress/zstd"
	"github.com/shirou/gopsutil/v3/mem"
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

var ChonkSize = DefaultChonkSize
var MEMOPT bool
var QUICKOPT bool
var CONTAINERMODE bool
var HIERARCHICALINDEX bool

const (
	EviFileNamespace         = "E|||:"
	PartiFileNamespace       = "P|||:"
	IdxFileNamespace         = "I|||:"
	RelationNamespace        = "R|||:"
	ReverseRelationNamespace = "Я|||:"
	ChonkNamespace           = "C|||:"
	NamespaceSeperator       = "|||:"
	RangeSeperator           = "-"
	DataSeperator            = "|||"
	PartitionIndexPrefix     = "p"
)

const (
	BLOBSDIR    = "BLOBS"
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
	CmdStore   = "store"
	CmdList    = "list"
	CmdRestore = "restore"
	CmdNear    = "near"
	CmdReset   = "reset"
	SubCmdIn   = "in"
	SubCmdOut  = "out"
	CmdSearch  = "search"

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
	FlagHierarchicalShort    = 'h'
	FlagSyncIndex            = "sync"
	FlagSyncIndexShort       = 's'
	FlagNoIndex              = "no-index"
	FlagNoIndexShort         = 'n'

	OperandFile  = "FILE"
	OperandHash  = "HASH"
	OperandQuery = "QUERY"
)

const IgnoreVar int64 = -1

var DECODER *zstd.Decoder
var ENCODER *zstd.Encoder

func GetMaxThreadCount() int {
	if MEMOPT {
		return 1
	}
	// High performance mode (default): Use CPU * 2 workers
	// This maximizes throughput since workers are I/O-bound
	// (waiting on file writes, DB operations, compression, etc.)
	return runtime.NumCPU() * 2
}
func GetCacheLimit() (int64, error) {
	if MEMOPT {
		return 64 * KB, nil
	}
	vmemstat, err := mem.VirtualMemory()
	return int64(vmemstat.Available / 4), err
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
