package model

// FileNodeFields holds file-node specific graph attributes.
type FileNodeFields struct {
	Name string
	Path string
	Size int64
}

// MicroArtefactNodeFields holds micro-artefact specific graph attributes.
type MicroArtefactNodeFields struct {
	Kind       string
	Detector   string
	Value      string
	Summary    string
	Confidence float32
	Start      int64
	End        int64
}
