package store

import (
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

func Store(infile structs.InputFile, errchan chan error) {
	if string(infile.GetNamespace()) == cnst.PartiFileNamespace {
		errchan <- storePartitionFile(infile)
	} else {
		errchan <- storeEvidenceFile(infile)
	}
}
func EvidenceFilePreStoreCheck(infile structs.InputFile) error {
	evidenceFile, err := dbio.GetEvidenceFile(infile.GetID(), infile.GetDB())
	if err != nil {
		return err
	}
	if !evidenceFile.Completed {
		return cnst.ErrIncompleteFile
	}
	if evidenceFile.Name == infile.GetName() {
		return nil
	}

	evidenceFile.Name = infile.GetName()
	return dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
}

// EnsureEvidenceFilePreStoreState makes sure the evidence metadata row exists
// and has a sane ingest state before asynchronous indexing starts.
func EnsureEvidenceFilePreStoreState(infile structs.InputFile) error {
	_, err := evidenceFilePreflight(infile)
	return err
}

func MarkEvidenceFileFailed(fileID []byte, db *badger.DB) error {
	evidenceFile, err := dbio.GetEvidenceFile(fileID, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if evidenceFile.Completed {
		return nil
	}
	if evidenceFile.Failed {
		return nil
	}

	evidenceFile.Failed = true
	if evidenceFile.IngestState == "" {
		evidenceFile.IngestState = structs.IngestStatePending
	}
	return dbio.SetFile(fileID, evidenceFile, db)
}

// MarkEvidenceFileFailedUnlessFlushed marks an ingest as failed unless it is
// already in FLUSHED state. FLUSHED ingests should remain recoverable so
// startup recovery/repair can auto-complete the final marker write.
func MarkEvidenceFileFailedUnlessFlushed(fileID []byte, db *badger.DB) error {
	evidenceFile, err := dbio.GetEvidenceFile(fileID, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if evidenceFile.Completed || evidenceFile.Failed {
		return nil
	}
	if evidenceFile.IngestState == structs.IngestStateFlushed {
		return nil
	}

	evidenceFile.Failed = true
	if evidenceFile.IngestState == "" {
		evidenceFile.IngestState = structs.IngestStatePending
	}
	return dbio.SetFile(fileID, evidenceFile, db)
}

// CompleteEvidenceFile atomically marks an evidence record as completed.
// It is used by the repair/recovery path to finish a FLUSHED ingest whose
// final COMPLETED marker was never written before a crash.
func CompleteEvidenceFile(fileID []byte, db *badger.DB) error {
	evidenceFile, err := dbio.GetEvidenceFile(fileID, db)
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if evidenceFile.Completed {
		return nil
	}

	evidenceFile.Completed = true
	evidenceFile.IngestState = structs.IngestStateCompleted
	return dbio.SetFile(fileID, evidenceFile, db)
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
		infile.GetInternalObjects(),
		partitionHash,
	)
	err := dbio.SetFile(partitionKey, partitionFile, infile.GetDB())
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

func storeEvidenceFile(infile structs.InputFile) error {
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
		hochoEvidenceHash, err = storeEvidenceDataStaged(infile)
	} else {
		hochoEvidenceHash, err = storeEvidenceDataBatchOwner(infile)
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
	evidenceFile, err := dbio.GetEvidenceFile(infile.GetID(), infile.GetDB())
	if err != nil {
		logging.GetLogger().Error("storeEvidenceFile GET_EVIDENCE_AFTER_STORE_ERROR", zap.Error(err))
		return err
	}
	if cnst.StoreHashStrategy == cnst.HochoHashStrategy && hochoEvidenceHash != "" {
		evidenceFile.FileHash = hochoEvidenceHash
	}
	evidenceFile.IngestState = structs.IngestStateFlushed
	err = dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
	if err != nil {
		logging.GetLogger().Error("storeEvidenceFile MARK_FLUSHED_ERROR", zap.Error(err))
	}
	return err
}

func evidenceFilePreflight(infile structs.InputFile) (structs.EvidenceFile, error) {
	detectedType := util.DetectEvidenceType(infile.GetName(), infile.GetMappedFile())

	evidenceFile, created, err := loadOrCreateEvidenceFile(infile, detectedType)
	if err != nil {
		return evidenceFile, err
	}
	if created {
		return evidenceFile, nil
	}

	if err := maybeUpdateEvidenceType(infile, &evidenceFile, detectedType); err != nil {
		return evidenceFile, err
	}

	if err := maybeSyncCompletedIngestState(infile, &evidenceFile); err != nil {
		return evidenceFile, err
	}

	if !evidenceFile.Completed {
		return ensureIncompleteEvidenceState(infile, evidenceFile)
	}

	return ensureEvidenceAlias(infile, evidenceFile)
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
		err = dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
		return evidenceFile, true, err
	}
	if err != nil {
		return evidenceFile, false, err
	}
	return evidenceFile, false, nil
}

func maybeUpdateEvidenceType(infile structs.InputFile, evidenceFile *structs.EvidenceFile, detectedType string) error {
	if evidenceFile.EvidenceType != "" && evidenceFile.EvidenceType != cnst.UnknownEvidenceType {
		return nil
	}
	if detectedType == evidenceFile.EvidenceType {
		return nil
	}
	evidenceFile.EvidenceType = detectedType
	return dbio.SetFile(infile.GetID(), *evidenceFile, infile.GetDB())
}

func maybeSyncCompletedIngestState(infile structs.InputFile, evidenceFile *structs.EvidenceFile) error {
	if !evidenceFile.Completed || evidenceFile.IngestState == structs.IngestStateCompleted {
		return nil
	}
	evidenceFile.IngestState = structs.IngestStateCompleted
	return dbio.SetFile(infile.GetID(), *evidenceFile, infile.GetDB())
}

func ensureIncompleteEvidenceState(infile structs.InputFile, evidenceFile structs.EvidenceFile) (structs.EvidenceFile, error) {
	needsUpdate := false
	if evidenceFile.IngestState == "" {
		evidenceFile.IngestState = structs.IngestStatePending
		needsUpdate = true
	}
	if evidenceFile.Failed {
		evidenceFile.Failed = false
		needsUpdate = true
	}
	if !needsUpdate {
		return evidenceFile, nil
	}
	err := dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
	return evidenceFile, err
}

func ensureEvidenceAlias(infile structs.InputFile, evidenceFile structs.EvidenceFile) (structs.EvidenceFile, error) {
	if evidenceFile.Name == infile.GetName() {
		return evidenceFile, nil
	}
	evidenceFile.Name = infile.GetName()
	err := dbio.SetFile(infile.GetID(), evidenceFile, infile.GetDB())
	return evidenceFile, err
}

func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)
	_ = db

	// Relation keys are deterministic per (file hash, chunk index), so an upsert is
	// safe and avoids an extra DB read on the hot ingest path.
	return dbio.SetBatchNode(relKey, chash, batch)
}
func processRevRel(index int64, fhash, chash []byte, batch *badger.WriteBatch, revRelBuffer *revRelAppendBuffer) error {
	if !cnst.ENABLEREVREL {
		return nil
	}
	if revRelBuffer != nil {
		revRelBuffer.add(chash, index)
	} else if err := dbio.SetReverseRelationAppendMember(chash, index, fhash, batch); err != nil {
		return err
	}
	return nil
}
