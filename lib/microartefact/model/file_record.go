package model

import (
	"crypto/sha256"
	"encoding/hex"
)

// FileNodeType identifies which per-file-type node metadata model applies.
type FileNodeType string

const (
	FileNodeTypeGeneric FileNodeType = "generic"
	FileNodeTypeELF     FileNodeType = "elf"
	FileNodeTypePE      FileNodeType = "pe"
	FileNodeTypePDF     FileNodeType = "pdf"
	FileNodeTypeTXT     FileNodeType = "txt"
)

// FileRecord is the generic container-level record for an indexed file.
// Type-specific fields are stored in per-file-type metadata pointers.
type FileRecord struct {
	Hash         string
	Name         string
	Path         string
	Size         int64
	IsDeleted    bool
	IsFragmented bool
	FileType     string
	NodeType     FileNodeType

	GenericMeta *GenericFileMetadata
	ELFMeta     *ELFMetadata
	PEMeta      *PEMetadata
	PDFMeta     *PDFMetadata
	TXTMeta     *TXTMetadata

	EvidenceFileID   string
	EvidenceFileName string
	PartitionID      string
	PartitionName    string
	IndexedFileID    string
}

// BuildIndexedFileID derives an indexed-file ID from a partition and a path.
//
// Deprecated: the graph names an indexed file by the base64 of its record's
// ID, which enrichment writes and extraction looks up. This derivation matched
// no node enrichment wrote, so extraction found nothing (GRA-1); it remains
// only for the test fixtures written with it.
func BuildIndexedFileID(partitionID, path, fallback string) string {
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

// EffectiveNodeType resolves node type from explicit NodeType or legacy FileType.
func (f FileRecord) EffectiveNodeType() FileNodeType {
	if f.NodeType != "" {
		return f.NodeType
	}
	switch f.FileType {
	case "elf":
		return FileNodeTypeELF
	case "pe":
		return FileNodeTypePE
	case "pdf":
		return FileNodeTypePDF
	case "txt":
		return FileNodeTypeTXT
	}

	if f.ELFMeta != nil {
		return FileNodeTypeELF
	}
	if f.PEMeta != nil {
		return FileNodeTypePE
	}
	if f.PDFMeta != nil {
		return FileNodeTypePDF
	}
	if f.TXTMeta != nil {
		return FileNodeTypeTXT
	}
	if f.GenericMeta != nil {
		return FileNodeTypeGeneric
	}

	return FileNodeTypeGeneric
}
