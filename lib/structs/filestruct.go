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
	Names map[string]struct{} `msgpack:"names"`
	Size  int64               `msgpack:"size"`
}

type IndexedNameMeta struct {
	IsDeleted    bool `msgpack:"is_deleted"`
	IsFragmented bool `msgpack:"is_fragmented"`
}

type IndexedFile struct {
	baseFile
	Start       int64                      `msgpack:"start"`
	IndexedType string                     `msgpack:"indexed_type"`
	IsDeleted   bool                       `msgpack:"is_deleted"`
	NameMeta    map[string]IndexedNameMeta `msgpack:"name_meta"`
}

func NewIndexedFile(name string, start, size int64, indexedType string, isDeleted bool) IndexedFile {
	bfile := baseFile{Names: map[string]struct{}{name: {}}, Size: size}
	nameMeta := map[string]IndexedNameMeta{name: {IsDeleted: isDeleted}}
	if indexedType == "" {
		indexedType = cnst.UnknownEvidenceType
	}
	return IndexedFile{baseFile: bfile, Start: start, IndexedType: indexedType, IsDeleted: isDeleted, NameMeta: nameMeta}
}

type InternalOffset struct {
	Start int64
	End   int64
}
type PartitionFile struct {
	IndexedFile
	InternalObjects map[string]InternalOffset `msgpack:"internal_objects"`
}

func NewPartitionFile(name string, start, size int64, indexedFiles map[string]InternalOffset) PartitionFile {
	indexedFile := NewIndexedFile(name, start, size, cnst.UnknownEvidenceType, false)
	return PartitionFile{IndexedFile: indexedFile, InternalObjects: indexedFiles}
}

type EvidenceFile struct {
	PartitionFile
	EvidenceType string      `msgpack:"evidence_type"`
	Completed    bool        `msgpack:"completed"`
	Failed       bool        `msgpack:"failed"`
	IngestState  IngestState `msgpack:"ingest_state"`
}

func NewEvidenceFile(name string, start, size int64, partitions map[string]InternalOffset, evidenceType string) EvidenceFile {
	partitionFile := NewPartitionFile(name, start, size, partitions)
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
