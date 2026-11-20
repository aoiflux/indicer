package structs

// Record fields added in W6. Every field is omitempty and msgpack encodes
// structs as maps, so records written before these fields existed decode with
// them empty, and the readers fall back to the legacy shapes (a name that
// carries the evidence ID, a single range [Start, Start+Size)).

// IngestStateDuplicate marks an attempt whose content was already stored as
// another evidence, named by DuplicateOf. Its own relations and index records
// are being deleted; the alias record keeps the attempt's audit trail.
const IngestStateDuplicate IngestState = "duplicate"

// ContentModel values: how an evidence's records describe content.
const (
	// ContentModelV2 records carry explicit evidence and partition IDs,
	// paths and content references.
	ContentModelV2 = "v2"
)

// Source formats an evidence records.
const (
	SourceRaw    = "raw"    // a raw image or file, stored as is
	SourceEWF    = "ewf"    // an EWF set, stored decoded
	SourceVHD    = "vhd"    // a VHD/VHDX chain, stored decoded
	SourceOpaque = "opaque" // a container DUES cannot decode, stored as is
	// SourceClient: a path a server client reported for content already
	// stored; nothing was read from it.
	SourceClient = "client"
)

// EvidenceSource says where an evidence's stored bytes came from.
type EvidenceSource struct {
	Format string `msgpack:"format"`
	// Container names an unsupported container stored as is (vmdk, qcow2,
	// dmg, l01), or the container an image was decoded from.
	Container string         `msgpack:"container,omitempty"`
	Members   []SourceMember `msgpack:"members,omitempty"`
	EWF       *EWFCheck      `msgpack:"ewf,omitempty"`
}

// SourceMember is one file an evidence was read from.
type SourceMember struct {
	Path string `msgpack:"path"`
	Size int64  `msgpack:"size"`
}

// EWF digest verification results.
const (
	EWFVerified   = "verified"   // every stored digest matched
	EWFMismatch   = "mismatch"   // a stored digest did not match
	EWFUnverified = "unverified" // the set stores no digest
)

// EWFCheck is what an EWF set recorded about its acquisition, and what the
// ingest found when it decoded the set.
type EWFCheck struct {
	StoredMD5  []byte `msgpack:"stored_md5,omitempty"`
	StoredSHA1 []byte `msgpack:"stored_sha1,omitempty"`
	MD5        []byte `msgpack:"md5,omitempty"`
	SHA1       []byte `msgpack:"sha1,omitempty"`
	Result     string `msgpack:"result,omitempty"`
	// ChunkTablesInvalid counts chunk tables whose checksum failed in both
	// copies; their chunks were decoded unverified (PAR-18).
	ChunkTablesInvalid int `msgpack:"chunk_tables_invalid,omitempty"`
	// AcquisitionErrors are ranges the imager could not read.
	AcquisitionErrors []ByteRange `msgpack:"acquisition_errors,omitempty"`
}

// ByteRange is a range of an evidence.
type ByteRange struct {
	Offset int64 `msgpack:"offset"`
	Length int64 `msgpack:"length"`
}

// IndexState is where an evidence's index pass stands. Storage state is the
// separate IngestState: a COMPLETED evidence may have an index that failed.
type IndexState string

const (
	IndexRunning     IndexState = "running"
	IndexComplete    IndexState = "complete"
	IndexPartial     IndexState = "partial"     // some partitions or files could not be indexed
	IndexFailed      IndexState = "failed"      // the pass stopped with an error
	IndexInterrupted IndexState = "interrupted" // the process ended while the pass ran
	IndexSkipped     IndexState = "skipped"     // not indexed, by request or because nothing can be
	IndexInvalid     IndexState = "invalid"     // the index is known wrong and hidden (legacy raw-stored E01)
)

// IndexInfo is an evidence's index state and what the pass covered.
type IndexInfo struct {
	State IndexState `msgpack:"state"`
	// Reason says why a pass was skipped, failed or quarantined.
	Reason   string        `msgpack:"reason,omitempty"`
	Started  int64         `msgpack:"started,omitempty"` // Unix nanoseconds
	Finished int64         `msgpack:"finished,omitempty"`
	Coverage IndexCoverage `msgpack:"coverage"`
}

