package core

// PartitionScheme identifies a partition-table scheme.
type PartitionScheme string

const (
	SchemeMBR  PartitionScheme = "mbr"
	SchemeGPT  PartitionScheme = "gpt"
	SchemeAPM  PartitionScheme = "apm" // Apple Partition Map
	SchemeBSD  PartitionScheme = "bsd" // BSD disklabel
	SchemeSun  PartitionScheme = "sun" // Sun VTOC
	SchemeNone PartitionScheme = "none"
)

// Partition describes one partition located within an Image.
//
// Offset and Size are absolute byte values within the Image, so a filesystem
// parser mounts a partition with io.NewSectionReader(image, p.Offset, p.Size).
type Partition struct {
	Index      int
	Offset     int64  // absolute byte offset within the Image
	Size       int64  // partition length in bytes
	TypeCode   uint64 // MBR type byte / GPT type code
	TypeName   string // human-readable type name
	Name       string // partition label, if any
	GUIDType   string // GPT partition-type GUID (empty for non-GPT)
	GUIDUnique string // GPT unique partition GUID (empty for non-GPT)
	Allocated  bool   // false for unallocated gaps reported by the parser
}

// PartitionTable is the result of parsing a partition scheme over an Image.
type PartitionTable struct {
	Scheme     PartitionScheme
	BlockSize  uint32
	Partitions []Partition
}

// Allocated returns only the allocated partitions (skipping unallocated gaps).
func (t PartitionTable) Allocated() []Partition {
	out := make([]Partition, 0, len(t.Partitions))
	for _, p := range t.Partitions {
		if p.Allocated {
			out = append(out, p)
		}
	}
	return out
}
