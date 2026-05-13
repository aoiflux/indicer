package hierarchy

// HierarchyTree is the shared top-level read model for container ancestry.
type HierarchyTree[DiskImage any] struct {
	DiskImages []DiskImage
}

// DiskImageNode represents one evidence image in the read model.
type DiskImageNode[Partition any] struct {
	ID         string
	Name       string
	Partitions []Partition
}

// PartitionNode represents one partition within a disk image in the read model.
type PartitionNode[File any] struct {
	ID    string
	Name  string
	Files []File
}
