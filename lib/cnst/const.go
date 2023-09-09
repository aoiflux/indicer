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
	FlagSyncIndex            = "sync"
	FlagSyncIndexShort       = 's'
	FlagNoIndex              = "no-index"
	FlagNoIndexShort         = 'n'

	OperandFile = "FILE"
	OperandHash = "HASH"
)

const IgnoreVar int64 = -1

var DECODER *zstd.Decoder
var ENCODER *zstd.Encoder

func GetMaxThreadCount() int {
	if MEMOPT {
		return 1
	}
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
