package store

import (
	"cmp"
	"errors"
	"fmt"

	"indicer/lib/logging"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

type RecoveryReport struct {
	ScannedEvidence  int `json:"scannedEvidence"`
	IncompleteFound  int `json:"incompleteFound"`
	MarkedFailed     int `json:"markedFailed"`
	CompletedFlushed int `json:"completedFlushed"`
}

// RecoverIncompleteIngests scans all evidence records and marks orphaned
// in-progress ingests as failed so retries and repair flows are deterministic.
func RecoverIncompleteIngests(db *badger.DB) (RecoveryReport, error) {
	var report RecoveryReport

	type orphanRecord struct {
		key  []byte
		file structs.EvidenceFile
	}
	orphans := make([]orphanRecord, 0)
	err := db.View(func(txn *badger.Txn) error {
		return forEachEvidenceRecord(txn, func(key []byte, evidenceFile structs.EvidenceFile) error {
			report.ScannedEvidence++
			if evidenceFile.Completed || evidenceFile.Failed {
				return nil
			}

			// Legacy incomplete records may not have ingest_state populated.
			state := evidenceFile.IngestState
			if state == "" {
				state = structs.IngestStatePending
			}

			if state == structs.IngestStatePending || state == structs.IngestStateFlushed {
				report.IncompleteFound++
				orphans = append(orphans, orphanRecord{key: key, file: evidenceFile})
			}

			return nil
		})
	})
	if err != nil {
		return report, err
	}

	// Each change below is a read-modify-write of the current record, which
	// skips a record another writer already finished or failed. Only material
	// known to be missing fails a FLUSHED ingest; a read error leaves it
	// FLUSHED for the next writable open and is returned (ING-10).
	var readErrs []error
	for _, orphan := range orphans {
		orphanID := orphan.key
		evidenceFile := orphan.file
		if evidenceFile.IngestState == structs.IngestStateFlushed {
			err := validateFlushed(orphanID, evidenceFile, db)
			if err != nil && !errors.Is(err, ErrMaterialMissing) {
				readErrs = append(readErrs, fmt.Errorf("validate flushed evidence %s: %w", firstCleanName(evidenceFile.Name), err))
				continue
			}
			if err == nil {
				// FLUSHED means chunk/relation dependencies were written; only the
				// COMPLETED marker was not written before the crash. Auto-complete
				// only after validating required dependencies are present.
				if err := CompleteEvidenceFile(orphanID, db); err != nil {
					return report, err
				}
				report.CompletedFlushed++
				logging.GetLogger().Info("RecoverIncompleteIngests AUTO_COMPLETED",
					zap.String("file_name", firstCleanName(evidenceFile.Name)),
				)
			} else {
				if err := MarkEvidenceFileFailed(orphanID, db); err != nil {
					return report, err
				}
				report.MarkedFailed++
				logging.GetLogger().Warn("RecoverIncompleteIngests FLUSHED_INVALID_MARKED_FAILED",
					zap.String("file_name", firstCleanName(evidenceFile.Name)),
					zap.Error(err),
				)
			}
		} else {
			// PENDING means the ingest was interrupted mid-write; mark failed.
			if err := MarkEvidenceFileFailed(orphanID, db); err != nil {
				return report, err
			}
			report.MarkedFailed++
			logging.GetLogger().Warn("RecoverIncompleteIngests MARKED_FAILED",
				zap.String("file_name", firstCleanName(evidenceFile.Name)),
				zap.String("ingest_state", string(cmp.Or(evidenceFile.IngestState, structs.IngestStatePending))),
			)
		}
	}

	logging.GetLogger().Info("RecoverIncompleteIngests COMPLETE",
		zap.Int("scanned_evidence", report.ScannedEvidence),
		zap.Int("incomplete_found", report.IncompleteFound),
		zap.Int("marked_failed", report.MarkedFailed),
		zap.Int("completed_flushed", report.CompletedFlushed),
	)

	return report, errors.Join(readErrs...)
}
