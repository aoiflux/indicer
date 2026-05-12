package enrichment

import (
	"crypto/sha256"
	"encoding/hex"
)

// FileRecord is a file-level enrichment payload destined for graphdb.
// Creates FILE nodes at disk image, partition, or indexed file level.
type FileRecord struct {
	Level string // "disk_image", "partition", or "indexed_file" - identifies which level this file belongs to

	// File identity
	FileName string // The file name at this level
	Path     string // Full path (for indexed files)

	// File metadata
	Size         int64  // Size of this file
	IsDeleted    bool   // Whether file is marked deleted
	IsFragmented bool   // Whether file is fragmented
	FileType     string // Type (only set at indexed file level)

	// Hierarchy references
	DiskImageID     string // ID/hash of parent evidence
	DiskImageName   string // Name of parent evidence
	PartitionID     string // ID/hash of parent partition (empty for disk_image level)
	PartitionName   string // Name of parent partition (empty for disk_image level)
	IndexedFileID   string // ID/hash of indexed file (empty for disk_image/partition level)
	IndexedFileHash string // Hash of indexed file
}

// EvidenceRecord is a top-level evidence object (input file) to be represented
// as a disk-image node in graphdb even when no partition/indexed parsing runs.
type EvidenceRecord struct {
	HashBase64 string
	Name       string
	Path       string
	Size       int64
}

func (record FileRecord) buildFileNodeID() string {
	switch record.Level {
	case "disk_image":
		return hashText("file-node", record.DiskImageID, record.FileName)
	case "partition":
		return hashText("file-node", record.PartitionID, record.FileName)
	case "indexed_file":
		return hashText("file-node", record.IndexedFileID, record.FileName)
	default:
		return hashText("file-node", record.DiskImageID, record.FileName)
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
