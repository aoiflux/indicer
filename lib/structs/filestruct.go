package structs

import "indicer/lib/cnst"

type baseFile struct {
	Names map[string]struct{} `msgpack:"names"`
	Size  int64               `msgpack:"size"`
}

type IndexedNameMeta struct {
	IsDeleted    bool `msgpack:"is_deleted"`
	IsFragmented bool `msgpack:"is_fragmented"`
}

type IndexedFile struct {
	baseFile
	Start       int64                      `msgpack:"start"`
	IndexedType string                     `msgpack:"indexed_type"`
	IsDeleted   bool                       `msgpack:"is_deleted"`
	NameMeta    map[string]IndexedNameMeta `msgpack:"name_meta"`
}

func NewIndexedFile(name string, start, size int64, indexedType string, isDeleted bool) IndexedFile {
	bfile := baseFile{Names: map[string]struct{}{name: {}}, Size: size}
	nameMeta := map[string]IndexedNameMeta{name: {IsDeleted: isDeleted}}
	if indexedType == "" {
		indexedType = cnst.UnknownEvidenceType
	}
	return IndexedFile{baseFile: bfile, Start: start, IndexedType: indexedType, IsDeleted: isDeleted, NameMeta: nameMeta}
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
	indexedFile := NewIndexedFile(name, start, size, cnst.UnknownEvidenceType, false)
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
