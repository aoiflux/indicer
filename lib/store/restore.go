package store

import (
	"bufio"
	"bytes"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
	"github.com/schollz/progressbar/v3"
	"github.com/vmihailenco/msgpack/v5"
)

type restoreChunkJob struct {
	seq          int64
	restoreIndex int64
	chunkKey     []byte
	metadata     []byte
}

type restoreChunkResult struct {
	seq  int64
	data []byte
	err  error
}

type restoreProducerResult struct {
	total int64
	err   error
}

func Restore(fhash string, dst *os.File, db *badger.DB) error {
	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}
	meta, err := GetFileMeta(fid, db)
	if err != nil {
		return err
	}
	return restoreData(meta, dst, db)
}

func GetFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		return getIndexedFileMeta(fid, db)
	}
	if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
		return getPartitionFileMeta(fid, db)
	}
	return getEvidenceFileMeta(fid, db)
}

func checkCompleted(ehash []byte, db *badger.DB) error {
	eid := util.GetEvidenceFileID(ehash)
	eviFile, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	if !eviFile.Completed {
		return cnst.ErrIncompleteFile
	}
	return nil
}

func getIndexedFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	var meta structs.FileMeta

	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return meta, err
	}
	ehash, err := GetLogicalFileEviHash(ifile.Names, db)
	if err != nil {
		return meta, err
	}

	meta.EviHash = ehash
	meta.Start = ifile.Start
	meta.Size = ifile.Size
	return meta, nil
}
func getPartitionFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	var meta structs.FileMeta

	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return meta, err
	}
	ehash, err := GetLogicalFileEviHash(pfile.Names, db)
	if err != nil {
		return meta, err
	}

	meta.EviHash = ehash
	meta.Start = pfile.Start
	meta.Size = pfile.Size
	return meta, nil
}
func GetLogicalFileEviHash(names map[string]struct{}, db *badger.DB) ([]byte, error) {
	name := util.GetArbitratyMapKey(names)
	ehash, err := util.GetEvidenceFileHash(name)
	if err != nil {
		return nil, err
	}
	err = checkCompleted(ehash, db)
	if err != nil {
		return nil, err
	}
	return ehash, nil
}

func getEvidenceFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	var meta structs.FileMeta

	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return meta, err
	}
	if !efile.Completed {
		return meta, cnst.ErrIncompleteFile
	}
	ehash := bytes.Split(fid, []byte(cnst.EviFileNamespace))[1]

	meta.EviHash = ehash
	meta.Start = efile.Start
	meta.Size = efile.Size
	return meta, nil
}

