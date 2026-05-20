package service

import (
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/logging"
	"indicer/lib/structs"
	"indicer/lib/util"
	"time"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

func GetFileChunkMap(fileStart, fileSize int64, fileHash []byte) (map[string]int64, error) {
	var meta structs.FileMeta
	meta.EviHash = fileHash
	meta.Size = fileSize
	meta.Start = fileStart
	return getChonkMap(meta, cnst.DB)
}

func getEvidenceFile(filePath, fileHashStr string, db *badger.DB) (structs.EvidenceFile, error) {
	var efile structs.EvidenceFile
	fileHash, err := base64.StdEncoding.DecodeString(fileHashStr)
	if err != nil {
		logging.GetLogger().Error("Failed to decode file hash", zap.String("hash", fileHashStr), zap.Error(err))
		return efile, err
	}
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)

	efile, err = dbio.GetEvidenceFile(eid, db)
	if err != nil {
		if err == badger.ErrKeyNotFound {
			logging.GetLogger().Warn("File not found in DB", zap.String("filepath", filePath))
			return efile, cnst.ErrFileNotFound
		}
		logging.GetLogger().Error("Failed to get evidence file", zap.String("filepath", filePath), zap.Error(err))
		return efile, err
	}
	if !efile.Completed {
		logging.GetLogger().Warn("File is incomplete", zap.String("filepath", filePath))
		return efile, cnst.ErrFileNotFound
	}
	return efile, nil
}

func getChonkMap(meta structs.FileMeta, db *badger.DB) (map[string]int64, error) {
	startTime := time.Now()
	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size
	if end <= dbstart {
		return map[string]int64{}, nil
	}

	eviHashB64 := base64.StdEncoding.EncodeToString(meta.EviHash)
	logging.GetLogger().Info("getChonkMap START",
		zap.String("eviHash", eviHashB64[:16]),
		zap.Int64("fileStart", meta.Start),
		zap.Int64("fileSize", meta.Size),
		zap.Int64("dbstart", dbstart),
		zap.Int64("end", end))

	// Phase 1: Batch fetch all relation hashes
	phase1Start := time.Now()
	restoreIndices, chunkHashes, err := batchGetChunkHashes(meta.EviHash, dbstart, end, db)
	if err != nil {
		logging.GetLogger().Error("Phase1 batchGetChunkHashes failed", zap.Error(err))
		return nil, err
	}
	phase1Duration := time.Since(phase1Start)
	logging.GetLogger().Info("Phase1 batchGetChunkHashes completed",
		zap.Int64("duration_ms", phase1Duration.Milliseconds()),
		zap.Int("chunks_fetched", len(chunkHashes)))

	// Phase 2: Batch fetch all chunk metadata
	phase2Start := time.Now()
	chunkKeys := make([][]byte, len(chunkHashes))
	for i, chash := range chunkHashes {
		chunkKeys[i] = util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	}

	chunkMetadata, chunkMetadataErrs := dbio.GetNodesBatch(chunkKeys, db)
	phase2Duration := time.Since(phase2Start)
	logging.GetLogger().Info("Phase2 GetNodesBatch completed",
		zap.Int64("duration_ms", phase2Duration.Milliseconds()),
		zap.Int("chunks_count", len(chunkKeys)))

	// Phase 3: Precompute stored chunk sizes from metadata
	phase3Start := time.Now()
	hasStoredSize, storedSizes := precomputeStoredChunkSizes(chunkMetadata, chunkMetadataErrs)
	phase3Duration := time.Since(phase3Start)

	storedSizeCount := 0
	for _, has := range hasStoredSize {
		if has {
			storedSizeCount++
		}
	}
	logging.GetLogger().Info("Phase3 precomputeStoredChunkSizes completed",
		zap.Int64("duration_ms", phase3Duration.Milliseconds()),
		zap.Int("chunks_with_stored_sizes", storedSizeCount),
		zap.Int("total_chunks", len(hasStoredSize)))

	// Phase 4: Parallel chunk-size computation
	phase4Start := time.Now()
	results := make([]chunkSizeResult, len(restoreIndices))
	chunkErrors := make([]error, len(restoreIndices))

	workers := cnst.GetMaxThreadCount()
	if workers < 1 {
		workers = 1
	}
	if workers > len(restoreIndices) {
		workers = len(restoreIndices)
	}

	jobs := make(chan int, workers)
	done := make(chan struct{}, workers)
	workerCtx := chunkMapWorkerContext{
		restoreIndices:    restoreIndices,
		chunkHashes:       chunkHashes,
		chunkKeys:         chunkKeys,
		chunkMetadata:     chunkMetadata,
		chunkMetadataErrs: chunkMetadataErrs,
		hasStoredSize:     hasStoredSize,
		storedSizes:       storedSizes,
		meta:              meta,
		dbstart:           dbstart,
		end:               end,
		db:                db,
		results:           results,
		chunkErrors:       chunkErrors,
	}

	for w := 0; w < workers; w++ {
		go workerCtx.run(jobs, done)
	}

	for i := range restoreIndices {
		jobs <- i
	}
	close(jobs)
	for i := 0; i < workers; i++ {
		<-done
	}

	phase4Duration := time.Since(phase4Start)
	logging.GetLogger().Info("Phase4 parallel worker processing completed",
		zap.Int64("duration_ms", phase4Duration.Milliseconds()),
		zap.Int("workers", workers))

	for _, e := range chunkErrors {
		if e != nil {
			logging.GetLogger().Error("Phase4 worker error", zap.Error(e))
			return nil, e
		}
	}

	numChunks := int((end - dbstart + cnst.ChonkSize - 1) / cnst.ChonkSize)
	chunkMap := make(map[string]int64, numChunks)
	for _, res := range results {
		chunkMap[res.hash] = res.size
	}

	totalDuration := time.Since(startTime)
	logging.GetLogger().Info("getChonkMap COMPLETE",
		zap.Int64("total_duration_ms", totalDuration.Milliseconds()),
		zap.Int64("phase1_ms", phase1Duration.Milliseconds()),
		zap.Int64("phase2_ms", phase2Duration.Milliseconds()),
		zap.Int64("phase3_ms", phase3Duration.Milliseconds()),
		zap.Int64("phase4_ms", phase4Duration.Milliseconds()),
		zap.Int("map_entries", len(chunkMap)))

	return chunkMap, nil
}

