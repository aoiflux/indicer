package store

// batch_owner.go implements the production ingest pipeline.
// Big picture:
//  1) Worker goroutines read chunk ranges, compute chunk hashes, and
//     materialize chunk metadata.
//  2) Workers send prepared chunk tasks to one writer goroutine over a channel.
//  3) The writer goroutine is the only owner of mutable ingest-write state.
//
// The writer exclusively owns:
//  - *badger.WriteBatch
//  - reverse-relation append buffer
//
// Why this is simpler and faster than the old shared-batch variant:
//  - No revRelAppendBuffer lock contention: one writer updates and flushes it.
//  - No WriteBatch multi-writer pressure: one goroutine performs all batch
//    mutations.
//
// Result:
//  - Lower lock overhead on the hot path.
//  - Easier reasoning about ownership and race safety.
//  - Deterministic write ordering for relation and reverse-relation updates.
//
// This is now the only ingest path; the shared-batch path is retired.

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

// chunkTask carries per-chunk outputs from hash/materialize workers to the
// single batch-owner goroutine. cmeta holds pre-materialized encoded chunk
// metadata for batch DB persistence; cdata is only used by benchmark/helper
// paths.
type chunkTask struct {
	index int64
	chash []byte
	cdata []byte
	cmeta []byte
}

type workerRes struct {
	err   error
	bytes int64
}

type chunkJob struct {
	idx      int64
	chonkEnd int64
}

type writerResult struct {
	hochoHash string
	err       error
}

type hochoAccumulator struct {
	enabled      bool
	hasher       hash.Hash
	expectedIdx  int64
	pendingChunk map[int64][]byte
}

func newHochoAccumulator(enabled bool, startIndex int64) *hochoAccumulator {
	if !enabled {
		return &hochoAccumulator{enabled: false}
	}
	return &hochoAccumulator{
		enabled:      true,
		hasher:       cnst.GetHashAlgo(true),
		expectedIdx:  startIndex,
		pendingChunk: make(map[int64][]byte),
	}
}

func (h *hochoAccumulator) add(index int64, chash []byte) error {
	if !h.enabled {
		return nil
	}
	h.pendingChunk[index] = append([]byte(nil), chash...)

	for {
		next, ok := h.pendingChunk[h.expectedIdx]
		if !ok {
			break
		}

		var lenPrefix [4]byte
		binary.BigEndian.PutUint32(lenPrefix[:], uint32(len(next)))
		if _, err := h.hasher.Write(lenPrefix[:]); err != nil {
			return err
		}
		if _, err := h.hasher.Write(next); err != nil {
			return err
		}

		delete(h.pendingChunk, h.expectedIdx)
		h.expectedIdx += cnst.ChonkSize
	}

	return nil
}

func (h *hochoAccumulator) finalize() (string, error) {
	if !h.enabled {
		return "", nil
	}
	if len(h.pendingChunk) != 0 {
		return "", fmt.Errorf("hocho accumulation incomplete: %d chunks pending", len(h.pendingChunk))
	}
	return base64.StdEncoding.EncodeToString(h.hasher.Sum(nil)), nil
}

