package structs

import "indicer/lib/constant"

type baseFile struct {
	Names []string `msgpack:"names"`
	Size  int64    `msgpack:"size"`
}
type IndexedFile struct {
	baseFile
	Start   int64 `msgpack:"start"`
	DBStart int64 `msgpack:"dbstart"`
}

func NewIndexedFile(name string, start, size int64) IndexedFile {
	bfile := baseFile{Names: []string{name}, Size: size}
	return IndexedFile{baseFile: bfile, Start: start, DBStart: constant.IgnoreVar}
}

type PartitionFile struct {
	IndexedFile
	InternalObjects [][]byte `msgpack:"internal_objects"`
}

func NewPartitionFile(name string, start, size int64, indexedFiles [][]byte) PartitionFile {
	indexedFile := NewIndexedFile(name, start, size)
	return PartitionFile{IndexedFile: indexedFile, InternalObjects: indexedFiles}
}

type EvidenceFile struct {
	PartitionFile
	Completed bool `msgpack:"completed"`
}

func NewEvidenceFile(name string, start, size int64, partitions [][]byte) EvidenceFile {
	partitionFile := NewPartitionFile(name, start, size, partitions)
	return EvidenceFile{PartitionFile: partitionFile, Completed: false}
}