func precomputeStoredChunkSizes(chunkMetadata [][]byte, chunkMetadataErrs []error) ([]bool, []int64) {
	hasStoredSize := make([]bool, len(chunkMetadata))
	storedSizes := make([]int64, len(chunkMetadata))

	decodedCount := 0
	errCount := 0

	for idx := range chunkMetadata {
		if chunkMetadataErrs[idx] != nil {
			errCount++
			continue
		}

		if size, ok := dbio.GetStoredChonkSealedSize(chunkMetadata[idx]); ok {
			hasStoredSize[idx] = true
			storedSizes[idx] = size
			decodedCount++
			continue
		}

		errCount++
		chunkMetadataErrs[idx] = fmt.Errorf("chunk metadata missing stored size")
		logging.GetLogger().Error("Chunk metadata decode failed", zap.Int("idx", idx))
	}

	logging.GetLogger().Debug("Stored size precompute summary",
		zap.Int("decoded", decodedCount),
		zap.Int("errors", errCount),
		zap.Int("total", len(chunkMetadata)))

	return hasStoredSize, storedSizes
}

func isBoundaryChunk(restoreIndex, dbstart, end int64) bool {
	return restoreIndex == dbstart || (restoreIndex+cnst.ChonkSize) > end
}

func batchGetChunkHashes(eviHash []byte, dbstart, end int64, db *badger.DB) ([]int64, [][]byte, error) {
	collector := newChunkHashCollector(eviHash, dbstart, end)
	err := db.View(collector.collect)
	if err != nil {
		return nil, nil, err
	}

	return collector.indices, collector.hashes, nil
}

type chunkSizeResult struct {
	hash string
	size int64
}

type chunkMapWorkerContext struct {
	restoreIndices    []int64
	chunkHashes       [][]byte
	chunkKeys         [][]byte
	chunkMetadata     [][]byte
	chunkMetadataErrs []error
	hasStoredSize     []bool
	storedSizes       []int64
	meta              structs.FileMeta
	dbstart           int64
	end               int64
	db                *badger.DB
	results           []chunkSizeResult
	chunkErrors       []error
}

func (c chunkMapWorkerContext) run(jobs <-chan int, done chan<- struct{}) {
	defer signalWorkerDone(done)

	for idx := range jobs {
		c.processIndex(idx)
	}
}

