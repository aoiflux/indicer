package store

// batch_owner.go implements the production ingest pipeline.
// Fan-out algorithm explanation

// Big picture:
//  1) Worker goroutines read chunk ranges, compute chunk hashes, and copy chunk
//     bytes.
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
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"sync"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

// chunkTask carries per-chunk outputs from hash/materialize workers to the
// single batch owner goroutine. cmeta holds pre-materialized encoded chunk
// metadata for batch DB persistence.
type chunkTask struct {
	index int64
	chash []byte
	cdata []byte
	cmeta []byte
}

type workerRes struct {
	err error
}

// storeEvidenceDataBatchOwner is the production evidence ingest path.
func storeEvidenceDataBatchOwner(infile structs.InputFile) (err error) {
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

	// ctx is cancelled when the writer goroutine exits with an error so that
	// in-flight workers do not block indefinitely on taskCh.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tune worker/queue shape by storage mode; container/hierarchical paths
	// include serialized components and benefit from steadier queue pressure.
	workerCount, taskQueueDepth := cnst.GetStorePipelineTuning(cnst.CONTAINERMODE, cnst.HIERARCHICALINDEX)
	taskCh := make(chan chunkTask, taskQueueDepth)
	writerErrCh := make(chan error, 1)

	go runBatchOwnerWriter(
		infile.GetHash(),
		infile.GetDB(),
		taskCh,
		containerMgr,
		blockMgr,
		cancel,
		writerErrCh,
	)

	workerResCh := make(chan workerRes, workerCount+1)
	seenChunks := &sync.Map{}
	active := 0
	mappedFile := infile.GetMappedFile()
	var firstWorkerErr error
	hasWorkerErr := false

	for storeIndex := infile.GetStartIndex(); storeIndex < infile.GetSize() && !hasWorkerErr; storeIndex += cnst.ChonkSize {
		var buffsize int64
		if infile.GetSize()-storeIndex <= cnst.ChonkSize {
			buffsize = infile.GetSize() - storeIndex
		} else {
			buffsize = cnst.ChonkSize
		}
		chonkEnd := storeIndex + buffsize
		idx := storeIndex

		go batchOwnerHashWorker(ctx, mappedFile, idx, chonkEnd, infile.GetDB(), containerMgr, blockMgr, seenChunks, taskCh, workerResCh, simhashWriter)
		active++

		if active > workerCount {
			res := <-workerResCh
			captureFirstWorkerErr(res.err, &firstWorkerErr, &hasWorkerErr)
			active--
			bar.Add64(buffsize)
		}
	}

	for active > 0 {
		res := <-workerResCh
		captureFirstWorkerErr(res.err, &firstWorkerErr, &hasWorkerErr)
		active--
		bar.Add64(cnst.ChonkSize)
	}

	close(taskCh)
	writerErr := <-writerErrCh

	bar.Add64(cnst.ChonkSize)
	bar.Finish()
	fmt.Fprintln(os.Stderr)
	barErr := bar.Close()

	if firstWorkerErr != nil {
		return firstWorkerErr
	}
	if writerErr != nil {
		return writerErr
	}
	return barErr
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
	db *badger.DB,
	taskCh <-chan chunkTask,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
	cancel context.CancelFunc,
	writerErrCh chan<- error,
) {
	werr := batchOwnerWriteLoop(fhash, db, taskCh, containerMgr, blockMgr)
	if werr != nil {
		cancel()
	}
	writerErrCh <- werr
}

func batchOwnerHashWorker(
	ctx context.Context,
	mappedFile []byte,
	idx, chonkEnd int64,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
	seenChunks *sync.Map,
	taskCh chan<- chunkTask,
	workerResCh chan<- workerRes,
	simhashWriter *simhashAsyncWriter,
) {
	lostChonk := mappedFile[idx:chonkEnd]
	chash, herr := util.GetChonkHash(lostChonk, cnst.GetHashAlgo())
	if herr != nil {
		workerResCh <- workerRes{herr}
		return
	}

	cdata := make([]byte, len(lostChonk))
	copy(cdata, lostChonk)

	if simhashWriter != nil {
		simhashWriter.enqueue(cdata, chash)
	}

	var cmeta []byte
	hashKey := string(chash)
	if _, alreadySeen := seenChunks.LoadOrStore(hashKey, struct{}{}); !alreadySeen {
		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		err := dbio.PingNode(ckey, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			cmeta, err = dbio.MaterializeChonkNode(ckey, cdata, db, containerMgr, blockMgr)
			if err != nil {
				workerResCh <- workerRes{err}
				return
			}
		} else if err != nil {
			workerResCh <- workerRes{err}
			return
		}
	}

	select {
	case taskCh <- chunkTask{index: idx, chash: chash, cmeta: cmeta}:
		workerResCh <- workerRes{}
	case <-ctx.Done():
		workerResCh <- workerRes{context.Canceled}
	}
}

func captureFirstWorkerErr(err error, firstWorkerErr *error, hasWorkerErr *bool) {
	if err == nil || *hasWorkerErr {
		return
	}
	*firstWorkerErr = err
	*hasWorkerErr = true
}

// batchOwnerWriteLoop is the single writer goroutine. It owns the WriteBatch,
// the seenChunks plain map, and the revRelAppendBuffer exclusively — no
// synchronisation primitives are needed for those structures here.
func batchOwnerWriteLoop(
	fhash []byte,
	db *badger.DB,
	taskCh <-chan chunkTask,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
) error {
	batch, err := util.InitBatch(db)
	if err != nil {
		return err
	}

	revRelBuffer := newRevRelAppendBuffer(256)

	for task := range taskCh {
		if len(task.cmeta) > 0 {
			ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, task.chash)
			if err := dbio.SetBatchNode(ckey, task.cmeta, batch); err != nil {
				batch.Cancel()
				return err
			}
		}
		if err := processRel(task.index, fhash, task.chash, db, batch); err != nil {
			batch.Cancel()
			return err
		}
		if err := processRevRel(task.index, fhash, task.chash, batch, revRelBuffer); err != nil {
			batch.Cancel()
			return err
		}
	}

	if revRelBuffer != nil {
		if err := revRelBuffer.flush(fhash, batch); err != nil {
			batch.Cancel()
			return err
		}
	}

	return batch.Flush()
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