// IndexCoverage counts what an index pass saw and what it recorded:
// FilesSeen = FilesIndexed + the sum of Skipped.
type IndexCoverage struct {
	Partitions   int              `msgpack:"partitions,omitempty"`
	FilesSeen    int64            `msgpack:"files_seen,omitempty"`
	FilesIndexed int64            `msgpack:"files_indexed,omitempty"`
	DirsSeen     int64            `msgpack:"dirs_seen,omitempty"`
	Fragmented   int64            `msgpack:"fragmented,omitempty"`
	Inline       int64            `msgpack:"inline,omitempty"`
	Sparse       int64            `msgpack:"sparse,omitempty"`
	Deleted      int64            `msgpack:"deleted,omitempty"`
	Materialized int64            `msgpack:"materialized,omitempty"`
	Unavailable  int64            `msgpack:"unavailable,omitempty"`
	Skipped      map[string]int64 `msgpack:"skipped,omitempty"`
	// Symlinks are indexed as metadata; OtherSeen counts special files
	// (devices, FIFOs, sockets), which have no content and are not indexed.
	Symlinks  int64 `msgpack:"symlinks,omitempty"`
	OtherSeen int64 `msgpack:"other_seen,omitempty"`
}

// Skip counts a file the pass saw but did not index, by reason.
func (c *IndexCoverage) Skip(reason string) {
	if c.Skipped == nil {
		c.Skipped = map[string]int64{}
	}
	c.Skipped[reason]++
}

// PartitionMeta is what the scan found in one partition-table entry.
type PartitionMeta struct {
	// Ordinal is the entry's index in the partition table, or -1 for a
	// filesystem that spans the whole device.
	Ordinal  int    `msgpack:"ordinal"`
	TypeName string `msgpack:"type_name,omitempty"`
	Label    string `msgpack:"label,omitempty"`
	FSType   string `msgpack:"fs_type,omitempty"`
	Status   string `msgpack:"status,omitempty"` // indexed, partial, unknown, unsupported, encrypted, read_error, open_error
	Detail   string `msgpack:"detail,omitempty"`
}

// PartitionReport is what the scan of one partition saw and what went wrong
// (PR|||:<pid>). Counters are exact; Warnings counts every issue by code and
// Samples keeps the first few of each.
type PartitionReport struct {
	FilesSeen    int64                    `msgpack:"files_seen,omitempty"`
	FilesIndexed int64                    `msgpack:"files_indexed,omitempty"`
	DirsSeen     int64                    `msgpack:"dirs_seen,omitempty"`
	OtherSeen    int64                    `msgpack:"other_seen,omitempty"`
	Deleted      int64                    `msgpack:"deleted,omitempty"`
	Fragmented   int64                    `msgpack:"fragmented,omitempty"`
	Sparse       int64                    `msgpack:"sparse,omitempty"`
	Inline       int64                    `msgpack:"inline,omitempty"`
	Decoded      int64                    `msgpack:"decoded,omitempty"`
	Materialized int64                    `msgpack:"materialized,omitempty"`
	Unavailable  int64                    `msgpack:"unavailable,omitempty"`
	Truncated    int64                    `msgpack:"truncated,omitempty"`
	BytesData    int64                    `msgpack:"bytes_data,omitempty"`
	Warnings     map[string]int64         `msgpack:"warnings,omitempty"`
	Samples      map[string][]IssueSample `msgpack:"samples,omitempty"`
}

// IssueSample is one recorded parser issue.
type IssueSample struct {
	Code   string `msgpack:"code"`
	Path   string `msgpack:"path,omitempty"`
	Inode  uint64 `msgpack:"inode,omitempty"`
	Detail string `msgpack:"detail,omitempty"`
}

// Content kinds of an indexed file.
const (
	// ContentExtents: ranges of the evidence, with holes that read as zeros.
	ContentExtents = "extents"
	// ContentInline: the bytes are in the record (resident or inline data).
	ContentInline = "inline"
	// ContentMaterialized: the bytes exist only decoded (compressed, WOF,
	// decmpfs) and are stored as an object of their own (X|||).
	ContentMaterialized = "materialized"
	// ContentUnavailable: the file has content DUES could not read; the
	// record is metadata only.
	ContentUnavailable = "unavailable"
	// ContentNone: the file is empty.
	ContentNone = "none"
)

