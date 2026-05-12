package enrichment

import (
	"crypto/sha256"
	"encoding/hex"
)

// FileRecord is a file-level enrichment payload destined for graphdb.
type FileRecord struct {
	Hash         string
	Name         string
	Path         string
	Size         int64
	IsDeleted    bool
	IsFragmented bool
	FileType     string

	DiskImageID   string
	DiskImageName string
	PartitionID   string
	PartitionName string
	IndexedFileID string
}

// EvidenceRecord is a top-level evidence object (input file) to be represented
// as a disk-image node in graphdb even when no partition/indexed parsing runs.
type EvidenceRecord struct {
	HashBase64 string
	Name       string
	Path       string
	Size       int64
}

func (record FileRecord) ensureIndexedFileID() string {
	if record.IndexedFileID != "" {
		return record.IndexedFileID
	}
	return buildIndexedFileID(record.PartitionID, record.Path, record.Hash)
}

func buildIndexedFileID(partitionID, path, fallback string) string {
	if partitionID != "" && path != "" {
		return hashText("path-node", partitionID, path)
	}
	if fallback != "" {
		return fallback
	}
	return hashText("path-node", partitionID, path)
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
