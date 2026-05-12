package enrichment

// Repository is injected into enrichment service so storage backend stays swappable.
type Repository interface {
	UpsertEvidence(record EvidenceRecord) error
	UpsertFile(record FileRecord) error
	Close() error
}
