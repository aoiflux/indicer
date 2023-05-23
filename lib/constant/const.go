package constant

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

var MaxThreadCount = runtime.NumCPU()
var ChonkSize = DefaultChonkSize

const (
	EvidenceFileNamespace  = "E|||:"
	PartitionFileNamespace = "P|||:"
	IndexedFileNamespace   = "I|||:"
	RelationNapespace      = "R|||:"
	ChonkNamespace         = "C|||:"
	NamespaceSeperator     = "|||:"
	PipeSeperator          = "|"
	FilePathSeperator      = "|||"
	PartitionIndexPrefix   = "p"

	EvidenceFileType  = "evidence"
	PartitionFileType = "partition"
	IndexedFileType   = "indexed"
)

var (
	ErrIncompleteFile         = errors.New("incomplete file")
	ErrUnableToParseFile      = errors.New("unable to find partitions/parse image file")
	ErrIncompatibleFile       = errors.New("unknown/incompatible file detected")
	ErrIncompatibleFileSystem = errors.New("unknown file system")
	ErrUnknownFileType        = errors.New("unknown file type. please try one of the following: evidence|partition|indexed")
)

const (
	EXFAT = 0x07
)

const (
	CmdStore   = "store"
	CmdList    = "list"
	CmdRestore = "restore"

	SubCmdEvi       = "evi"
	SubCmdPartition = "partition"
	SubCmdIndexed   = "indexed"
)

const IgnoreVar int64 = -1

func GetCacheLimit() (int64, error) {
	vmemstat, err := mem.VirtualMemory()
	return int64(vmemstat.Available / 8), err
}
