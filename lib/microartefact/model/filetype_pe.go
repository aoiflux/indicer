package model

// PEMetadata is a placeholder for PE-specific file node metadata.
// Extend as PE parsing support is expanded.
type PEMetadata struct {
	Machine      string
	EntryPoint   uint64
	SectionCount int
}
