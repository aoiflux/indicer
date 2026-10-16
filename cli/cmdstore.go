package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	"indicer/lib/enrichment"
	"indicer/lib/logging"
	"indicer/lib/parser"
	"indicer/lib/parser/tskcompat"
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

type asyncFileHashResult struct {
	encodedHash string
	err         error
}

type evidenceHashOutcome struct {
	encodedHash string
	resultCh    <-chan asyncFileHashResult
}

func startAsyncFileHash(filePath string) <-chan asyncFileHashResult {
	resultCh := make(chan asyncFileHashResult, 1)
	go func() {
		handle, err := os.Open(filePath)
		if err != nil {
			resultCh <- asyncFileHashResult{err: err}
			return
		}
		defer handle.Close()

		hash, err := util.GetFileHash(handle, cnst.GetHashAlgo(true))
		if err != nil {
			resultCh <- asyncFileHashResult{err: err}
			return
		}

		resultCh <- asyncFileHashResult{encodedHash: base64.StdEncoding.EncodeToString(hash)}
	}()
	return resultCh
}

func awaitAsyncFileHash(hashResultCh <-chan asyncFileHashResult) (string, error) {
	if hashResultCh == nil {
		return "", nil
	}
	result := <-hashResultCh
	return result.encodedHash, result.err
}

func computeSyncFileHash(filePath string) (string, error) {
	handle, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer handle.Close()

	hash, err := util.GetFileHash(handle, cnst.GetHashAlgo(true))
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(hash), nil
}

func prepareEvidenceFileHash(filePath string) (evidenceHashOutcome, error) {
	switch cnst.StoreHashStrategy {
	case cnst.AsyncHashStrategy:
		return evidenceHashOutcome{resultCh: startAsyncFileHash(filePath)}, nil
	case cnst.HochoHashStrategy:
		// Hocho evidence hash is produced by ingest from chunk hashes.
		return evidenceHashOutcome{}, nil
	case cnst.SyncHashStrategy:
		encodedHash, err := computeSyncFileHash(filePath)
		if err != nil {
			return evidenceHashOutcome{}, err
		}
		return evidenceHashOutcome{encodedHash: encodedHash}, nil
	default:
		return evidenceHashOutcome{}, fmt.Errorf("unsupported store hash strategy: %s", cnst.StoreHashStrategy)
	}
}

