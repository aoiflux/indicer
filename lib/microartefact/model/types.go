package model

type FileRecord struct {
	Hash     string
	Name     string
	Path     string
	Size     int64
	FileType string

	DiskImageID   string
	DiskImageName string
	PartitionID   string
	PartitionName string
	IndexedFileID string
}

type Span struct {
	Start int64
	End   int64
}

type Artefact struct {
	Kind       string
	Detector   string
	Value      string
	Summary    string
	Confidence float32
	Span       Span
	Attributes map[string]string
}

type CanonicalNode struct {
	ID                 string
	Type               string
	CanonicalKey       string
	Timestamp          string
	Origin             string
	PayloadRef         string
	Features           map[string]string
	Confidence         float32
	Parser             string
	RawEvidenceSnippet string
}

type EvidenceField struct {
	FieldName string
	Parser    string
	RawValue  string
}

type Relation struct {
	FromKind      string
	FromValue     string
	ToKind        string
	ToValue       string
	RelationType  string
	Method        string
	Deterministic bool
	Confidence    float32
	Evidence      []EvidenceField
}
