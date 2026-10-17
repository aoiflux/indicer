package store

import (
	"context"
	"encoding/base64"
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/logging"
	"indicer/lib/structs"
	"indicer/lib/util"
	"time"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// StoreInputFile stores infile, a partition or an evidence file, and returns
// only when nothing it started is still running, so the caller may close the
// database straight after (I6). A cancelled ctx stops an evidence store early.
func StoreInputFile(ctx context.Context, infile structs.InputFile) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if string(infile.GetNamespace()) == cnst.PartiFileNamespace {
		return storePartitionFile(infile)
	}
	return storeEvidenceFile(ctx, infile)
}
func EvidenceFilePreStoreCheck(infile structs.InputFile) error {
	return dbio.UpdateEvidence(infile.GetDB(), infile.GetID(), func(_ *badger.Txn, evidenceFile *structs.EvidenceFile) error {
		if !evidenceFile.Completed {
			return cnst.ErrIncompleteFile
		}
		if evidenceFile.Name == infile.GetName() {
			return dbio.ErrNoChange
		}
		evidenceFile.Name = infile.GetName()
		return nil
	})
}

// EnsureEvidenceFilePreStoreState makes sure the evidence metadata row exists
// and has a sane ingest state before asynchronous indexing starts.
func EnsureEvidenceFilePreStoreState(infile structs.InputFile) error {
	_, err := evidenceFilePreflight(infile)
	return err
}

