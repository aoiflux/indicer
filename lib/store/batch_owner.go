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
	"encoding/binary"
	"fmt"
	"hash"
	"indicer/lib/cnst"
	"indicer/lib/fio"
	"indicer/lib/util"
	"sync"

	"github.com/dgraph-io/badger/v4"
)

// chunkTask carries per-chunk outputs from hash/materialize workers to the
// single batch-owner goroutine. cmeta holds pre-materialized encoded chunk
// metadata for batch DB persistence, which the writer holds back until the
// chunk bytes are durable.
type chunkTask struct {
	index int64
	chash []byte
	cmeta []byte
	size  int64 // bytes of evidence content in the chunk
}

type writerResult struct {
	root []byte
	err  error
}

// hochoAccumulator computes the hocho root, the hash over every chunk hash in
// order (length-prefixed), from chunk hashes that arrive in any order.
type hochoAccumulator struct {
	hasher       hash.Hash
	stride       int64
	expectedIdx  int64
	pendingChunk map[int64][]byte
}

func newHochoAccumulator(newHash func() hash.Hash, stride, startIndex int64) *hochoAccumulator {
	return &hochoAccumulator{
		hasher:       newHash(),
		stride:       stride,
		expectedIdx:  startIndex,
		pendingChunk: make(map[int64][]byte),
	}
}

func (h *hochoAccumulator) add(index int64, chash []byte) error {
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
		h.expectedIdx += h.stride
	}

	return nil
}

func (h *hochoAccumulator) finalize() ([]byte, error) {
	if len(h.pendingChunk) != 0 {
		return nil, fmt.Errorf("hocho accumulation incomplete: %d chunks pending", len(h.pendingChunk))
	}
	return h.hasher.Sum(nil), nil
}

// ingestChunksBatchOwner is the production chunk pass.
func ingestChunksBatchOwner(parentCtx context.Context, p ChunkPass) (res ChunkResult, err error) {
	shape := shapeFor(p)
	bar := newPassBar(p)

	var simhashWriter *simhashAsyncWriter
	if cnst.ENABLESIMHASH {
		simhashWriter = newSimhashAsyncWriter(p.DB, cnst.GetMaxThreadCount())
		defer func() {
			simhashErr := simhashWriter.wait()
			if err == nil && simhashErr != nil {
				err = simhashErr
			}
		}()
	}

	var containerMgr *fio.ContainerManager
	if cnst.CONTAINERMODE {
		containerMgr = fio.NewContainerManager(p.DB.Opts().Dir)
		defer closeContainerManager(&err, containerMgr)
	}

	// ctx is cancelled when the caller cancels or any stage fails, which
	// stops the feed and lets every worker drain.
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	taskCh := make(chan chunkTask, shape.queueDepth)
	writerResCh := make(chan writerResult, 1)
	go runBatchOwnerWriter(p.ID, shape, p.DB, containerMgr, taskCh, cancel, writerResCh)

	tap := newDigestTap(shape.fileHash, p.Ranges, p.EWF)
	feed := shape.feed(ctx, p, tap)
	seenChunks := newShardedChunkSet()

	var mu sync.Mutex
	var firstWorkerErr error
	var chunks int64
	var wg sync.WaitGroup
	for range shape.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := shape.chunkHash()
			for c := range feed.chunks() {
				n := int64(len(c.data))
				werr := batchOwnerHashWorker(ctx, c.off, c.data, h, p.DB, containerMgr, seenChunks, taskCh, simhashWriter)
				c.release()
				mu.Lock()
				if werr != nil {
					captureFirstWorkerErr(werr, &firstWorkerErr)
					cancel()
				} else {
					chunks++
					bar.Add64(n)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	feedErr := feed.wait()

	close(taskCh)
	writerRes := <-writerResCh
	finishPassBar(p, bar)

	if err := passErr(parentCtx, feedErr, firstWorkerErr); err != nil {
		return ChunkResult{}, err
	}
	if writerRes.err != nil {
		return ChunkResult{}, writerRes.err
	}
	return passResult(tap, writerRes.root, chunks), nil
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

func runBatchOwnerWriter(
	fhash []byte,
	shape passShape,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	taskCh <-chan chunkTask,
	cancel context.CancelFunc,
	writerResCh chan<- writerResult,
) {
	root, werr := batchOwnerWriteLoop(fhash, shape, db, containerMgr, taskCh)
	if werr != nil {
		cancel()
	}
	writerResCh <- writerResult{root: root, err: werr}
}

// batchOwnerHashWorker hashes the chunk data at idx, stores it unless this
// pass or the database already did, and hands the result to the writer. h is
// the worker's own chunk hasher.
func batchOwnerHashWorker(
	ctx context.Context,
	idx int64,
	chunkBytes []byte,
	h hash.Hash,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	seenChunks *shardedChunkSet,
	taskCh chan<- chunkTask,
	simhashWriter *simhashAsyncWriter,
) error {
	h.Reset()
	h.Write(chunkBytes)
	chash := h.Sum(nil)

	if simhashWriter != nil {
		simhashWriter.enqueue(chunkBytes, chash)
	}

	var cmeta []byte
	if !seenChunks.loadOrStore(chash) {
		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		var err error
		if cmeta, err = materializeChunkMeta(ckey, chunkBytes, db, containerMgr); err != nil {
			return err
		}
	}

	select {
	case taskCh <- chunkTask{index: idx, chash: chash, cmeta: cmeta, size: int64(len(chunkBytes))}:
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

// batchOwnerWriteLoop is the single writer goroutine. It owns the WriteBatch,
// and the chunk-metadata barrier exclusively — no synchronisation primitives
// are needed for those structures here. It returns the hocho root.
func batchOwnerWriteLoop(
	fhash []byte,
	shape passShape,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	taskCh <-chan chunkTask,
) ([]byte, error) {
	batch, err := util.InitBatch(db)
	if err != nil {
		return nil, err
	}

	barrier := newChunkMetaBarrier(db.Opts().Dir, containerMgr, batch)
	hocho := newHochoAccumulator(shape.fileHash, shape.chunk, 0)

	// On failure the writer returns at once; its caller cancels the pass,
	// so workers blocked on taskCh give up.
	fail := func(err error) ([]byte, error) {
		batch.Cancel()
		return nil, err
	}
	for task := range taskCh {
		if err := hocho.add(task.index, task.chash); err != nil {
			return fail(err)
		}
		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, task.chash)
		if err := barrier.record(ckey, task.cmeta, task.size); err != nil {
			return fail(err)
		}
		if err := processRel(task.index, fhash, task.chash, db, batch); err != nil {
			return fail(err)
		}
		if err := processRevRel(task.index, fhash, task.chash, batch); err != nil {
			return fail(err)
		}
	}

	if err := barrier.fire(); err != nil {
		batch.Cancel()
		return nil, err
	}

	root, err := hocho.finalize()
	if err != nil {
		batch.Cancel()
		return nil, err
	}

	if err := batch.Flush(); err != nil {
		return nil, err
	}

	return root, nil
}
