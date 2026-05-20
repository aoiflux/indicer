package hierarchy

// ContainerHierarchy is the shared top-level read model for container ancestry.
type ContainerHierarchy[EvidenceFile any] struct {
	EvidenceFiles []EvidenceFile
}

// EvidenceFileNode represents one top-level evidence file in the read model.
type EvidenceFileNode[PartitionFile any] struct {
	ID         string
	Name       string
	Partitions []PartitionFile
}

// PartitionFileNode represents one partition file under an evidence file.
type PartitionFileNode[IndexedFile any] struct {
	ID    string
	Name  string
	Files []IndexedFile
}
