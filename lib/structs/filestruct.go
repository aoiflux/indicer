package structs

import "indicer/lib/cnst"

type baseFile struct {
	Names map[string]struct{} `msgpack:"names"`
	Size  int64               `msgpack:"size"`
}
type IndexedFile struct {
	baseFile
	Start       int64  `msgpack:"start"`
	IndexedType string `msgpack:"indexed_type"`
}

func NewIndexedFile(name string, start, size int64, indexedType string) IndexedFile {
	bfile := baseFile{Names: map[string]struct{}{name: {}}, Size: size}
	if indexedType == "" {
		indexedType = cnst.UnknownEvidenceType
	}
	return IndexedFile{baseFile: bfile, Start: start, IndexedType: indexedType}
}

type InternalOffset struct {
	Start int64
	End   int64
}
type PartitionFile struct {
	IndexedFile
	InternalObjects map[string]InternalOffset `msgpack:"internal_objects"`
}

func NewPartitionFile(name string, start, size int64, indexedFiles map[string]InternalOffset) PartitionFile {
	indexedFile := NewIndexedFile(name, start, size, cnst.UnknownEvidenceType)
	return PartitionFile{IndexedFile: indexedFile, InternalObjects: indexedFiles}
}

type EvidenceFile struct {
	PartitionFile
	EvidenceType string `msgpack:"evidence_type"`
	Completed    bool   `msgpack:"completed"`
}

func NewEvidenceFile(name string, start, size int64, partitions map[string]InternalOffset, evidenceType string) EvidenceFile {
	partitionFile := NewPartitionFile(name, start, size, partitions)
	return EvidenceFile{PartitionFile: partitionFile, EvidenceType: evidenceType, Completed: false}
}

type FileTypes interface {
	PartitionFile | EvidenceFile
}
