package enrichment

// Repository is injected into enrichment service so storage backend stays swappable.
type Repository interface {
	UpsertEvidence(record EvidenceRecord) error
	UpsertFile(record FileRecord) error
	ReadHierarchy() (*HierarchyTree, error)
	Close() error
}

// HierarchyTree represents the disk_image → partition → indexed_file hierarchy
// from the enrichment graphdb.
type HierarchyTree struct {
	DiskImages []*DiskImageNode
}

// DiskImageNode represents one evidence image in the enrichment graph.
type DiskImageNode struct {
	ID         string
	Name       string
	Partitions []*PartitionNode
}

// PartitionNode represents one partition within a disk image.
type PartitionNode struct {
	ID    string
	Name  string
	Files []*FileNode
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
}
