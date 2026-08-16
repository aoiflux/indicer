package model

import "time"

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

	// ObservedTime is when the thing the artefact describes happened, as
	// distinct from when it was extracted — an event record's own timestamp, a
	// binary's compile time. Nil for the majority of artefacts, which are
	// pattern matches with no time of their own.
	//
	// Detectors that parse a structured record used to strand these in
	// Attributes as formatted strings, which made them unusable for a timeline.
	ObservedTime *time.Time
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
