package repository

import "indicer/lib/microartefact/model"

type Repository interface {
	Store(file model.FileRecord, artefacts []model.Artefact, relations []model.Relation) error
	Close() error
}

// Extraction statuses a file's indexed node records, so a run can tell a file
// examined before from one never examined, artefacts or not.
const (
	StatusExtracted   = "extracted"    // artefacts found
	StatusNone        = "none"         // examined, nothing found
	StatusUnsupported = "unsupported"  // no detector applies to its kind
	StatusParseFailed = "parse_failed" // its format's parser failed; text detectors ran
	StatusTooLarge    = "too_large"    // past the structured-parse limit; text detectors ran on its start
)

// Result is what extraction found in one file, as a repository records it.
type Result struct {
	Status string
	// Replace deletes the artefacts an earlier run stored for the file and
	// stores these instead.
	Replace bool
}

// ResultRepository records every file's extraction, whether or not it found
// artefacts, with the file's artefact count.
type ResultRepository interface {
	Record(file model.FileRecord, artefacts []model.Artefact, relations []model.Relation, result Result) error
}

type NopRepository struct{}

func (NopRepository) Store(model.FileRecord, []model.Artefact, []model.Relation) error {
	return nil
}

func (NopRepository) Close() error {
	return nil
}
