package model

// PDFMetadata is a placeholder for PDF-specific file node metadata.
// Extend as PDF parsing support is expanded.
type PDFMetadata struct {
	Version       string
	PageCount     int
	Producer      string
	HasJavaScript bool
}
