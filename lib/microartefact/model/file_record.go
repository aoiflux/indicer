package model

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
	Hash     string
	Name     string
	Path     string
	Size     int64
	FileType string
	NodeType FileNodeType

	GenericMeta *GenericFileMetadata
	ELFMeta     *ELFMetadata
	PEMeta      *PEMetadata
	PDFMeta     *PDFMetadata
	TXTMeta     *TXTMetadata

	DiskImageID   string
	DiskImageName string
	PartitionID   string
	PartitionName string
	IndexedFileID string
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
