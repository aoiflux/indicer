package model

import "time"

// FileNodeFields holds file-node specific graph attributes.
type FileNodeFields struct {
	Name         string
	Path         string
	Size         int64
	Hash         string
	IsDeleted    bool
	IsFragmented bool
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

	// ObservedTime is when the artefact says the thing it describes happened —
	// an event record's timestamp, a binary's compile time — as opposed to when
	// it was extracted. Nil when the artefact carries no time of its own.
	ObservedTime *time.Time

	// Attributes are the detector's structured fields. They were written to the
	// graph from the start but never read back, which left the EVTX record
	// timestamps and registry key paths invisible to every consumer of the
	// hierarchy.
	Attributes map[string]string
}
