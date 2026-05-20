package cli

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/enrichment"
	"indicer/lib/logging"
	"indicer/lib/parser"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/edsrzf/mmap-go"
	"go.uber.org/zap"
)

func StoreData(chonkSize int, dbpath, evipath string, key []byte, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool) error {
	start := time.Now()
	logging.GetLogger().Info("StoreData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
		zap.String("evidence_path", evipath),
		zap.Bool("sync_index", syncIndex),
		zap.Bool("no_index", noIndex),
		zap.Bool("enable_fts", enableFTS),
		zap.Bool("enable_enrichment", enableEnrichment),
	)

	db, dbpath, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("StoreData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	err = util.EnsureBlobPath(dbpath)
	if err != nil {
		logging.GetLogger().Error("StoreData ENSURE_BLOB_PATH_ERROR", zap.Error(err), zap.String("db_path", dbpath))
		return err
	}

	finfo, err := os.Stat(evipath)
	if err != nil {
		logging.GetLogger().Error("StoreData STAT_ERROR", zap.Error(err), zap.String("evidence_path", evipath))
		return err
	}

	if finfo.IsDir() {
		logging.GetLogger().Info("StoreData STORE_FOLDER_EXECUTE", zap.String("evidence_path", evipath))
		fmt.Println("Storing Entire Folder")
		err = StoreFolder(chonkSize, evipath, key, syncIndex, noIndex, enableFTS, enableEnrichment, db)
		if err != nil {
			logging.GetLogger().Error("StoreData STORE_FOLDER_ERROR", zap.Error(err), zap.String("evidence_path", evipath))
			return err
		}
	}
	logging.GetLogger().Info("StoreData STORE_FILE_EXECUTE", zap.String("evidence_path", evipath))
	err = StoreFile(chonkSize, evipath, key, syncIndex, noIndex, enableFTS, enableEnrichment, db)
	if err != nil {
		logging.GetLogger().Error("StoreData STORE_FILE_ERROR", zap.Error(err), zap.String("evidence_path", evipath))
		return err
	}

	err = db.Close()
	if err != nil {
		logging.GetLogger().Error("StoreData DB_CLOSE_ERROR", zap.Error(err))
		return err
	}
	logging.GetLogger().Info("StoreData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

func StoreFolder(chonkSize int, evidir string, key []byte, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) error {
	start := time.Now()
	logging.GetLogger().Info("StoreFolder START",
		zap.Int("chonk_size", chonkSize),
		zap.String("evidence_dir", evidir),
		zap.Bool("sync_index", syncIndex),
		zap.Bool("no_index", noIndex),
		zap.Bool("enable_fts", enableFTS),
		zap.Bool("enable_enrichment", enableEnrichment),
	)

	err := filepath.Walk(evidir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			logging.GetLogger().Error("StoreFolder WALK_ERROR", zap.Error(err), zap.String("file_path", path))
			return err
		}

		if info.IsDir() {
			return nil
		}

		return StoreFile(chonkSize, path, key, syncIndex, noIndex, enableFTS, enableEnrichment, db)
	})

	if err != nil {
		logging.GetLogger().Error("StoreFolder STORE_ERROR", zap.Error(err), zap.String("evidence_dir", evidir))
		return err
	}

	fmt.Println()
	fmt.Println("Folder Store Time: ", time.Since(start))
	logging.GetLogger().Info("StoreFolder COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

func StoreFile(chonkSize int, evipath string, key []byte, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) error {
	start := time.Now()
	logging.GetLogger().Info("StoreFile START",
		zap.Int("chonk_size", chonkSize),
		zap.String("file_path", evipath),
		zap.Bool("sync_index", syncIndex),
		zap.Bool("no_index", noIndex),
		zap.Bool("enable_fts", enableFTS),
		zap.Bool("enable_enrichment", enableEnrichment),
	)

	info, err := os.Stat(evipath)
	if err != nil {
		logging.GetLogger().Error("StoreFile STAT_ERROR", zap.Error(err), zap.String("file_path", evipath))
		return err
	}
	if info.IsDir() {
		logging.GetLogger().Debug("StoreFile SKIP_DIRECTORY", zap.String("file_path", evipath))
		return nil
	}

	fmt.Println("Pre-store checks....")
	logging.GetLogger().Debug("StoreFile INIT_EVIDENCE_FILE", zap.String("file_path", evipath))
	eviFile, err := initEvidenceFile(evipath, db)
	if err != nil {
		logging.GetLogger().Error("StoreFile INIT_EVIDENCE_FILE_ERROR", zap.Error(err), zap.String("file_path", evipath))
		return err
	}
	err = store.EvidenceFilePreStoreCheck(eviFile)
	if err != nil && err != badger.ErrKeyNotFound && err != cnst.ErrIncompleteFile {
		logging.GetLogger().Error("StoreFile PRESTORE_CHECK_ERROR", zap.Error(err))
		return err
	}
	if err == nil {
		if enableEnrichment {
			logging.GetLogger().Debug("StoreFile ENRICH_EXISTING_EVIDENCE")
			err := enrichEvidenceNode(db, eviFile, evipath)
			if err != nil {
				logging.GetLogger().Error("StoreFile ENRICH_EXISTING_EVIDENCE_ERROR", zap.Error(err))
				return err
			}
		}
		logging.GetLogger().Info("StoreFile COMPLETE",
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.String("result", "already_stored"),
		)
		return nil
	}

	idxErrCh, err := startIndexing(eviFile, db, noIndex, syncIndex, enableFTS, enableEnrichment)
	if err != nil {
		logging.GetLogger().Error("StoreFile INDEX_ERROR", zap.Error(err))
		return err
	}

	eviname := filepath.Base(evipath)
	fmt.Printf("\nSaving Evidence File: %s\n", eviname)

	echan := make(chan error)
	go store.Store(eviFile, echan)
	err = <-echan
	if err != nil {
		drainIndexError(idxErrCh)
		markEvidenceFileFailed(eviFile.GetID(), db)
		logging.GetLogger().Error("StoreFile STORE_ERROR", zap.Error(err))
		return err
	}

	err = transitionEvidenceFileToCompleted(eviFile, eviname, db, idxErrCh)
	if err != nil {
		return err
	}

	if idxErr := awaitIndexError(idxErrCh); idxErr != nil {
		logging.GetLogger().Error("StoreFile INDEX_ERROR", zap.Error(idxErr))
		return idxErr
	}

	if enableEnrichment {
		logging.GetLogger().Debug("StoreFile ENRICH_NEW_EVIDENCE")
		err := enrichEvidenceNode(db, eviFile, evipath)
		if err != nil {
			logging.GetLogger().Error("StoreFile ENRICH_NEW_EVIDENCE_ERROR", zap.Error(err))
			return err
		}
	}

	err = closeInputFileResources(eviFile)
	if err != nil {
		return err
	}

	fmt.Printf("\nStored in: %v\n\n", time.Since(start))
	logging.GetLogger().Info("StoreFile COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("result", "stored"),
	)
	return nil
}

func startIndexing(eviFile structs.InputFile, db *badger.DB, noIndex bool, syncIndex bool, enableFTS bool, enableEnrichment bool) (chan error, error) {
	if noIndex {
		return nil, nil
	}

	if syncIndex {
		logging.GetLogger().Info("StoreFile INDEX_EXECUTE_SYNC")
		err := indexEvidenceFile(eviFile, db, enableFTS, enableEnrichment)
		if err != nil {
			return nil, err
		}
		return nil, nil
	}

	logging.GetLogger().Info("StoreFile INDEX_EXECUTE_ASYNC")
	idxErrCh := make(chan error, 1)
	go func() {
		idxErrCh <- indexEvidenceFile(eviFile, db, enableFTS, enableEnrichment)
	}()

	return idxErrCh, nil
}

func transitionEvidenceFileToCompleted(eviFile structs.InputFile, eviname string, db *badger.DB, idxErrCh chan error) error {
	// Verify that all chunk data has been flushed to persistent storage.
	eviNode, err := dbio.GetEvidenceFile(eviFile.GetID(), eviFile.GetDB())
	if err != nil {
		markEvidenceFileFailed(eviFile.GetID(), db)
		logging.GetLogger().Error("StoreFile GET_EVIDENCE_NODE_ERROR", zap.Error(err))
		return err
	}

	if eviNode.IngestState != structs.IngestStateFlushed {
		markEvidenceFileFailed(eviFile.GetID(), db)
		stateErr := fmt.Errorf("ingest state transition violation: expected %s, got %s", structs.IngestStateFlushed, eviNode.IngestState)
		logging.GetLogger().Error("StoreFile INGEST_STATE_VIOLATION", zap.Error(stateErr))
		return stateErr
	}

	// Atomically transition to COMPLETED state so recovery can detect incomplete ingest.
	eviNode.Completed = true
	eviNode.Failed = false
	eviNode.IngestState = structs.IngestStateCompleted
	err = dbio.SetFile(eviFile.GetID(), eviNode, eviFile.GetDB())
	if err != nil {
		drainIndexError(idxErrCh)
		markEvidenceFileFailed(eviFile.GetID(), db)
		logging.GetLogger().Error("StoreFile SET_EVIDENCE_NODE_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Debug("StoreFile INGEST_ATOMIC_COMMITTED",
		zap.String("file_name", eviname),
		zap.String("ingest_state", string(structs.IngestStateCompleted)),
	)

	return nil
}

func awaitIndexError(idxErrCh chan error) error {
	if idxErrCh == nil {
		return nil
	}

	idxErr := <-idxErrCh
	if idxErr != nil && !errors.Is(idxErr, cnst.ErrIncompatibleFileSystem) {
		return idxErr
	}

	return nil
}

func drainIndexError(idxErrCh chan error) {
	if idxErrCh != nil {
		<-idxErrCh
	}
}

func markEvidenceFileFailed(fileID []byte, db *badger.DB) {
	if markErr := store.MarkEvidenceFileFailedUnlessFlushed(fileID, db); markErr != nil {
		logging.GetLogger().Error("StoreFile MARK_FAILED_ERROR", zap.Error(markErr))
	}
}

func closeInputFileResources(eviFile structs.InputFile) error {
	mappedFile := eviFile.GetMappedFile()
	err := mappedFile.Unmap()
	if err != nil {
		logging.GetLogger().Error("StoreFile UNMAP_ERROR", zap.Error(err))
		return err
	}

	err = eviFile.GetHandle().Close()
	if err != nil {
		logging.GetLogger().Error("StoreFile HANDLE_CLOSE_ERROR", zap.Error(err))
		return err
	}

	return nil
}

func enrichEvidenceNode(db *badger.DB, eviFile structs.InputFile, evipath string) error {
	logging.GetLogger().Debug("enrichEvidenceNode START", zap.String("file_path", evipath))
	repo, err := enrichment.OpenGrapheneRepository(db.Opts().Dir)
	if err != nil {
		logging.GetLogger().Error("enrichEvidenceNode OPEN_GRAPH_REPOSITORY_ERROR", zap.Error(err))
		return err
	}
	service := enrichment.NewService(db, repo)
	defer service.Close()

	evidenceHashB64, err := eviFile.GetEncodedHash()
	if err != nil {
		logging.GetLogger().Error("enrichEvidenceNode ENCODE_HASH_ERROR", zap.Error(err))
		return err
	}

	err = service.EnrichEvidence(enrichment.EvidenceRecord{
		HashBase64: string(evidenceHashB64),
		Name:       filepath.Base(evipath),
		Path:       evipath,
		Size:       eviFile.GetSize(),
	})
	if err != nil {
		logging.GetLogger().Error("enrichEvidenceNode ENRICH_ERROR", zap.Error(err))
		return err
	}
	logging.GetLogger().Debug("enrichEvidenceNode COMPLETE", zap.String("file_path", evipath))
	return nil
}

func indexEvidenceFile(eviFile structs.InputFile, db *badger.DB, enableFTS bool, enableEnrichment bool) error {
	logging.GetLogger().Info("indexEvidenceFile START",
		zap.String("file_name", eviFile.GetName()),
		zap.Bool("enable_fts", enableFTS),
		zap.Bool("enable_enrichment", enableEnrichment),
	)
	tuskJSON, hasTusk := parser.TuskAnalysis(eviFile.GetHandle().Name())
	partitions := parser.ParseImage(tuskJSON, hasTusk, eviFile.GetSize(), eviFile.GetHandle())
	idxChan := make(chan error)

	var enrichRepo *enrichment.GrapheneRepository
	if enableEnrichment {
		var err error
		enrichRepo, err = enrichment.OpenGrapheneRepository(db.Opts().Dir)
		if err != nil {
			logging.GetLogger().Error("indexEvidenceFile OPEN_GRAPH_REPOSITORY_ERROR", zap.Error(err))
			return err
		}
		defer enrichRepo.Close()
	}
	logging.GetLogger().Debug("indexEvidenceFile PARTITION_PARSE_COMPLETE", zap.Int("partition_count", len(partitions)))

	for index, partition := range partitions {
		phash := eviFile.GetHash()
		var err error
		if partition.Start != 0 && partition.Size != eviFile.GetSize() {
			phash, err = util.GetLogicalFileHash(eviFile.GetHandle(), cnst.GetHashAlgo(true), partition.Start, partition.Size, true)
			if err != nil {
				logging.GetLogger().Error("indexEvidenceFile PARTITION_HASH_ERROR", zap.Error(err), zap.Int("partition_index", index))
				return err
			}
		}
		eviFile.UpdateInternalObjects(partition.Start, partition.Size, phash)

		ehash, err := eviFile.GetEncodedHash()
		if err != nil {
			logging.GetLogger().Error("indexEvidenceFile ENCODE_HASH_ERROR", zap.Error(err), zap.Int("partition_index", index))
			return err
		}
		pname := string(util.AppendToBytesSlice(ehash, cnst.DataSeperator, eviFile.GetName(), "_", cnst.PartitionIndexPrefix, index))
		pfile := structs.NewInputFile(
			db,
			eviFile.GetHandle(),
			eviFile.GetMappedFile(),
			pname,
			cnst.PartiFileNamespace,
			phash,
			partition.Size,
			partition.Start,
		)

		if enableEnrichment {
			err := upsertPartitionNodeForUnparsedPartition(enrichRepo, eviFile, pfile)
			if err != nil {
				logging.GetLogger().Error("indexEvidenceFile UPSERT_PARTITION_ERROR", zap.Error(err), zap.Int("partition_index", index))
				return err
			}
		}

		if hasTusk {
			go parser.IndexFilesystem(tuskJSON, pfile, idxChan, enableFTS, enableEnrichment)
		} else {
			go parser.IndexEXFAT(pfile, idxChan, enableFTS, enableEnrichment)
		}
		// Use select so that if the goroutine finishes before we send the
		// start signal (e.g. empty partition with no files), we receive the
		// result directly instead of deadlocking on the send.
		select {
		case idxChan <- nil:
			err = <-idxChan
		case err = <-idxChan:
		}
		if errors.Is(err, cnst.ErrIncompatibleFile) {
			continue
		}
		if err != nil {
			logging.GetLogger().Error("indexEvidenceFile INDEX_PARTITION_ERROR", zap.Error(err), zap.Int("partition_index", index))
			return err
		}
	}

	err := persistEvidenceInternalObjects(eviFile.GetID(), eviFile.GetInternalObjects(), db)
	if err != nil {
		logging.GetLogger().Error("indexEvidenceFile PERSIST_INTERNAL_OBJECTS_ERROR", zap.Error(err))
		return err
	}
	logging.GetLogger().Info("indexEvidenceFile COMPLETE", zap.Int("partition_count", len(partitions)))
	return nil
}

func persistEvidenceInternalObjects(fileID []byte, internalObjects map[string]structs.InternalOffset, db *badger.DB) error {
	evidenceFile, err := dbio.GetEvidenceFile(fileID, db)
	if err != nil {
		return err
	}

	evidenceFile.InternalObjects = make(map[string]structs.InternalOffset, len(internalObjects))
	for key, offset := range internalObjects {
		evidenceFile.InternalObjects[key] = offset
	}

	return dbio.SetFile(fileID, evidenceFile, db)
}

func upsertPartitionNodeForUnparsedPartition(repo *enrichment.GrapheneRepository, eviFile structs.InputFile, pfile structs.InputFile) error {
	if repo == nil {
		return nil
	}

	evidenceHashB64, err := eviFile.GetEncodedHash()
	if err != nil {
		return err
	}
	partitionHashB64, err := pfile.GetEncodedHash()
	if err != nil {
		return err
	}

	return repo.UpsertPartition(enrichment.PartitionRecord{
		EvidenceFileID:   string(evidenceHashB64),
		EvidenceFileName: eviFile.GetName(),
		PartitionID:      string(partitionHashB64),
		PartitionName:    pfile.GetName(),
	})
}

func initEvidenceFile(evifilepath string, db *badger.DB) (structs.InputFile, error) {
	var eviFile structs.InputFile

	eviInfo, err := os.Stat(evifilepath)
	if err != nil {
		return eviFile, err
	}
	eviSize := eviInfo.Size()
	eviHandle, err := os.Open(evifilepath)
	if err != nil {
		return eviFile, err
	}
	eviFileName := filepath.Base(evifilepath)

	eviFileHash, err := util.GetFileHash(eviHandle, cnst.GetHashAlgo(true))
	if err != nil {
		return eviFile, err
	}

	mappedFile, err := mmap.Map(eviHandle, mmap.RDONLY, 0)
	if err != nil {
		return eviFile, err
	}

	eviFile = structs.NewInputFile(
		db,
		eviHandle,
		mappedFile,
		eviFileName,
		cnst.EviFileNamespace,
		eviFileHash,
		eviSize,
		0,
	)

	return eviFile, nil
}
