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

	// The fields below are written by the W6 index pass (ContentModelV2);
	// legacy records leave them empty and carry the evidence ID and path in
	// Name. Read them through dbio.RecordEvidenceID and dbio.RecordPath.

	// EvidenceID and PartitionID are the raw IDs of the record's evidence
	// and partition (a partition record has no PartitionID).
	EvidenceID  []byte `msgpack:"evidence_id,omitempty"`
	PartitionID []byte `msgpack:"partition_id,omitempty"`
	// Path is the escaped full path in its filesystem, ":stream" included
	// for an alternate data stream.
	Path       string      `msgpack:"path,omitempty"`
	Stream     string      `msgpack:"stream,omitempty"`
	NodeKind   string      `msgpack:"node_kind,omitempty"` // file, symlink, other
	Inode      uint64      `msgpack:"inode,omitempty"`
	Seq        uint32      `msgpack:"seq,omitempty"`
	Target     string      `msgpack:"target,omitempty"` // symlink target
	Links      int         `msgpack:"links,omitempty"`
	Prov       uint32      `msgpack:"prov,omitempty"`       // core.Prov flags
	Confidence uint8       `msgpack:"confidence,omitempty"` // core.Confidence
	Content    *ContentRef `msgpack:"content,omitempty"`
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
	// Partition is what the scan found in the partition-table entry; nil for
	// legacy records and evidence records.
	Partition *PartitionMeta `msgpack:"partition,omitempty"`
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

	// The fields below are written by ingests from W6 on; a legacy record
	// leaves them empty, and its layout is inferred (I3).
	ContentModel  string `msgpack:"content_model,omitempty"`
	ChunkSize     int64  `msgpack:"chunk_size,omitempty"`
	ChunkHashAlgo string `msgpack:"chunk_hash_algo,omitempty"`
	FileHashAlgo  string `msgpack:"file_hash_algo,omitempty"`
	HashStrategy  string `msgpack:"hash_strategy,omitempty"`
	// PlainHash is the hash of the stored bytes; ContentRoot is the hocho
	// root over the chunk hashes, the content identity idempotence uses
	// (D1). FileHash is one of the two, as the hash strategy says.
	PlainHash   string          `msgpack:"plain_hash,omitempty"`
	ContentRoot string          `msgpack:"content_root,omitempty"`
	Fingerprint string          `msgpack:"fingerprint,omitempty"`
	Source      *EvidenceSource `msgpack:"source,omitempty"`
	// Features are the optional records this ingest wrote: revrel,
	// simhash, fts, enrich.
	Features  []string   `msgpack:"features,omitempty"`
	Attempt   int        `msgpack:"attempt,omitempty"`
	LastError string     `msgpack:"last_error,omitempty"`
	Index     *IndexInfo `msgpack:"index,omitempty"`
	// DuplicateOf is the raw ID of the evidence a duplicate attempt's
	// content belongs to.
	DuplicateOf []byte `msgpack:"duplicate_of,omitempty"`
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
