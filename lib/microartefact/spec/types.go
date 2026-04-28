package spec

import parsingpkg "indicer/lib/microartefact/parsing"

type DetectionRule struct {
	ID           string
	DetectorName string
	AppliesTo    []parsingpkg.Kind
	Enabled      bool
}

type RelationRule struct {
	ID            string
	BuilderName   string
	RelationType  string
	Deterministic bool
	Enabled       bool
	Confidence    float32
}

type Sheet struct {
	Title          string
	Version        string
	DetectionRules []DetectionRule
	RelationRules  []RelationRule
}