func (c chunkMapWorkerContext) processIndex(idx int) {
	restoreIndex := c.restoreIndices[idx]
	chash := c.chunkHashes[idx]
	chashB64 := base64.StdEncoding.EncodeToString(chash)
	isBoundary := isBoundaryChunk(restoreIndex, c.dbstart, c.end)

	var csize int64
	var err error
	switch c.chunkMetadataErrs[idx] {
	case nil:
		if !isBoundary && c.hasStoredSize[idx] {
			// Interior chunk with pre-computed size - fast path
			csize = c.storedSizes[idx]
			logging.GetLogger().Debug("Chunk fast path (pre-computed)",
				zap.Int("idx", idx),
				zap.String("hash", chashB64[:16]),
				zap.Int64("restoreIdx", restoreIndex),
				zap.Int64("csize", csize))
			break
		}

		// Boundary chunk or no stored size - need to recompute
		chunkType := "boundary"
		if isBoundary {
			if restoreIndex == c.dbstart {
				chunkType = "boundary(first)"
			} else {
				chunkType = "boundary(last)"
			}
		} else {
			chunkType = "interior(no_stored_size)"
		}
		logging.GetLogger().Debug("Chunk recompute START",
			zap.Int("idx", idx),
			zap.String("hash", chashB64[:16]),
			zap.Int64("restoreIdx", restoreIndex),
			zap.String("type", chunkType))

		startRecompute := time.Now()
		csize, err = dbio.GetChonkSizeWithMetadata(restoreIndex, c.meta.Start, c.meta.Size, c.dbstart, c.end, c.chunkMetadata[idx], c.db)
		recomputeDuration := time.Since(startRecompute)

		if err == nil {
			logging.GetLogger().Debug("Chunk recompute DONE",
				zap.Int("idx", idx),
				zap.String("hash", chashB64[:16]),
				zap.Int64("csize", csize),
				zap.Int64("duration_ms", recomputeDuration.Milliseconds()))
		}

	case badger.ErrKeyNotFound:
		// Keep backward compatibility for hierarchical/block-index stored chunks.
		logging.GetLogger().Debug("Chunk block index fallback",
			zap.Int("idx", idx),
			zap.String("hash", chashB64[:16]),
			zap.Int64("restoreIdx", restoreIndex))
		csize, err = dbio.GetChonkSize(restoreIndex, c.meta.Start, c.meta.Size, c.dbstart, c.end, c.chunkKeys[idx], c.db)

	default:
		err = c.chunkMetadataErrs[idx]
		logging.GetLogger().Error("Chunk error",
			zap.Int("idx", idx),
			zap.String("hash", chashB64[:16]),
			zap.Int64("restoreIdx", restoreIndex),
			zap.Error(err))
	}

	if err != nil {
		c.chunkErrors[idx] = err
		return
	}

	c.results[idx] = chunkSizeResult{
		hash: chashB64,
		size: csize,
	}
}

func signalWorkerDone(done chan<- struct{}) {
	done <- struct{}{}
}

type chunkHashCollector struct {
	eviHash []byte
	dbstart int64
	end     int64
	indices []int64
	hashes  [][]byte
}

func newChunkHashCollector(eviHash []byte, dbstart, end int64) chunkHashCollector {
	numChunks := int((end - dbstart + cnst.ChonkSize - 1) / cnst.ChonkSize)
	return chunkHashCollector{
		eviHash: eviHash,
		dbstart: dbstart,
		end:     end,
		indices: make([]int64, 0, numChunks),
		hashes:  make([][]byte, 0, numChunks),
	}
}

func (c *chunkHashCollector) collect(txn *badger.Txn) error {
	logging.GetLogger().Debug("Hash collection START",
		zap.String("eviHash", base64.StdEncoding.EncodeToString(c.eviHash)[:16]),
		zap.Int64("dbstart", c.dbstart),
		zap.Int64("end", c.end))

	for restoreIndex := c.dbstart; restoreIndex < c.end; restoreIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, c.eviHash, cnst.DataSeperator, restoreIndex)
		item, err := txn.Get(relKey)
		if err != nil {
			logging.GetLogger().Error("Failed to get relKey",
				zap.Int64("restoreIndex", restoreIndex),
				zap.Error(err))
			return err
		}

		raw, err := item.ValueCopy(nil)
		if err != nil {
			logging.GetLogger().Error("Failed to copy value",
				zap.Int64("restoreIndex", restoreIndex),
				zap.Error(err))
			return err
		}

		decoded, decodeErr := cnst.DECODER.DecodeAll(raw, nil)
		if decodeErr == nil {
			raw = decoded
		}

		c.indices = append(c.indices, restoreIndex)
		c.hashes = append(c.hashes, raw)
	}

	logging.GetLogger().Debug("Hash collection DONE", zap.Int("collected", len(c.hashes)))
	return nil
}
