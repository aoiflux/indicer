package enrichment

import (
	"indicer/lib/graph/ids"
	"indicer/lib/structs"
)

// FileRecord is a file-level enrichment payload destined for graphdb.
// Creates FILE nodes at evidence file, partition, or indexed file level.
type FileRecord struct {
	Level string // "evidence_file", "partition", or "indexed_file" - identifies which level this file belongs to

	// File identity
	FileName string // The file name at this level
	Path     string // Full path (for indexed files)

	// File metadata
	Size         int64  // Size of this file
	IsDeleted    bool   // Whether file is marked deleted
	IsFragmented bool   // Whether file is fragmented
	FileType     string // Type (only set at indexed file level)
	MimeType     string // MIME type inferred from extension/type hints
	Tags         []string
	Entropy      float64
	HasEntropy   bool

	// Times are the filesystem MACB timestamps, nil where the source
	// filesystem does not record them or the evidence predates their capture.
	Times *structs.Timestamps

	// Start is the file's byte offset within its parent container. It is what
	// ties a graph node back to a location in the image, and without it a node
	// cannot be traced to the bytes it came from.
	Start int64

	// Simhash is the file-level similarity fingerprint, already computed and
	// cached during ingest but until now readable only by lib/near. HasSimhash
	// distinguishes "no fingerprint" from a legitimate zero.
	Simhash    uint64
	HasSimhash bool

	// Hierarchy references
	EvidenceFileID   string // ID/hash of parent evidence
	EvidenceFileName string // Name of parent evidence
	PartitionID      string // ID/hash of parent partition (empty for evidence_file level)
	PartitionName    string // Name of parent partition (empty for evidence_file level)
	IndexedFileID    string // ID/hash of indexed file (empty for evidence_file/partition level)
	IndexedFileHash  string // Hash of indexed file
}

// EvidenceRecord is a top-level evidence object (input file) to be represented
// as an evidence-file node in graphdb even when no partition/indexed parsing runs.
type EvidenceRecord struct {
	HashBase64 string
	Name       string
	Path       string
	Size       int64

	// EvidenceType is the acquisition format sniffed at ingest — E01, raw, vhd.
	// It says how the evidence was packaged, not what it contains.
	EvidenceType string

	// IngestState, Completed and Failed describe how far ingest got. An
	// examiner reading the graph needs to know whether a thin subtree means
	// "nothing there" or "we never finished reading it".
	IngestState string
	Completed   bool
	Failed      bool

	// PartitionCount is how many partitions this evidence yielded, so a graph
	// with none is distinguishable from one that was never parsed.
	PartitionCount int
}

// PartitionRecord represents a structural partition node that should exist
// even when partition payload parsing fails.
type PartitionRecord struct {
	EvidenceFileID   string
	EvidenceFileName string
	PartitionID      string
	PartitionName    string

	// Offset and Size locate the partition within the image; FilesystemType is
	// the filesystem detected inside it and Hash its content digest. All four
	// are parsed at ingest and were being dropped, leaving partition nodes with
	// nothing but an id and a name.
	Offset         int64
	Size           int64
	FilesystemType string
	Hash           string

	// IndexedFileCount is how many files the partition yielded. Zero on a
	// partition that failed to parse, which is why it is worth recording.
	IndexedFileCount int
}

func (record FileRecord) buildFileNodeID() string {
	identity := record.Path
	if identity == "" {
		identity = record.FileName
	}

	switch record.Level {
	case "evidence_file":
		return hashText("file-node", record.EvidenceFileID, identity)
	case "partition":
		return hashText("file-node", record.PartitionID, identity)
	case "indexed_file":
		return hashText("file-node", record.IndexedFileID, identity)
	default:
		return hashText("file-node", record.EvidenceFileID, identity)
	}
}

// hashText delegates to the shared derivation. It stays as a local name
// because the identifiers it produces are persisted, and keeping the call sites
// untouched makes it obvious that nothing about them changed.
func hashText(value string, rest ...string) string { return ids.HashText(value, rest...) }
