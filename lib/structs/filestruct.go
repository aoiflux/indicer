package structs

import "indicer/lib/cnst"

// IngestState tracks the commitment phase of an ingest transaction
type IngestState string

const (
	IngestStatePending   IngestState = "pending"
	IngestStateFlushed   IngestState = "flushed"
	IngestStateCompleted IngestState = "completed"
)

type baseFile struct {
	Name string `msgpack:"name"`
	Size int64  `msgpack:"size"`
}

// Timestamps carries a file's MACB times as Unix nanoseconds.
//
// A nil field means the filesystem does not record that time — ext has no
// birth time, exFAT and FAT have no change time — and that is a different
// statement from "the epoch". Flattening nil to 0 would assert a timestamp no
// evidence supports, so the pointers are preserved end to end.
//
// The fields are omitempty and msgpack encodes structs as maps, so a record
// written before timestamps existed decodes with every field nil and a record
// written without them is byte-identical to the old encoding. No migration.
type Timestamps struct {
	Modified *int64 `msgpack:"mtime,omitempty"`
	Accessed *int64 `msgpack:"atime,omitempty"`
	Changed  *int64 `msgpack:"ctime,omitempty"`
	Born     *int64 `msgpack:"btime,omitempty"`
}

// Any reports whether at least one time is present.
func (t *Timestamps) Any() bool {
	return t != nil && (t.Modified != nil || t.Accessed != nil || t.Changed != nil || t.Born != nil)
}

type IndexedFile struct {
	baseFile
	Start        int64  `msgpack:"start"`
	IndexedType  string `msgpack:"indexed_type"`
	IsDeleted    bool   `msgpack:"is_deleted"`
	IsFragmented bool   `msgpack:"is_fragmented"`
	FileHash     string `msgpack:"file_hash"`

	// Times are the filesystem's MACB timestamps, nil when the source
	// filesystem did not carry them or the record predates this field.
	Times *Timestamps `msgpack:"times,omitempty"`
}

// NewIndexedFile creates a new IndexedFile with optional FileHash
func NewIndexedFile(name string, start, size int64, indexedType string, isDeleted bool, fileHash string) IndexedFile {
	bfile := baseFile{Name: name, Size: size}
	if indexedType == "" {
		indexedType = cnst.UnknownEvidenceType
	}
	return IndexedFile{baseFile: bfile, Start: start, IndexedType: indexedType, IsDeleted: isDeleted, FileHash: fileHash}
}

type InternalOffset struct {
	Start int64
	End   int64
}
type PartitionFile struct {
	IndexedFile
	// InternalObjects is the members map of records written before ING-21,
	// and of evidence records, whose members are partitions.
	InternalObjects map[string]InternalOffset `msgpack:"internal_objects"`
	// MemberCount is how many members a partition keeps as PX||| keys.
	MemberCount int `msgpack:"member_count,omitempty"`
}

// NewPartitionFile creates a new PartitionFile with optional FileHash
func NewPartitionFile(name string, start, size int64, indexedFiles map[string]InternalOffset, fileHash string) PartitionFile {
	indexedFile := NewIndexedFile(name, start, size, cnst.UnknownEvidenceType, false, fileHash)
	return PartitionFile{IndexedFile: indexedFile, InternalObjects: indexedFiles}
}

type EvidenceFile struct {
	PartitionFile
	EvidenceType string      `msgpack:"evidence_type"`
	Completed    bool        `msgpack:"completed"`
	Failed       bool        `msgpack:"failed"`
	IngestState  IngestState `msgpack:"ingest_state"`
}

// NewEvidenceFile creates a new EvidenceFile with optional FileHash
func NewEvidenceFile(name string, start, size int64, partitions map[string]InternalOffset, evidenceType string, fileHash string) EvidenceFile {
	partitionFile := NewPartitionFile(name, start, size, partitions, fileHash)
	return EvidenceFile{PartitionFile: partitionFile, EvidenceType: evidenceType, Completed: false, Failed: false, IngestState: IngestStatePending}
}

// IsIngestIncomplete returns true if ingest is still in progress (not completed)
func (ef EvidenceFile) IsIngestIncomplete() bool {
	return !ef.Completed && ef.IngestState != IngestStateCompleted
}

// IsIngestOrphaned returns true if ingest is in FLUSHED or PENDING but not marked as failed
func (ef EvidenceFile) IsIngestOrphaned() bool {
	return !ef.Completed && !ef.Failed &&
		(ef.IngestState == IngestStatePending || ef.IngestState == IngestStateFlushed)
}

type FileTypes interface {
	PartitionFile | EvidenceFile
}
