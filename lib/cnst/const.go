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

var (
	ErrIncompleteFile         = errors.New("incomplete file")
	ErrUnableToParseFile      = errors.New("unable to find partitions/parse image file")
	ErrIncompatibleFile       = errors.New("unknown/incompatible file detected")
	ErrIncompatibleFileSystem = errors.New("unknown file system")
	ErrUnknownFileType        = errors.New("unknown file type. please try one of the following: evidence|partition|indexed")
	ErrIncorrectOption        = errors.New("indicer near <in|out> <hash|file_path> [deep]\n\tUse option in to get NeAR of files inside the database, provide a hash string\n\tUse option out to get NeAR of files outside of the database, provide a path")
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

	SubCmdEvi       = "evi"
	SubCmdPartition = "partition"
	SubCmdIndexed   = "indexed"

	InOptionIn  = "in"
	InOptionOut = "out"
	DeepOption  = "deep"
)

const IgnoreVar int64 = -1

func GetMaxThreadCount() int {
	max := runtime.NumCPU() / 2
	if max > 2 {
		return max
	}
	return 2
}
func GetCacheLimit() (int64, error) {
	vmemstat, err := mem.VirtualMemory()
	return int64(vmemstat.Available / 8), err
}