// storeEvidenceDataBatchOwner is the production evidence ingest path.
// Returns a hocho evidence hash only when hocho strategy is active.
func storeEvidenceDataBatchOwner(parentCtx context.Context, infile structs.InputFile) (evidenceHochoHash string, err error) {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	bar := progressbar.NewOptions64(
		infile.GetSize(),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)

	var simhashWriter *simhashAsyncWriter
	if cnst.ENABLESIMHASH {
		simhashWriter = newSimhashAsyncWriter(infile.GetDB(), cnst.GetMaxThreadCount())
		defer func() {
			simhashErr := simhashWriter.wait()
			if err == nil && simhashErr != nil {
				err = simhashErr
			}
		}()
	}

	var containerMgr *fio.ContainerManager
	var blockMgr *fio.BlockManager

	if cnst.CONTAINERMODE {
		containerMgr = fio.NewContainerManager(infile.GetDB().Opts().Dir)
		defer closeContainerManager(&err, containerMgr)

		if cnst.HIERARCHICALINDEX {
			blockMgr = fio.NewBlockManager(infile.GetDB().Opts().Dir, containerMgr)
			defer closeBlockManager(&err, blockMgr)
		}
	}

	if !cnst.CONTAINERMODE && cnst.StoreAsyncFileWrites {
		fio.StartAsyncFileWriter(0)
		defer func() {
			mergeDeferredErr(&err, fio.CloseAsyncFileWriter())
		}()
	}

	// ctx is cancelled when parent context is canceled or writer exits with an error so that
	// in-flight workers do not block indefinitely on taskCh.
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// Tune worker/queue shape by storage mode; container/hierarchical paths
	// include serialized components and benefit from steadier queue pressure.
	workerCount, taskQueueDepth := cnst.GetStorePipelineTuning(cnst.CONTAINERMODE, cnst.HIERARCHICALINDEX)
	taskCh := make(chan chunkTask, taskQueueDepth)
	writerResCh := make(chan writerResult, 1)

	go runBatchOwnerWriter(
		infile.GetHash(),
		infile.GetStartIndex(),
		infile.GetDB(),
		taskCh,
		cancel,
		writerResCh,
	)

	jobCh := make(chan chunkJob, taskQueueDepth)
	workerResCh := make(chan workerRes, workerCount+1)
	seenChunks := newShardedChunkSet()
	mappedFile := infile.GetMappedFile()
	var firstWorkerErr error

	for i := 0; i < workerCount; i++ {
		go batchOwnerHashWorkerLoop(ctx, mappedFile, infile.GetDB(), containerMgr, blockMgr, seenChunks, jobCh, taskCh, workerResCh, simhashWriter)
	}

	totalJobs := 0
	completedJobs := 0
	jobChClosed := false
	for storeIndex := infile.GetStartIndex(); storeIndex < infile.GetSize(); storeIndex += cnst.ChonkSize {
		if err := ctx.Err(); err != nil {
			close(jobCh)
			jobChClosed = true
			for completedJobs < totalJobs {
				res := <-workerResCh
				completedJobs++
				captureFirstWorkerErr(res.err, &firstWorkerErr)
				bar.Add64(res.bytes)
			}
			break
		}
		var buffsize int64
		if infile.GetSize()-storeIndex <= cnst.ChonkSize {
			buffsize = infile.GetSize() - storeIndex
		} else {
			buffsize = cnst.ChonkSize
		}
		chonkEnd := storeIndex + buffsize
		job := chunkJob{idx: storeIndex, chonkEnd: chonkEnd}

		for {
			select {
			case jobCh <- job:
				totalJobs++
				goto enqueued
			case res := <-workerResCh:
				completedJobs++
				captureFirstWorkerErr(res.err, &firstWorkerErr)
				bar.Add64(res.bytes)
			case <-ctx.Done():
				close(jobCh)
				jobChClosed = true
				for completedJobs < totalJobs {
					res := <-workerResCh
					completedJobs++
					captureFirstWorkerErr(res.err, &firstWorkerErr)
					bar.Add64(res.bytes)
				}
				goto doneScheduling
			}
		}

	enqueued:
	}

doneScheduling:
	if ctx.Err() != nil && firstWorkerErr == nil {
		firstWorkerErr = ctx.Err()
	}
	if !jobChClosed {
		close(jobCh)
	}

	for completedJobs < totalJobs {
		res := <-workerResCh
		completedJobs++
		captureFirstWorkerErr(res.err, &firstWorkerErr)
		bar.Add64(res.bytes)
	}

	close(taskCh)
	writerRes := <-writerResCh
	bar.Finish()
	fmt.Fprintln(os.Stderr)
	barErr := bar.Close()

	if firstWorkerErr != nil {
		return "", firstWorkerErr
	}
	if writerRes.err != nil {
		return "", writerRes.err
	}
	if barErr != nil {
		return "", barErr
	}

	return writerRes.hochoHash, nil
}

func mergeDeferredErr(target *error, incoming error) {
	if target == nil || *target != nil || incoming == nil {
		return
	}
	*target = incoming
}

func closeContainerManager(target *error, containerMgr *fio.ContainerManager) {
	if containerMgr == nil {
		return
	}
	mergeDeferredErr(target, containerMgr.Close())
}

func closeBlockManager(target *error, blockMgr *fio.BlockManager) {
	if blockMgr == nil {
		return
	}
	mergeDeferredErr(target, blockMgr.Close())
}

func runBatchOwnerWriter(
	fhash []byte,
	startIndex int64,
	db *badger.DB,
	taskCh <-chan chunkTask,
	cancel context.CancelFunc,
	writerResCh chan<- writerResult,
) {
	hochoHash, werr := batchOwnerWriteLoop(fhash, startIndex, db, taskCh)
	if werr != nil {
		cancel()
	}
	writerResCh <- writerResult{hochoHash: hochoHash, err: werr}
}

func batchOwnerHashWorkerLoop(
	ctx context.Context,
	mappedFile []byte,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
	seenChunks *shardedChunkSet,
	jobCh <-chan chunkJob,
	taskCh chan<- chunkTask,
	workerResCh chan<- workerRes,
	simhashWriter *simhashAsyncWriter,
) {
	for job := range jobCh {
		err := batchOwnerHashWorker(
			ctx,
			mappedFile,
			job.idx,
			job.chonkEnd,
			db,
			containerMgr,
			blockMgr,
			seenChunks,
			taskCh,
			simhashWriter,
		)
		workerResCh <- workerRes{err: err, bytes: job.chonkEnd - job.idx}
	}
}

