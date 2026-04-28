package model

// TXTMetadata is a placeholder for text-specific file node metadata.
// Extend as plain-text parsing support is expanded.
type TXTMetadata struct {
	LineCount    int
	Encoding     string
	LanguageHint string
}