func MarkEvidenceFileFailed(fileID []byte, db *badger.DB) error {
	err := dbio.UpdateEvidence(db, fileID, func(_ *badger.Txn, evidenceFile *structs.EvidenceFile) error {
		if evidenceFile.Completed || evidenceFile.Failed {
			return dbio.ErrNoChange
		}
		evidenceFile.Failed = true
		if evidenceFile.IngestState == "" {
			evidenceFile.IngestState = structs.IngestStatePending
		}
		return nil
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	return err
}

// MarkEvidenceFileFailedUnlessFlushed marks an ingest as failed unless it is
// already in FLUSHED state. FLUSHED ingests should remain recoverable so
// startup recovery/repair can auto-complete the final marker write.
func MarkEvidenceFileFailedUnlessFlushed(fileID []byte, db *badger.DB) error {
	err := dbio.UpdateEvidence(db, fileID, func(_ *badger.Txn, evidenceFile *structs.EvidenceFile) error {
		if evidenceFile.Completed || evidenceFile.Failed || evidenceFile.IngestState == structs.IngestStateFlushed {
			return dbio.ErrNoChange
		}
		evidenceFile.Failed = true
		if evidenceFile.IngestState == "" {
			evidenceFile.IngestState = structs.IngestStatePending
		}
		return nil
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	return err
}

// CompleteEvidenceFile atomically marks an evidence record as completed.
// It is used by the repair/recovery path to finish a FLUSHED ingest whose
// final COMPLETED marker was never written before a crash.
func CompleteEvidenceFile(fileID []byte, db *badger.DB) error {
	err := dbio.UpdateEvidence(db, fileID, func(_ *badger.Txn, evidenceFile *structs.EvidenceFile) error {
		if evidenceFile.Completed {
			return dbio.ErrNoChange
		}
		evidenceFile.Completed = true
		evidenceFile.IngestState = structs.IngestStateCompleted
		return nil
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	return err
}

func storePartitionFile(infile structs.InputFile) error {
	start := time.Now()
	logging.GetLogger().Info("storePartitionFile START",
		zap.String("file_name", infile.GetName()),
		zap.Int64("file_size", infile.GetSize()),
	)
	partitionKey := dbio.CanonicalPartitionKey(infile.GetID())
	encodedHash := base64.StdEncoding.EncodeToString(infile.GetFileHash())
	partitionLookupKey := util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, encodedHash)
	partitionHash := base64.StdEncoding.EncodeToString(infile.GetFileHash())
	partitionFile := structs.NewPartitionFile(
		infile.GetName(),
		infile.GetStartIndex(),
		infile.GetSize(),
		nil,
		partitionHash,
	)
	// Indexed files go to one key each, not into the record (ING-21).
	err := dbio.SetPartitionWithMembers(infile.GetDB(), infile.GetID(), partitionFile, infile.GetInternalObjects())
	if err != nil {
		logging.GetLogger().Error("storePartitionFile SET_FILE_ERROR", zap.Error(err))
		return err
	}
	if err := dbio.AppendHashLookupUUIDByKey(infile.GetDB(), partitionLookupKey, partitionKey); err != nil {
		logging.GetLogger().Error("storePartitionFile SET_LOOKUP_ERROR", zap.Error(err))
		return err
	}
	logging.GetLogger().Info("storePartitionFile COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("result", "created"),
	)
	return nil
}

func storeEvidenceFile(ctx context.Context, infile structs.InputFile) error {
	start := time.Now()
	logging.GetLogger().Info("storeEvidenceFile START",
		zap.String("file_name", infile.GetName()),
		zap.Int64("file_size", infile.GetSize()),
	)

	evidenceFile, err := evidenceFilePreflight(infile)
	if err != nil {
		logging.GetLogger().Error("storeEvidenceFile PREFLIGHT_ERROR", zap.Error(err))
		return err
	}
	if evidenceFile.Completed {
		logging.GetLogger().Info("storeEvidenceFile COMPLETE",
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.String("result", "already_completed"),
		)
		return nil
	}
	var hochoEvidenceHash string
	if cnst.StorePipelineMode == cnst.StorePipelineStaged {
		hochoEvidenceHash, err = storeEvidenceDataStaged(ctx, infile)
	} else {
		hochoEvidenceHash, err = storeEvidenceDataBatchOwner(ctx, infile)
	}
	if err != nil {
		return failEvidenceStore(infile, err)
	}
	if err := markEvidenceFileFlushed(infile, hochoEvidenceHash); err != nil {
		return err
	}
	logging.GetLogger().Info("storeEvidenceFile COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("result", "stored"),
	)
	return nil
}

func failEvidenceStore(infile structs.InputFile, storeErr error) error {
	if markErr := MarkEvidenceFileFailed(infile.GetID(), infile.GetDB()); markErr != nil {
		logging.GetLogger().Error("storeEvidenceFile MARK_FAILED_ERROR", zap.Error(markErr))
	}
	logging.GetLogger().Error("storeEvidenceFile STORE_DATA_ERROR", zap.Error(storeErr))
	return storeErr
}

func markEvidenceFileFlushed(infile structs.InputFile, hochoEvidenceHash string) error {
	// Mark as FLUSHED: all chunk data has been written to persistent storage.
	// The caller (cmdstore.go) atomically transitions this to COMPLETED.
	err := dbio.UpdateEvidence(infile.GetDB(), infile.GetID(), func(_ *badger.Txn, evidenceFile *structs.EvidenceFile) error {
		if cnst.StoreHashStrategy == cnst.HochoHashStrategy && hochoEvidenceHash != "" {
			evidenceFile.FileHash = hochoEvidenceHash
		}
		evidenceFile.IngestState = structs.IngestStateFlushed
		return nil
	})
	if err != nil {
		logging.GetLogger().Error("storeEvidenceFile MARK_FLUSHED_ERROR", zap.Error(err))
	}
	return err
}

func evidenceFilePreflight(infile structs.InputFile) (structs.EvidenceFile, error) {
	detectedType := util.DetectEvidenceType(infile.GetName(), infile.GetMappedFile())

	evidenceFile, created, err := loadOrCreateEvidenceFile(infile, detectedType)
	if err != nil || created {
		return evidenceFile, err
	}

	err = dbio.UpdateEvidence(infile.GetDB(), infile.GetID(), func(_ *badger.Txn, current *structs.EvidenceFile) error {
		changed := applyPreflight(current, infile.GetName(), detectedType)
		evidenceFile = *current
		if !changed {
			return dbio.ErrNoChange
		}
		return nil
	})
	return evidenceFile, err
}

// applyPreflight brings an existing evidence record into the state an ingest
// starts from, and reports whether anything changed:
//   - an unknown evidence type takes the detected one;
//   - a COMPLETED record's ingest state says so;
//   - an incomplete record is PENDING and no longer failed, ready for retry;
//   - a completed one is renamed to this ingest's name.
func applyPreflight(e *structs.EvidenceFile, name, detectedType string) bool {
	changed := false
	if (e.EvidenceType == "" || e.EvidenceType == cnst.UnknownEvidenceType) && detectedType != e.EvidenceType {
		e.EvidenceType = detectedType
		changed = true
	}
	if e.Completed && e.IngestState != structs.IngestStateCompleted {
		e.IngestState = structs.IngestStateCompleted
		changed = true
	}
	if !e.Completed {
		if e.IngestState == "" {
			e.IngestState = structs.IngestStatePending
			changed = true
		}
		if e.Failed {
			e.Failed = false
			changed = true
		}
		return changed
	}
	if e.Name != name {
		e.Name = name
		changed = true
	}
	return changed
}

func loadOrCreateEvidenceFile(infile structs.InputFile, detectedType string) (structs.EvidenceFile, bool, error) {
	evidenceFile, err := dbio.GetEvidenceFile(infile.GetID(), infile.GetDB())
	if errors.Is(err, badger.ErrKeyNotFound) {
		evidenceFile := structs.NewEvidenceFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
			infile.GetInternalObjects(),
			detectedType,
			"", // FileHash placeholder
		)
		err = dbio.CreateEvidence(infile.GetDB(), infile.GetID(), evidenceFile)
		return evidenceFile, true, err
	}
	if err != nil {
		return evidenceFile, false, err
	}
	return evidenceFile, false, nil
}

func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)
	_ = db

	// Relation keys are deterministic per (file hash, chunk index), so an upsert is
	// safe and avoids an extra DB read on the hot ingest path.
	return dbio.SetBatchNode(relKey, chash, batch)
}

// processRevRel records that chunk chash occurs at index in fhash, as one key
// of its own. Nothing is buffered or merged, so memory does not grow with the
// evidence and concurrent ingests never overwrite each other's relations
// (STO-14).
func processRevRel(index int64, fhash, chash []byte, batch *badger.WriteBatch) error {
	if !cnst.ENABLEREVREL {
		return nil
	}
	return dbio.SetReverseRelationAppendMember(chash, index, fhash, batch)
}
