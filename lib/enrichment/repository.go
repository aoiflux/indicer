package enrichment

import (
	"indicer/lib/hierarchy"
	"indicer/lib/microartefact/model"
)

// Repository is injected into enrichment service so storage backend stays swappable.
type Repository interface {
	UpsertEvidence(record EvidenceRecord) error
	UpsertFile(record FileRecord) error
	ReadHierarchy() (*HierarchyTree, error)
	Close() error
}

type HierarchyTree = hierarchy.HierarchyTree[*DiskImageNode]
type DiskImageNode = hierarchy.DiskImageNode[*PartitionNode]
type PartitionNode = hierarchy.PartitionNode[*FileNode]

// IndexedFileNode is a shared projection for indexed-file nodes in graphdb.
// Micro-artefact query paths can embed this to avoid redefining file-level
// graph node fields in multiple packages.
type IndexedFileNode struct {
	ID           string
	Name         string
	Path         string
	Size         int64
	Hash         string
	IsDeleted    bool
	IsFragmented bool
	FileType     string
	MimeType     string
	Tags         []string
}

// FileLevelArtefactNode is one micro-artefact detected inside an indexed file.
type FileLevelArtefactNode struct {
	model.MicroArtefactNodeFields
}

// IndexedFileWithArtefactsNode extends indexed-file projection with contained artefacts.
type IndexedFileWithArtefactsNode struct {
	IndexedFileNode
	Artefacts []*FileLevelArtefactNode
}

// FileNode represents one indexed file within a partition with enrichment metadata.
type FileNode struct {
	ID             string      // Unique ID for this node (indexed_file_id or file_node_id)
	Hash           string      // Hash of the indexed file (empty for file-level nodes)
	FileName       string      // Name of a file (for file-level nodes)
	Path           string      // Path of a file (for file-level nodes)
	FileNames      []string    // Names of files contained in this indexed file (for indexed-level nodes)
	FileLevelNodes []*FileNode // File-level nodes under this indexed file
	Size           int64       // Total size of the indexed file
	IsDeleted      bool        // Whether indexed file is marked deleted
	IsFragmented   bool        // Whether indexed file is fragmented
	FileType       string      // Type of indexed file (e.g., "exfat", "zip")
	MimeType       string      // MIME type for file-level nodes
	Tags           []string    // Classification tags for file-level nodes
}
