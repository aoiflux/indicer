package structs

type baseFile struct {
	// id    []byte
	Names []string `json:"names"`
	Size  int64    `json:"size"`
}

// func (b *baseFile) SetID(id []byte) {
// 	b.id = id
// }
// func (b baseFile) GetID() []byte {
// 	return b.id
// }

type IndexedFile struct {
	baseFile
	Start   int64 `json:"start"`
	End     int64 `json:"end"`
	DBStart int64 `json:"dbstart"`
}

func NewIndexedFile(name string, start, end, size int64) IndexedFile {
	bfile := baseFile{Names: []string{name}, Size: size}
	return IndexedFile{baseFile: bfile, Start: start, End: end, DBStart: -1}
}

type PartitionFile struct {
	IndexedFile
	InternalObjects [][]byte `json:"internal_objects"`
}

func NewPartitionFile(name string, start, end, size int64, indexedFileHashes [][]byte) PartitionFile {
	indexedFile := NewIndexedFile(name, start, end, size)
	return PartitionFile{IndexedFile: indexedFile, InternalObjects: indexedFileHashes}
}

type EvidenceFile struct {
	PartitionFile
	Completed bool `json:"completed"`
}