// Extent kinds, as core.ExtentKind.
const (
	ExtentData      uint8 = 0
	ExtentSparse    uint8 = 1
	ExtentUnwritten uint8 = 2
)

// Extent is one range of an indexed file's content. Offset is the evidence
// offset of a data extent; holes have none.
type Extent struct {
	//lint:ignore U1000 msgpack reads the struct's options from this field
	_msgpack   struct{} `msgpack:",as_array"`
	FileOffset int64
	Length     int64
	Offset     int64
	Kind       uint8
}

// ContentRef says where an indexed file's bytes are. Located bytes [0,
// Located) are described; a file whose content could only partly be found
// has Located < Size, and nothing past Located is ever filled in.
type ContentRef struct {
	Kind    string `msgpack:"kind"`
	Located int64  `msgpack:"located"`
	// Extents of an extents file. A file with more than MaxRecordExtents
	// keeps them in ExtentPages pages of their own (XE|||).
	Extents     []Extent `msgpack:"extents,omitempty"`
	ExtentPages int      `msgpack:"extent_pages,omitempty"`
	Inline      []byte   `msgpack:"inline,omitempty"`
	// ObjectID is the raw ID of a materialised file's object (X|||).
	ObjectID []byte `msgpack:"object_id,omitempty"`
}

// MaxRecordExtents is how many extents an indexed record carries itself, and
// how many each extent page holds.
const MaxRecordExtents = 1 << 16

// MaxInlineBytes is the largest content kept inline in a record; larger
// resident or inline data is materialised.
const MaxInlineBytes = 16 << 10

// ObjectRecord describes a materialised object: content stored through the
// chunk pipeline under its own ID, with its own relations.
type ObjectRecord struct {
	Size        int64  `msgpack:"size"`
	ChunkSize   int64  `msgpack:"chunk_size"`
	FileHash    string `msgpack:"file_hash"` // plain hash, base64
	EvidenceID  []byte `msgpack:"evidence_id"`
	IndexedID   []byte `msgpack:"indexed_id"`
	Description string `msgpack:"description,omitempty"` // what decoded it, e.g. "ntfs lznt1"
}

// AliasRecord is a later ingest of content already stored (D1). It is kept
// under the winner's ID and a hash of the path, so storing the same file again
// rewrites the same record.
type AliasRecord struct {
	Name      string         `msgpack:"name"`
	Path      string         `msgpack:"path"`
	Size      int64          `msgpack:"size"`
	Source    EvidenceSource `msgpack:"source"`
	AttemptID []byte         `msgpack:"attempt_id,omitempty"` // the duplicate record, now deleted
	At        int64          `msgpack:"at"`                   // Unix nanoseconds
	Merged    *MergeInfo     `msgpack:"merged,omitempty"`
}

// MergeInfo is where a merged evidence came from.
type MergeInfo struct {
	Package          string     `msgpack:"package"` // the package's ContentSHA256 (hex)
	SourceEvidenceID []byte     `msgpack:"source_evidence_id"`
	SourceFileHash   string     `msgpack:"source_file_hash"` // as recorded, in the source's algorithm
	SourceHashAlgo   string     `msgpack:"source_hash_algo"`
	SourceActor      string     `msgpack:"source_actor,omitempty"`
	SourceActorID    string     `msgpack:"source_actor_id,omitempty"`
	SourceKeyID      string     `msgpack:"source_key_id,omitempty"`
	Trusted          bool       `msgpack:"trusted,omitempty"` // the signature matched a pinned key
	At               int64      `msgpack:"at"`                // Unix nanoseconds
	Prior            *MergeInfo `msgpack:"prior,omitempty"`   // the source's own MergeInfo, one hop
}

// MergeLedgerEntry maps a merged source evidence to the target evidence that
// holds its content.
type MergeLedgerEntry struct {
	Target   []byte `msgpack:"target"`    // raw ID of the target evidence
	Root     string `msgpack:"root"`      // its ContentRoot when written; empty for legacy records
	FileHash string `msgpack:"file_hash"` // its FileHash when written
	Package  string `msgpack:"package"`   // the package that wrote the entry
	At       int64  `msgpack:"at"`
}
