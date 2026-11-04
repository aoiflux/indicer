package store

import (
	"context"
	"encoding/binary"
	"hash"
	"indicer/internal/diskspace"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"
	"sync"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
)

const stagedDiskBatchSize = 128

// Chunk is the stage payload emitted by ingestion.
type Chunk struct {
	ID   uint64
	Data []byte
}

// HashedChunk is emitted by hashing workers and consumed by dedup stage.
// Sum is the chunk hash in the database's chunk-hash algorithm.
type HashedChunk struct {
	ID   uint64
	Sum  []byte
	Data []byte
}

type ingestedChunk struct {
	chunk  Chunk
	offset int64
	fc     *feedChunk
}

type hashedChunkEnvelope struct {
	hashed HashedChunk
	offset int64
	fc     *feedChunk
}

// stagedWriteChunk is one chunk for the writer. dup marks a chunk already
// sent earlier in this session: its bytes are stored once, but every
// occurrence still needs its relation, reverse relation and hocho entry. The
// writer releases fc once the chunk is stored.
type stagedWriteChunk struct {
	offset int64
	hash   []byte
	data   []byte
	dup    bool
	fc     *feedChunk
}

type stagedWriterResult struct {
	root []byte
	err  error
}

type stageErr struct {
	err error
}

// ingestChunksStaged runs an explicit staged ingest pipeline:
// feed -> hashing -> dedup -> batched disk writer.
func ingestChunksStaged(parentCtx context.Context, p ChunkPass) (res ChunkResult, err error) {
	shape := shapeFor(p)
	meter := startPass(parentCtx, p)
	// Registered first, so it runs last: after the simhash writer drained.
	defer func() { meter.done(err) }()

	var simhashWriter *simhashAsyncWriter
	if p.Simhash {
		simhashWriter = newSimhashAsyncWriter(p.DB, cnst.GetMaxThreadCount())
		defer func() {
			simhashErr := simhashWriter.wait()
			if err == nil && simhashErr != nil {
				err = simhashErr
			}
		}()
	}

	writes := joinChunkWriter(p.DB)
	defer writes.leave(&err)

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	queueDepth := shape.queueDepth
	hashCh := make(chan ingestedChunk, queueDepth)
	dedupCh := make(chan hashedChunkEnvelope, queueDepth)
	writeCh := make(chan stagedWriteChunk, queueDepth)
	stageErrCh := make(chan stageErr, shape.workers+4)
	writerResCh := make(chan stagedWriterResult, 1)

	var firstErr error
	var firstErrOnce sync.Once
	reportErr := func(incoming error) {
		if incoming == nil {
			return
		}
		firstErrOnce.Do(func() {
			firstErr = incoming
			cancel()
		})
		select {
		case stageErrCh <- stageErr{err: incoming}:
		default:
		}
	}

	// The writer holds chunks until it stores them, so its batch must stay
	// well below the feed's ring, or the feed would wait for slots the
	// writer waits to fill.
	batchSize := max(1, min(stagedDiskBatchSize, shape.slots/4))
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		root, werr := stagedDiskWriterLoop(p.ID, shape, batchSize, p.DB, writes, writeCh, meter)
		writerResCh <- stagedWriterResult{root: root, err: werr}
		if werr != nil {
			reportErr(werr)
		}
	}()

	seenChunks := newShardedChunkSet()
	var dedupWG sync.WaitGroup
	dedupWG.Add(1)
	go func() {
		defer dedupWG.Done()
		defer close(writeCh)
		if derr := dedupStageLoop(ctx, seenChunks, dedupCh, writeCh); derr != nil {
			reportErr(derr)
		}
	}()

	var hashWG sync.WaitGroup
	for i := 0; i < shape.workers; i++ {
		hashWG.Add(1)
		go func() {
			defer hashWG.Done()
			if herr := hashStageWorkerLoop(ctx, shape.chunkHash, hashCh, dedupCh, simhashWriter); herr != nil {
				reportErr(herr)
			}
		}()
	}

	tap := newDigestTap(shape.fileHash, p.Ranges, p.EWF)
	feed := shape.feed(ctx, p, tap)
	var chunks int64
	var ingestWG sync.WaitGroup
	ingestWG.Add(1)
	go func() {
		defer ingestWG.Done()
		defer close(hashCh)
		n, ierr := ingestStageLoop(ctx, feed, hashCh, meter)
		chunks = n
		if ierr != nil {
			reportErr(ierr)
		}
	}()

	ingestWG.Wait()
	feedErr := feed.wait()
	hashWG.Wait()
	close(dedupCh)
	dedupWG.Wait()
	writerWG.Wait()

	var writerRes stagedWriterResult
	select {
	case writerRes = <-writerResCh:
	default:
	}

	select {
	case serr := <-stageErrCh:
		if firstErr == nil {
			firstErr = serr.err
		}
	default:
	}

	if err := passErr(parentCtx, feedErr, firstErr); err != nil {
		return ChunkResult{}, err
	}
	if writerRes.err != nil {
		return ChunkResult{}, writerRes.err
	}
	return passResult(tap, writerRes.root, chunks, meter), nil
}

