package store

import (
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/logging"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
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
	orphans := make([][]byte, 0)
	eviPrefix := []byte(cnst.EviFileNamespace)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(eviPrefix); it.ValidForPrefix(eviPrefix); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, decodeErr := cnst.DECODER.DecodeAll(value, nil)
			if decodeErr == nil {
				value = decoded
			}

			var evidenceFile structs.EvidenceFile
			if err := msgpack.Unmarshal(value, &evidenceFile); err != nil {
				return fmt.Errorf("decode evidence file: %w", err)
			}

			report.ScannedEvidence++
			if evidenceFile.Completed || evidenceFile.Failed {
				continue
			}

			// Legacy incomplete records may not have ingest_state populated.
			state := evidenceFile.IngestState
			if state == "" {
				state = structs.IngestStatePending
			}

			if state == structs.IngestStatePending || state == structs.IngestStateFlushed {
				report.IncompleteFound++
				orphans = append(orphans, key)
			}
		}

		return nil
	})
	if err != nil {
		return report, err
	}

	for _, orphanID := range orphans {
		evidenceFile, err := dbio.GetEvidenceFile(orphanID, db)
		if err != nil {
			return report, err
		}
		if evidenceFile.Completed || evidenceFile.Failed {
			continue
		}
		if evidenceFile.IngestState == "" {
			evidenceFile.IngestState = structs.IngestStatePending
		}
		if evidenceFile.IngestState == structs.IngestStateFlushed {
			if err := ValidateFlushedEvidenceMaterialized(orphanID, evidenceFile, db); err == nil {
				// FLUSHED means all chunk data was committed; only the COMPLETED
				// marker was not written before the crash. Auto-complete only
				// after validating required chunk material is readable.
				evidenceFile.Completed = true
				evidenceFile.IngestState = structs.IngestStateCompleted
				if err := dbio.SetFile(orphanID, evidenceFile, db); err != nil {
					return report, err
				}
				report.CompletedFlushed++
				logging.GetLogger().Info("RecoverIncompleteIngests AUTO_COMPLETED",
					zap.String("file_name", firstCleanNameFromMap(evidenceFile.Names)),
				)
			} else {
				evidenceFile.Failed = true
				if err := dbio.SetFile(orphanID, evidenceFile, db); err != nil {
					return report, err
				}
				report.MarkedFailed++
				logging.GetLogger().Warn("RecoverIncompleteIngests FLUSHED_INVALID_MARKED_FAILED",
					zap.String("file_name", firstCleanNameFromMap(evidenceFile.Names)),
					zap.Error(err),
				)
			}
		} else {
			// PENDING means the ingest was interrupted mid-write; mark failed.
			evidenceFile.Failed = true
			if err := dbio.SetFile(orphanID, evidenceFile, db); err != nil {
				return report, err
			}
			report.MarkedFailed++
			logging.GetLogger().Warn("RecoverIncompleteIngests MARKED_FAILED",
				zap.String("file_name", firstCleanNameFromMap(evidenceFile.Names)),
				zap.String("ingest_state", string(evidenceFile.IngestState)),
			)
		}
	}

	logging.GetLogger().Info("RecoverIncompleteIngests COMPLETE",
		zap.Int("scanned_evidence", report.ScannedEvidence),
		zap.Int("incomplete_found", report.IncompleteFound),
		zap.Int("marked_failed", report.MarkedFailed),
		zap.Int("completed_flushed", report.CompletedFlushed),
	)

	return report, nil
}
