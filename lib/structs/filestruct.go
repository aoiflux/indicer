package structs

type baseFile struct {
	Names []string `json:"names"`
	Size  int64    `json:"size"`
}
type IndexedFile struct {
	baseFile
	Start   int64 `json:"start"`
	DBStart int64 `json:"dbstart"`
}

func NewIndexedFile(name string, start, size int64) IndexedFile {
	bfile := baseFile{Names: []string{name}, Size: size}
	return IndexedFile{baseFile: bfile, Start: start, DBStart: -1}
}

type PartitionFile struct {
	IndexedFile
	InternalObjects [][]byte `json:"internal_objects"`
}

func NewPartitionFile(name string, start, size int64, indexedFileHashes [][]byte) PartitionFile {
	indexedFile := NewIndexedFile(name, start, size)
	return PartitionFile{IndexedFile: indexedFile, InternalObjects: indexedFileHashes}
}

type EvidenceFile struct {
	PartitionFile
	Completed bool `json:"completed"`
}