func restoreData(meta structs.FileMeta, dst *os.File, db *badger.DB) error {
	configureRestoreCache()

	fio.EnableContainerReadCache()
	defer fio.DisableContainerReadCache()

	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	if err := dst.Truncate(meta.Size); err != nil {
		return err
	}
	if _, err := dst.Seek(0, 0); err != nil {
		return err
	}

	writer := bufio.NewWriterSize(dst, cnst.GetRestoreWriteBufferSize())

	relPrefix := make([]byte, 0, len(cnst.RelationNamespace)+len(meta.EviHash)+len(cnst.DataSeperator))
	relPrefix = append(relPrefix, cnst.RelationNamespace...)
	relPrefix = append(relPrefix, meta.EviHash...)
	relPrefix = append(relPrefix, cnst.DataSeperator...)

	bar := progressbar.NewOptions64(
		meta.Size,
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)

	workerCount, jobQueueDepth := cnst.GetRestorePipelineTuning()
	jobs := make(chan restoreChunkJob, jobQueueDepth)
	results := make(chan restoreChunkResult, jobQueueDepth)
	producerDone := make(chan restoreProducerResult, 1)

	done := make(chan struct{})
	var doneOnce sync.Once
	stop := func() {
		doneOnce.Do(func() {
			close(done)
		})
	}

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-done:
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}

					data, err := getRestoredChunkData(meta, job.restoreIndex, dbstart, end, db, job.chunkKey, job.metadata)
					result := restoreChunkResult{seq: job.seq, data: data, err: err}

					select {
					case results <- result:
					case <-done:
						return
					}

					if err != nil {
						return
					}
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		const fetchBatchSize = 128
		var totalSeq int64

		err := db.View(func(txn *badger.Txn) error {
			for batchStart := dbstart; batchStart < end; {
				restoreIndexes := make([]int64, 0, fetchBatchSize)
				relationKeys := make([][]byte, 0, fetchBatchSize)

				for i := 0; i < fetchBatchSize && batchStart < end; i++ {
					restoreIndex := batchStart
					batchStart += cnst.ChonkSize

					restoreIndexes = append(restoreIndexes, restoreIndex)
					relationKey := make([]byte, len(relPrefix), len(relPrefix)+20)
					copy(relationKey, relPrefix)
					relationKey = strconv.AppendInt(relationKey, restoreIndex, 10)
					relationKeys = append(relationKeys, relationKey)
				}

				chashes, err := getTxnNodesBatch(relationKeys, txn)
				if err != nil {
					return err
				}

				chunkKeys := make([][]byte, 0, len(chashes))
				for _, chash := range chashes {
					chunkKey := make([]byte, 0, len(cnst.ChonkNamespace)+len(chash))
					chunkKey = append(chunkKey, cnst.ChonkNamespace...)
					chunkKey = append(chunkKey, chash...)
					chunkKeys = append(chunkKeys, chunkKey)
				}

				metadataBatch, err := getTxnNodesBatchAllowMissing(chunkKeys, txn)
				if err != nil {
					return err
				}

				batchSeqStart := totalSeq
				totalSeq += int64(len(restoreIndexes))

				emissionOrder := buildContainerLocalityOrder(metadataBatch)
				if len(emissionOrder) == 0 {
					emissionOrder = make([]int, len(restoreIndexes))
					for i := range restoreIndexes {
						emissionOrder[i] = i
					}
				}

				for _, i := range emissionOrder {
					restoreIndex := restoreIndexes[i]
					job := restoreChunkJob{seq: batchSeqStart + int64(i), restoreIndex: restoreIndex, chunkKey: chunkKeys[i], metadata: metadataBatch[i]}
					select {
					case jobs <- job:
					case <-done:
						return nil
					}
				}
			}
			return nil
		})

		producerDone <- restoreProducerResult{total: totalSeq, err: err}
	}()

	go func() {
		workers.Wait()
		close(results)
	}()

	progressInterval := cnst.GetRestoreProgressInterval()
	lastProgress := time.Now()
	var pendingProgress int64
	nextSeq := int64(0)
	pending := make(map[int64][]byte)
	var workerErr error

	for result := range results {
		if result.err != nil {
			if workerErr == nil {
				workerErr = result.err
				stop()
			}
			continue
		}

		if result.seq == nextSeq {
			written, err := writeRestoredChunkData(writer, result.data)
			if err != nil {
				workerErr = err
				stop()
				continue
			}
			pendingProgress += written
			nextSeq++

			for {
				data, ok := pending[nextSeq]
				if !ok {
					break
				}
				delete(pending, nextSeq)
				written, err = writeRestoredChunkData(writer, data)
				if err != nil {
					workerErr = err
					stop()
					break
				}
				pendingProgress += written
				nextSeq++
			}
		} else if result.seq > nextSeq {
			pending[result.seq] = result.data
		}

		if pendingProgress >= 4*cnst.MB || time.Since(lastProgress) >= progressInterval {
			bar.Add64(pendingProgress)
			pendingProgress = 0
			lastProgress = time.Now()
		}
	}

	producerResult := <-producerDone
	if workerErr != nil {
		return workerErr
	}
	if producerResult.err != nil {
		return producerResult.err
	}
	if nextSeq != producerResult.total {
		return fmt.Errorf("restore pipeline incomplete: wrote %d/%d chunks", nextSeq, producerResult.total)
	}

	if pendingProgress > 0 {
		bar.Add64(pendingProgress)
	}
	if err := writer.Flush(); err != nil {
		return err
	}

	bar.Finish()
	fmt.Fprintln(os.Stderr)
	fmt.Println("Restored file with size: ", humanize.Bytes(uint64(meta.Size)))
	return bar.Close()
}