func StoreData(ctx context.Context, opts DBOptions, evipath string, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	logging.GetLogger().Info("StoreData START",
		zap.String("db_path", opts.Path),
		zap.String("evidence_path", evipath),
		zap.Bool("sync_index", syncIndex),
		zap.Bool("no_index", noIndex),
		zap.Bool("enable_fts", enableFTS),
		zap.Bool("enable_enrichment", enableEnrichment),
	)

	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		logging.GetLogger().Error("StoreData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)
	db, dbpath := h.DB, h.Root
	chonkSize := h.Manifest.Layout.ChunkKiB
	var key []byte // the store path takes its keys from the database
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
		if err := ctx.Err(); err != nil {
			return err
		}
		logging.GetLogger().Info("StoreData STORE_FOLDER_EXECUTE", zap.String("evidence_path", evipath))
		console.Println("Storing Entire Folder")
		err = StoreFolderWithContext(ctx, chonkSize, evipath, key, syncIndex, noIndex, enableFTS, enableEnrichment, db)
		// Files that did store are kept, so the value log is compacted
		// whether or not some of the folder failed.
		compactValueLog(h, "StoreData")
		if err != nil {
			logging.GetLogger().Error("StoreData STORE_FOLDER_ERROR", zap.Error(err), zap.String("evidence_path", evipath))
			return err
		}
		logging.GetLogger().Info("StoreData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	logging.GetLogger().Info("StoreData STORE_FILE_EXECUTE", zap.String("evidence_path", evipath))
	err = StoreFileWithContext(ctx, chonkSize, evipath, key, syncIndex, noIndex, enableFTS, enableEnrichment, db)
	if err != nil {
		logging.GetLogger().Error("StoreData STORE_FILE_ERROR", zap.Error(err), zap.String("evidence_path", evipath))
		return err
	}

	compactValueLog(h, "StoreData")
	logging.GetLogger().Info("StoreData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

func StoreFolder(chonkSize int, evidir string, key []byte, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) error {
	return StoreFolderWithContext(context.Background(), chonkSize, evidir, key, syncIndex, noIndex, enableFTS, enableEnrichment, db)
}

func StoreFolderWithContext(ctx context.Context, chonkSize int, evidir string, key []byte, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) error {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	logging.GetLogger().Info("StoreFolder START",
		zap.Int("chonk_size", chonkSize),
		zap.String("evidence_dir", evidir),
		zap.Bool("sync_index", syncIndex),
		zap.Bool("no_index", noIndex),
		zap.Bool("enable_fts", enableFTS),
		zap.Bool("enable_enrichment", enableEnrichment),
	)

	// One file that fails, or a directory that cannot be read, no longer
	// stops the folder (ING-11): every file is attempted and the failures
	// are reported together. Only regular files are stored; a symlink,
	// FIFO or device is skipped, as opening a FIFO blocks.
	var stored, skipped int
	var failures []string
	var firstErr error
	fail := func(path string, err error) {
		failures = append(failures, fmt.Sprintf("%s: %v", path, err))
		if firstErr == nil {
			firstErr = err
		}
	}
	err := filepath.WalkDir(evidir, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			logging.GetLogger().Error("StoreFolder WALK_ERROR", zap.Error(err), zap.String("file_path", path))
			fail(path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			logging.GetLogger().Warn("StoreFolder SKIP_NOT_REGULAR", zap.String("file_path", path), zap.String("type", d.Type().String()))
			console.Printf("Skipping %s: not a regular file\n", path)
			skipped++
			return nil
		}
		if err := storeOneFile(ctx, chonkSize, path, key, syncIndex, noIndex, enableFTS, enableEnrichment, db); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			logging.GetLogger().Error("StoreFolder STORE_FILE_ERROR", zap.Error(err), zap.String("file_path", path))
			fail(path, err)
			return nil
		}
		stored++
		return nil
	})
	if err != nil {
		logging.GetLogger().Error("StoreFolder STORE_ERROR", zap.Error(err), zap.String("evidence_dir", evidir))
		return err
	}

	console.Println()
	console.Printf("Folder store: %d stored, %d failed, %d skipped (not regular files) in %v\n", stored, len(failures), skipped, time.Since(start))
	for _, f := range failures {
		console.Println("  failed:", f)
	}
	logging.GetLogger().Info("StoreFolder COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Int("stored", stored), zap.Int("failed", len(failures)), zap.Int("skipped", skipped))
	if len(failures) > 0 {
		return fmt.Errorf("%d file(s) in %s failed to store, first %s: %w", len(failures), evidir, failures[0], firstErr)
	}
	return nil
}

// storeOneFile stores one file of a folder; tests replace it.
var storeOneFile = StoreFileWithContext

func StoreFile(chonkSize int, evipath string, key []byte, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) error {
	return StoreFileWithContext(context.Background(), chonkSize, evipath, key, syncIndex, noIndex, enableFTS, enableEnrichment, db)
}

func StoreFileWithContext(ctx context.Context, chonkSize int, evipath string, key []byte, syncIndex bool, noIndex bool, enableFTS bool, enableEnrichment bool, db *badger.DB) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
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
	if err := ctx.Err(); err != nil {
		return err
	}

	console.Println("Pre-store checks....")
	logging.GetLogger().Debug("StoreFile INIT_EVIDENCE_FILE", zap.String("file_path", evipath))
	eviFile, err := initEvidenceFile(evipath, db)
	if err != nil {
		logging.GetLogger().Error("StoreFile INIT_EVIDENCE_FILE_ERROR", zap.Error(err), zap.String("file_path", evipath))
		return err
	}
	// Every return below has waited for the indexer, so the mapping can go.
	defer func() {
		if rerr := releaseEvidenceFile(eviFile); err == nil {
			err = rerr
		}
	}()
	if eviFile.GetSize() == 0 {
		// An empty file holds no filesystem to index.
		noIndex = true
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

	hashOutcome, err := prepareEvidenceFileHash(evipath)
	if err != nil {
		logging.GetLogger().Error("StoreFile HASH_PREP_ERROR", zap.Error(err), zap.String("strategy", cnst.StoreHashStrategy))
		return err
	}

	err = store.EnsureEvidenceFilePreStoreState(eviFile)
	if err != nil {
		logging.GetLogger().Error("StoreFile ENSURE_EVIDENCE_PRESTORE_STATE_ERROR", zap.Error(err))
		return err
	}

	deferIndexUntilFlushed := cnst.StoreHashStrategy == cnst.HochoHashStrategy && cnst.HochoMode == cnst.HochoModePostDedup
	var idxErrCh chan error
	if !deferIndexUntilFlushed {
		idxErrCh, err = startIndexing(eviFile, db, noIndex, syncIndex, enableFTS, enableEnrichment)
		if err != nil {
			logging.GetLogger().Error("StoreFile INDEX_ERROR", zap.Error(err))
			return err
		}
	} else {
		logging.GetLogger().Info("StoreFile INDEX_DEFERRED_UNTIL_FLUSHED", zap.String("hocho_mode", cnst.HochoMode))
	}

	eviname := filepath.Base(evipath)
	console.Printf("\nSaving Evidence File: %s\n", eviname)

	// Wait for the store even when ctx is cancelled: returning early left it
	// writing while the caller closed the database (ING-5).
	err = store.StoreInputFile(ctx, eviFile)
	if ctxErr := ctx.Err(); ctxErr != nil {
		drainIndexError(idxErrCh)
		return ctxErr
	}
	if err != nil {
		drainIndexError(idxErrCh)
		markEvidenceFileFailed(eviFile.GetID(), db)
		logging.GetLogger().Error("StoreFile STORE_ERROR", zap.Error(err))
		return err
	}

	err = transitionEvidenceFileToCompleted(eviFile, eviname, db, idxErrCh, hashOutcome)
	if err != nil {
		return err
	}

	if deferIndexUntilFlushed {
		idxErrCh, err = startIndexing(eviFile, db, noIndex, syncIndex, enableFTS, enableEnrichment)
		if err != nil {
			logging.GetLogger().Error("StoreFile INDEX_ERROR", zap.Error(err))
			markEvidenceFileFailed(eviFile.GetID(), db)
			return err
		}
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

	console.Printf("\nStored in: %v\n\n", time.Since(start))
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

func transitionEvidenceFileToCompleted(eviFile structs.InputFile, eviname string, db *badger.DB, idxErrCh chan error, hashOutcome evidenceHashOutcome) error {
	encodedHash := hashOutcome.encodedHash
	if encodedHash == "" && cnst.StoreHashStrategy != cnst.HochoHashStrategy {
		var err error
		encodedHash, err = awaitAsyncFileHash(hashOutcome.resultCh)
		if err != nil {
			drainIndexError(idxErrCh)
			markEvidenceFileFailed(eviFile.GetID(), db)
			logging.GetLogger().Error("StoreFile HASH_AWAIT_ERROR", zap.Error(err))
			return err
		}
	}

	// One read-modify-write of the current record (ING-8): the indexer may
	// have written the partition map while the hash was computed, and a copy
	// read before the wait would erase it.
	err := dbio.UpdateEvidence(db, eviFile.GetID(), func(txn *badger.Txn, eviNode *structs.EvidenceFile) error {
		// Every chunk must already be on disk.
		if eviNode.IngestState != structs.IngestStateFlushed {
			return fmt.Errorf("ingest state transition violation: expected %s, got %s", structs.IngestStateFlushed, eviNode.IngestState)
		}
		hash := encodedHash
		if hash == "" && cnst.StoreHashStrategy == cnst.HochoHashStrategy {
			hash = eviNode.FileHash
			if hash == "" {
				return fmt.Errorf("ingest-reused hocho hash missing on flushed evidence record")
			}
		}
		eviNode.FileHash = hash
		eviNode.Completed = true
		eviNode.Failed = false
		eviNode.IngestState = structs.IngestStateCompleted
		if hash == "" {
			return nil
		}
		lookupKey := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, hash)
		return dbio.AppendHashLookupUUIDByKeyTxn(txn, lookupKey, dbio.CanonicalEvidenceKey(eviFile.GetID()))
	})
	if err != nil {
		drainIndexError(idxErrCh)
		markEvidenceFileFailed(eviFile.GetID(), db)
		logging.GetLogger().Error("StoreFile COMPLETE_EVIDENCE_ERROR", zap.Error(err))
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

// releaseEvidenceFile unmaps and closes an evidence file; tests replace it.
var releaseEvidenceFile = closeInputFileResources

func closeInputFileResources(eviFile structs.InputFile) error {
	if mappedFile := eviFile.GetMappedFile(); len(mappedFile) > 0 {
		if err := mappedFile.Unmap(); err != nil {
			logging.GetLogger().Error("StoreFile UNMAP_ERROR", zap.Error(err))
			return err
		}
	}

	err := eviFile.GetHandle().Close()
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
	statsBefore := store.LogicalHochoReuseStats{}
	if cnst.StoreHashStrategy == cnst.HochoHashStrategy {
		statsBefore = store.SnapshotLogicalHochoReuseStats()
	}
	// Evidence parser: the pure-Go lib/parser stack via tskcompat, which emits the
	// libtusk JSON shape so ParseImage/IndexFilesystem downstream are unchanged.
	// On a parse failure hasTusk stays false and ParseImage falls back to its own
	// MBR/exFAT detection.
	var tuskJSON string
	var hasTusk bool
	if out, err := tskcompat.Analyze(eviFile.GetHandle().Name()); err == nil {
		tuskJSON, hasTusk = out, true
	} else {
		logging.GetLogger().Error("indexEvidenceFile PARSER_ERROR", zap.Error(err))
	}
	partitions := parser.ParseImage(tuskJSON, hasTusk, eviFile.GetSize(), eviFile.GetHandle())

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
		idxChan := make(chan error)
		var phash []byte
		if cnst.StoreHashStrategy == cnst.HochoHashStrategy && cnst.HochoMode != cnst.HochoModeBaseline {
			reusedHash, reused, reuseErr := store.TryComputeLogicalHochoWithEdges(eviFile.GetID(), partition.Start, partition.Size, eviFile.GetMappedFile(), db)
			if reuseErr != nil {
				logging.GetLogger().Error("indexEvidenceFile PARTITION_HASH_REUSE_ERROR", zap.Error(reuseErr), zap.Int("partition_index", index))
				return reuseErr
			}
			if reused {
				phash = reusedHash
			}
		}
		if len(phash) == 0 {
			var err error
			phash, err = util.GetLogicalFileHash(eviFile.GetHandle(), cnst.GetHashAlgo(true), partition.Start, partition.Size, true)
			if err != nil {
				logging.GetLogger().Error("indexEvidenceFile PARTITION_HASH_ERROR", zap.Error(err), zap.Int("partition_index", index))
				return err
			}
		}

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
		eviFile.UpdateInternalObjects(partition.Start, partition.Size, pfile.GetID())

		if enableEnrichment {
			err := upsertPartitionNodeForUnparsedPartition(enrichRepo, eviFile, pfile)
			if err != nil {
				logging.GetLogger().Error("indexEvidenceFile UPSERT_PARTITION_ERROR", zap.Error(err), zap.Int("partition_index", index))
				return err
			}
		}

		if hasTusk {
			go parser.IndexFilesystem(tuskJSON, pfile, eviFile.GetID(), idxChan, enableFTS, enableEnrichment)
		} else {
			go parser.IndexEXFAT(pfile, eviFile.GetID(), idxChan, enableFTS, enableEnrichment)
		}
		select {
		case err = <-idxChan:
		case <-time.After(5 * time.Minute):
			err = fmt.Errorf("partition indexing timed out (partition=%d name=%s)", index+1, pfile.GetName())
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
	if cnst.StoreHashStrategy == cnst.HochoHashStrategy {
		statsAfter := store.SnapshotLogicalHochoReuseStats()
		logging.GetLogger().Info("indexEvidenceFile LOGICAL_HOCHO_REUSE_STATS",
			zap.Uint64("attempts", statsAfter.Attempts-statsBefore.Attempts),
			zap.Uint64("reused", statsAfter.Reused-statsBefore.Reused),
			zap.Uint64("fallback_unaligned", statsAfter.FallbackUnaligned-statsBefore.FallbackUnaligned),
			zap.Uint64("fallback_missing_relation", statsAfter.FallbackMissingRelation-statsBefore.FallbackMissingRelation),
			zap.Uint64("errors", statsAfter.Errors-statsBefore.Errors),
		)
	}
	logging.GetLogger().Info("indexEvidenceFile COMPLETE", zap.Int("partition_count", len(partitions)))
	return nil
}

func persistEvidenceInternalObjects(fileID []byte, internalObjects map[string]structs.InternalOffset, db *badger.DB) error {
	return dbio.UpdateEvidence(db, fileID, func(_ *badger.Txn, evidenceFile *structs.EvidenceFile) error {
		evidenceFile.InternalObjects = make(map[string]structs.InternalOffset, len(internalObjects))
		for key, offset := range internalObjects {
			evidenceFile.InternalObjects[key] = offset
		}
		return nil
	})
}

func upsertPartitionNodeForUnparsedPartition(repo *enrichment.GrapheneRepository, eviFile structs.InputFile, pfile structs.InputFile) error {
	if repo == nil {
		return nil
	}

	evidenceHashB64, err := eviFile.GetEncodedHash()
	if err != nil {
		return err
	}
	partitionHashB64 := base64.StdEncoding.EncodeToString(pfile.GetFileHash())

	return repo.UpsertPartition(enrichment.PartitionRecord{
		EvidenceFileID:   string(evidenceHashB64),
		EvidenceFileName: eviFile.GetName(),
		PartitionID:      partitionHashB64,
		PartitionName:    pfile.GetName(),
	})
}

func initEvidenceFile(evifilepath string, db *badger.DB) (structs.InputFile, error) {
	var eviFile structs.InputFile

	eviInfo, err := os.Stat(evifilepath)
	if err != nil {
		return eviFile, err
	}
	if !eviInfo.Mode().IsRegular() {
		// Opening a FIFO blocks, and a device has no fixed content to store.
		return eviFile, fmt.Errorf("%s is not a regular file (%s)", evifilepath, eviInfo.Mode().Type())
	}
	eviSize := eviInfo.Size()
	eviHandle, err := os.Open(evifilepath)
	if err != nil {
		return eviFile, err
	}
	eviFileName := filepath.Base(evifilepath)

	// An empty file cannot be mapped, and needs no mapping (ING-11).
	var mappedFile mmap.MMap
	if eviSize > 0 {
		mappedFile, err = mmap.Map(eviHandle, mmap.RDONLY, 0)
		if err != nil {
			eviHandle.Close()
			return eviFile, err
		}
	}

	eviFile = structs.NewInputFile(
		db,
		eviHandle,
		mappedFile,
		eviFileName,
		cnst.EviFileNamespace,
		nil,
		eviSize,
		0,
	)

	return eviFile, nil
}
