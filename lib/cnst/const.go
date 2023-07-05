package cnst

import (
	"errors"
	"runtime"

	"github.com/shirou/gopsutil/mem"
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
	MaxBatchCount           = 1024 * 10
	DefaultChonkSize        = 256 * KB
	KeySize                 = 32
)

var ChonkSize = DefaultChonkSize
var MEMOPT bool

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
	EviBucket                = "E"
	PartiBucket              = "P"
	IdxBucket                = "I"
	RelBucket                = "R"
	ReverseRelBucket         = "Я"
	ChonkBucket              = "C"
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
	ErrKeyNotFound            = errors.New("key not found")
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

	OperandFile = "FILE"
	OperandHash = "HASH"
)

const IgnoreVar int64 = -1

func GetMaxThreadCount() int {
	if MEMOPT {
		return 1
	}
	return runtime.NumCPU() * 2
}
func GetBatchLimit() (int, error) {
	if MEMOPT {
		return 1000, nil
	}
	vmemstat, err := mem.VirtualMemory()
	batchLimitBytes := int64(vmemstat.Available / 4)
	batchLimitChonks := batchLimitBytes / DefaultChonkSize
	return int(batchLimitChonks), err
}