func batchOwnerHashWorker(
	ctx context.Context,
	mappedFile []byte,
	idx, chonkEnd int64,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
	seenChunks *shardedChunkSet,
	taskCh chan<- chunkTask,
	simhashWriter *simhashAsyncWriter,
) error {
	chunkBytes := mappedFile[idx:chonkEnd]
	chash, herr := util.GetChonkHash(chunkBytes, cnst.GetHashAlgo())
	if herr != nil {
		return herr
	}

	if simhashWriter != nil {
		simhashWriter.enqueue(chunkBytes, chash)
	}

	var cmeta []byte
	if !seenChunks.loadOrStore(chash) {
		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		if containerMgr != nil {
			// Container mode: WriteChunkToContainer cannot detect chunks from
			// previous sessions, so we must probe the DB first.
			err := dbio.PingNode(ckey, db)
			if errors.Is(err, badger.ErrKeyNotFound) {
				cmeta, err = dbio.MaterializeChonkNode(ckey, chunkBytes, db, containerMgr, blockMgr)
				if err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
		} else {
			// File mode: WriteChonk is idempotent (os.Stat check); call
			// MaterializeChonkNode directly without a prior DB probe.
			var merr error
			cmeta, merr = dbio.MaterializeChonkNode(ckey, chunkBytes, db, nil, blockMgr)
			if merr != nil {
				return merr
			}
		}
	}

	select {
	case taskCh <- chunkTask{index: idx, chash: chash, cmeta: cmeta}:
		return nil
	case <-ctx.Done():
		return context.Canceled
	}
}

func captureFirstWorkerErr(err error, firstWorkerErr *error) {
	if err == nil || *firstWorkerErr != nil {
		return
	}
	*firstWorkerErr = err
}

// batchOwnerWriteLoop is the single writer goroutine. It owns the WriteBatch
// and revRelAppendBuffer exclusively — no synchronisation primitives are
// needed for those structures here.
func batchOwnerWriteLoop(
	fhash []byte,
	startIndex int64,
	db *badger.DB,
	taskCh <-chan chunkTask,
) (string, error) {
	batch, err := util.InitBatch(db)
	if err != nil {
		return "", err
	}

	revRelBuffer := newRevRelAppendBuffer(256)
	hocho := newHochoAccumulator(cnst.StoreHashStrategy == cnst.HochoHashStrategy, startIndex)

	for task := range taskCh {
		if err := hocho.add(task.index, task.chash); err != nil {
			batch.Cancel()
			return "", err
		}
		if len(task.cmeta) > 0 {
			ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, task.chash)
			if err := dbio.SetBatchNode(ckey, task.cmeta, batch); err != nil {
				batch.Cancel()
				return "", err
			}
		}
		if err := processRel(task.index, fhash, task.chash, db, batch); err != nil {
			batch.Cancel()
			return "", err
		}
		if err := processRevRel(task.index, fhash, task.chash, batch, revRelBuffer); err != nil {
			batch.Cancel()
			return "", err
		}
	}

	if revRelBuffer != nil {
		if err := revRelBuffer.flush(fhash, db, batch); err != nil {
			batch.Cancel()
			return "", err
		}
	}

	hochoHash, err := hocho.finalize()
	if err != nil {
		batch.Cancel()
		return "", err
	}

	if err := batch.Flush(); err != nil {
		return "", err
	}

	return hochoHash, nil
}

// batchOwnerProcessChonk mirrors processChonk but uses a plain map for
// seenChunks since only the single writer goroutine calls it.
// When cdata is nil or empty the function skips the existence probe and store;
// this is used by benchmark helpers that only exercise the rel/revrel write path.
func batchOwnerProcessChonk(
	cdata, chash []byte,
	db *badger.DB,
	batch *badger.WriteBatch,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
	seenChunks map[string]struct{},
) error {
	if len(cdata) == 0 {
		return nil
	}
	hashKey := string(chash)
	if _, alreadySeen := seenChunks[hashKey]; alreadySeen {
		return nil
	}

	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	if containerMgr != nil {
		// Container mode: requires DB probe to avoid re-appending on resume.
		err := dbio.PingNode(ckey, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			if setErr := dbio.SetBatchChonkNode(ckey, cdata, db, batch, containerMgr, blockMgr); setErr != nil {
				return setErr
			}
			seenChunks[hashKey] = struct{}{}
			return nil
		}
		if err == nil {
			seenChunks[hashKey] = struct{}{}
		}
		return err
	}
	// File mode: WriteChonk is idempotent, call unconditionally.
	if setErr := dbio.SetBatchChonkNode(ckey, cdata, db, batch, nil, blockMgr); setErr != nil {
		return setErr
	}
	seenChunks[hashKey] = struct{}{}
	return nil
}
