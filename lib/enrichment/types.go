package enrichment

import (
	"crypto/sha256"
	"encoding/hex"
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

func hashText(value string, rest ...string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(value))
	for _, next := range rest {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(next))
	}
	return hex.EncodeToString(h.Sum(nil))
}