func configureRestoreCache() {
	cacheSize, err := cnst.GetCacheLimit()
	if err != nil {
		cacheSize = 256 * cnst.MB
	}

	maxCache := int64(4 * cnst.GB)
	if cacheSize > maxCache {
		cacheSize = maxCache
	}
	fio.SetContainerReadCacheSize(cacheSize)
}

func getRestoredChunkData(meta structs.FileMeta, restoreIndex, dbstart, end int64, db *badger.DB, chunkKey []byte, metadata []byte) ([]byte, error) {
	if len(metadata) == 0 {
		data, err := dbio.GetChonkData(restoreIndex, meta.Start, meta.Size, dbstart, end, chunkKey, db)
		if err != nil {
			return nil, err
		}
		return data, nil
	}

	data, err := dbio.GetChonkDataWithMetadata(restoreIndex, meta.Start, meta.Size, dbstart, end, metadata, db)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func writeRestoredChunkData(dst *bufio.Writer, data []byte) (int64, error) {
	if _, err := dst.Write(data); err != nil {
		return 0, err
	}

	return int64(len(data)), nil
}

func getTxnNodesBatch(keys [][]byte, txn *badger.Txn) ([][]byte, error) {
	values := make([][]byte, 0, len(keys))

	for _, key := range keys {
		item, err := txn.Get(key)
		if err != nil {
			return nil, err
		}

		data, err := item.ValueCopy(nil)
		if err != nil {
			return nil, err
		}

		decoded, err := cnst.DECODER.DecodeAll(data, nil)
		if err == nil {
			values = append(values, decoded)
			continue
		}

		values = append(values, data)
	}

	return values, nil
}

func getTxnNodesBatchAllowMissing(keys [][]byte, txn *badger.Txn) ([][]byte, error) {
	values := make([][]byte, len(keys))

	for i, key := range keys {
		item, err := txn.Get(key)
		if err != nil {
			if err == badger.ErrKeyNotFound {
				continue
			}
			return nil, err
		}

		data, err := item.ValueCopy(nil)
		if err != nil {
			return nil, err
		}

		decoded, err := cnst.DECODER.DecodeAll(data, nil)
		if err == nil {
			values[i] = decoded
			continue
		}

		values[i] = data
	}

	return values, nil
}

// buildContainerLocalityOrder returns an index emission order that clusters
// container-backed chunks together per batch to improve cache and read locality.
// It preserves deterministic output by keeping original logical seq IDs and
// only changing worker dispatch order.
func buildContainerLocalityOrder(metadataBatch [][]byte) []int {
	if len(metadataBatch) < 2 {
		return nil
	}

	containerGroups := make(map[string][]int)
	groupOrder := make([]string, 0, 8)
	nonContainer := make([]int, 0, len(metadataBatch))

	for i, metadata := range metadataBatch {
		containerPath, ok := extractContainerPath(metadata)
		if !ok {
			nonContainer = append(nonContainer, i)
			continue
		}

		if _, exists := containerGroups[containerPath]; !exists {
			groupOrder = append(groupOrder, containerPath)
		}
		containerGroups[containerPath] = append(containerGroups[containerPath], i)
	}

	if len(groupOrder) <= 1 {
		return nil
	}

	order := make([]int, 0, len(metadataBatch))
	for _, containerPath := range groupOrder {
		order = append(order, containerGroups[containerPath]...)
	}
	order = append(order, nonContainer...)

	return order
}

func extractContainerPath(metadata []byte) (string, bool) {
	if len(metadata) == 0 {
		return "", false
	}

	var parsed structs.ChonkMetadata
	if err := msgpack.Unmarshal(metadata, &parsed); err != nil {
		return "", false
	}
	if !parsed.Container || parsed.Path == "" {
		return "", false
	}

	return parsed.Path, true
}