// ingestStageLoop hands the feed's chunks to the hash stage and returns how
// many it handed on. It drains the feed even after a failure, releasing what
// it does not pass on.
func ingestStageLoop(
	ctx context.Context,
	feed *chunkFeed,
	hashCh chan<- ingestedChunk,
	meter *passMeter,
) (int64, error) {
	var n int64
	var err error
	for c := range feed.chunks() {
		if err != nil {
			c.release()
			continue
		}
		chunkID, idErr := nextChunkIDV7()
		if idErr != nil {
			c.release()
			err = idErr
			continue
		}
		payload := ingestedChunk{chunk: Chunk{ID: chunkID, Data: c.data}, offset: c.off, fc: c}
		select {
		case hashCh <- payload:
			meter.fed(int64(len(c.data)))
			n++
		case <-ctx.Done():
			c.release()
			err = context.Canceled
		}
	}
	return n, err
}

func hashStageWorkerLoop(
	ctx context.Context,
	newHash func() hash.Hash,
	hashCh <-chan ingestedChunk,
	dedupCh chan<- hashedChunkEnvelope,
	simhashWriter *simhashAsyncWriter,
) error {
	// The chunk hash is the database's, as in the batch pipeline (CFG-6).
	digest := newHash()

	for ingestChunk := range hashCh {
		digest.Reset()
		if _, err := digest.Write(ingestChunk.chunk.Data); err != nil {
			ingestChunk.fc.release()
			return err
		}
		sum := digest.Sum(nil)

		if simhashWriter != nil {
			simhashWriter.enqueue(ingestChunk.chunk.Data, sum)
		}

		payload := hashedChunkEnvelope{
			hashed: HashedChunk{
				ID:   ingestChunk.chunk.ID,
				Sum:  sum,
				Data: ingestChunk.chunk.Data,
			},
			offset: ingestChunk.offset,
			fc:     ingestChunk.fc,
		}

		select {
		case dedupCh <- payload:
		case <-ctx.Done():
			ingestChunk.fc.release()
			return context.Canceled
		}
	}

	return nil
}

func dedupStageLoop(
	ctx context.Context,
	seenChunks *shardedChunkSet,
	dedupCh <-chan hashedChunkEnvelope,
	writeCh chan<- stagedWriteChunk,
) error {
	for hashed := range dedupCh {
		// A repeated chunk still goes to the writer (STO-9): dropping it
		// here lost its relation, and the evidence could not be restored.
		payload := stagedWriteChunk{
			offset: hashed.offset,
			hash:   hashed.hashed.Sum,
			data:   hashed.hashed.Data,
			dup:    seenChunks.loadOrStore(hashed.hashed.Sum),
			fc:     hashed.fc,
		}

		select {
		case writeCh <- payload:
		case <-ctx.Done():
			hashed.fc.release()
			return context.Canceled
		}
	}

	return nil
}

func stagedDiskWriterLoop(
	fhash []byte,
	shape passShape,
	batchSize int,
	db *badger.DB,
	writes *passChunks,
	writeCh <-chan stagedWriteChunk,
	meter *passMeter,
) ([]byte, error) {
	batch, err := util.InitBatch(db)
	if err != nil {
		return nil, err
	}

	barrier := newChunkMetaBarrier(dbio.KVDir(db), writes.containers(), batch)
	hocho := newHochoAccumulator(shape.fileHash, shape.chunk, 0)
	pending := make([]stagedWriteChunk, 0, batchSize)

	flushPending := func() error {
		defer func() {
			for _, item := range pending {
				if item.fc != nil {
					item.fc.release()
				}
			}
			pending = pending[:0]
		}()
		for _, item := range pending {
			chash := item.hash

			if err := hocho.add(item.offset, chash); err != nil {
				return err
			}

			var cmeta []byte
			ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
			if !item.dup {
				var err error
				var written int64
				var fresh bool
				if cmeta, written, fresh, err = materializeChunkMeta(ckey, item.data, db, writes); err != nil {
					return err
				}
				if fresh {
					meter.stored(int64(len(item.data)), written)
				}
			}
			if err := barrier.record(ckey, cmeta, int64(len(item.data))); err != nil {
				return err
			}

			if err := processRel(item.offset, fhash, chash, db, batch); err != nil {
				return err
			}
			if err := processRevRel(shape.revrel, item.offset, fhash, chash, batch); err != nil {
				return err
			}
		}
		return nil
	}

	for item := range writeCh {
		pending = append(pending, item)
		if len(pending) < batchSize {
			continue
		}
		if err := flushPending(); err != nil {
			batch.Cancel()
			return nil, err
		}
	}

	if len(pending) > 0 {
		if err := flushPending(); err != nil {
			batch.Cancel()
			return nil, err
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

// materializeChunkMeta stores the chunk under ckey unless the database
// already holds it intact, and returns the metadata to record for it: nil
// when the chunk was already stored. A chunk another pass beside this one
// stores, or stored and has not committed, is not stored again: its
// metadata is returned, with fresh false and written 0 (passChunks.store).
// The database is asked inside the claim, so a pass that ends between the
// two cannot be missed by both.
func materializeChunkMeta(
	ckey []byte,
	cdata []byte,
	db *badger.DB,
	writes *passChunks,
) (meta []byte, written int64, fresh bool, err error) {
	return writes.store(ckey, func() ([]byte, int64, error) {
		stored, err := dbio.ChunkStored(ckey, db)
		if err != nil || stored {
			return nil, 0, err
		}
		if dir := dbio.KVDir(db); dir != "" {
			if err := diskspace.GuardFor(dir).Add(len(cdata)); err != nil {
				return nil, 0, err
			}
		}
		return materializeChunk(ckey, cdata, db, writes.containers())
	})
}

// materializeChunk encodes one chunk and writes its blob or container record;
// tests replace it to count the chunks an ingest materialises.
var materializeChunk = dbio.MaterializeChonkNode

func nextChunkIDV7() (uint64, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(id[8:16]), nil
}
