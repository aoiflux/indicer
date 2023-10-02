package structs

type baseFile struct {
	Names []string `msgpack:"names"`
	Size  int64    `msgpack:"size"`
}
type IndexedFile struct {
	baseFile
	Start int64 `msgpack:"start"`
}

func NewIndexedFile(name string, start, size int64) IndexedFile {
	bfile := baseFile{Names: []string{name}, Size: size}
	return IndexedFile{baseFile: bfile, Start: start}
}

type PartitionFile struct {
	IndexedFile
	InternalObjects []string `msgpack:"internal_objects"`
}

func NewPartitionFile(name string, start, size int64, indexedFiles []string) PartitionFile {
	indexedFile := NewIndexedFile(name, start, size)
	return PartitionFile{IndexedFile: indexedFile, InternalObjects: indexedFiles}
}

type EvidenceFile struct {
	PartitionFile
	Completed bool `msgpack:"completed"`
}

func NewEvidenceFile(name string, start, size int64, partitions []string) EvidenceFile {
	partitionFile := NewPartitionFile(name, start, size, partitions)
	return EvidenceFile{PartitionFile: partitionFile, Completed: false}
}

type FileTypes interface {
	PartitionFile | EvidenceFile
}
