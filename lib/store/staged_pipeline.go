package store

import (
	"context"
	"encoding/base64"
	"encoding/binary"
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
	"github.com/google/uuid"
	"github.com/schollz/progressbar/v3"
	"github.com/zeebo/blake3"
)

const stagedDiskBatchSize = 128

// Chunk is the stage payload emitted by ingestion.
type Chunk struct {
	ID   uint64
	Data []byte
}

// HashedChunk is emitted by hashing workers and consumed by dedup stage.
type HashedChunk struct {
	ID   uint64
	Sum  [32]byte
	Data []byte
}

type ingestedChunk struct {
	chunk  Chunk
	offset int64
}

type hashedChunkEnvelope struct {
	hashed HashedChunk
	offset int64
}

type stagedWriteChunk struct {
	offset int64
	hash   [32]byte
	data   []byte
}

type stagedWriterResult struct {
	hochoHash string
	err       error
}

type stageErr struct {
	err error
}

// storeEvidenceDataStaged runs an explicit staged ingest pipeline:
// ingestion -> hashing -> dedup -> batched disk writer.
func storeEvidenceDataStaged(parentCtx context.Context, infile structs.InputFile) (evidenceHochoHash string, err error) {
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

	workerCount, queueDepth := cnst.GetStorePipelineTuning(cnst.CONTAINERMODE, cnst.HIERARCHICALINDEX)
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	hashCh := make(chan ingestedChunk, queueDepth)
	dedupCh := make(chan hashedChunkEnvelope, queueDepth)
	writeCh := make(chan stagedWriteChunk, queueDepth)
	stageErrCh := make(chan stageErr, workerCount+4)
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

	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		hochoHash, werr := stagedDiskWriterLoop(infile.GetHash(), infile.GetStartIndex(), infile.GetDB(), containerMgr, blockMgr, writeCh)
		writerResCh <- stagedWriterResult{hochoHash: hochoHash, err: werr}
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
	for i := 0; i < workerCount; i++ {
		hashWG.Add(1)
		go func() {
			defer hashWG.Done()
			if herr := hashStageWorkerLoop(ctx, hashCh, dedupCh, simhashWriter); herr != nil {
				reportErr(herr)
			}
		}()
	}

	var ingestWG sync.WaitGroup
	ingestWG.Add(1)
	go func() {
		defer ingestWG.Done()
		defer close(hashCh)
		if ierr := ingestStageLoop(ctx, infile.GetMappedFile(), infile.GetStartIndex(), infile.GetSize(), hashCh, bar); ierr != nil {
			reportErr(ierr)
		}
	}()

	ingestWG.Wait()
	hashWG.Wait()
	close(dedupCh)
	dedupWG.Wait()
	writerWG.Wait()

	var writerRes stagedWriterResult
	select {
	case writerRes = <-writerResCh:
	default:
	}

	bar.Finish()
	fmt.Fprintln(os.Stderr)
	barErr := bar.Close()

	select {
	case serr := <-stageErrCh:
		if firstErr == nil {
			firstErr = serr.err
		}
	default:
	}

	if firstErr != nil {
		return "", firstErr
	}
	if writerRes.err != nil {
		return "", writerRes.err
	}
	if barErr != nil {
		return "", barErr
	}

	return writerRes.hochoHash, nil
}

func ingestStageLoop(
	ctx context.Context,
	mappedFile []byte,
	startIndex int64,
	totalSize int64,
	hashCh chan<- ingestedChunk,
	bar *progressbar.ProgressBar,
) error {
	for offset := startIndex; offset < totalSize; offset += cnst.ChonkSize {
		var chunkSize int64
		if totalSize-offset <= cnst.ChonkSize {
			chunkSize = totalSize - offset
		} else {
			chunkSize = cnst.ChonkSize
		}
		end := offset + chunkSize
		chunkID, err := nextChunkIDV7()
		if err != nil {
			return err
		}
		payload := ingestedChunk{
			chunk: Chunk{
				ID:   chunkID,
				Data: mappedFile[offset:end],
			},
			offset: offset,
		}

		select {
		case hashCh <- payload:
			bar.Add64(chunkSize)
		case <-ctx.Done():
			return context.Canceled
		}
	}

	return nil
}

func hashStageWorkerLoop(
	ctx context.Context,
	hashCh <-chan ingestedChunk,
	dedupCh chan<- hashedChunkEnvelope,
	simhashWriter *simhashAsyncWriter,
) error {
	digest := blake3.New()

	for ingestChunk := range hashCh {
		digest.Reset()
		if _, err := digest.Write(ingestChunk.chunk.Data); err != nil {
			return err
		}

		sumBytes := digest.Sum(nil)
		var sum [32]byte
		copy(sum[:], sumBytes)

		if simhashWriter != nil {
			simhashWriter.enqueue(ingestChunk.chunk.Data, sum[:])
		}

		payload := hashedChunkEnvelope{
			hashed: HashedChunk{
				ID:   ingestChunk.chunk.ID,
				Sum:  sum,
				Data: ingestChunk.chunk.Data,
			},
			offset: ingestChunk.offset,
		}

		select {
		case dedupCh <- payload:
		case <-ctx.Done():
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
		if seenChunks.loadOrStore(hashed.hashed.Sum[:]) {
			continue
		}

		payload := stagedWriteChunk{
			offset: hashed.offset,
			hash:   hashed.hashed.Sum,
			data:   hashed.hashed.Data,
		}

		select {
		case writeCh <- payload:
		case <-ctx.Done():
			return context.Canceled
		}
	}

	return nil
}

func stagedDiskWriterLoop(
	fhash []byte,
	startIndex int64,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
	writeCh <-chan stagedWriteChunk,
) (string, error) {
	batch, err := util.InitBatch(db)
	if err != nil {
		return "", err
	}

	revRelBuffer := newRevRelAppendBuffer(256)
	hocho := newHochoAccumulator(cnst.StoreHashStrategy == cnst.HochoHashStrategy, startIndex)
	pending := make([]stagedWriteChunk, 0, stagedDiskBatchSize)

	flushPending := func() error {
		for _, item := range pending {
			chash := item.hash[:]
			ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)

			if err := hocho.add(item.offset, chash); err != nil {
				return err
			}

			cmeta, err := materializeChunkMeta(ckey, item.data, db, containerMgr, blockMgr)
			if err != nil {
				return err
			}

			if len(cmeta) > 0 {
				if err := dbio.SetBatchNode(ckey, cmeta, batch); err != nil {
					return err
				}
			}

			if err := processRel(item.offset, fhash, chash, db, batch); err != nil {
				return err
			}
			if err := processRevRel(item.offset, fhash, chash, batch, revRelBuffer); err != nil {
				return err
			}
		}
		pending = pending[:0]
		return nil
	}

	for item := range writeCh {
		pending = append(pending, item)
		if len(pending) < stagedDiskBatchSize {
			continue
		}
		if err := flushPending(); err != nil {
			batch.Cancel()
			return "", err
		}
	}

	if len(pending) > 0 {
		if err := flushPending(); err != nil {
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

func materializeChunkMeta(
	ckey []byte,
	cdata []byte,
	db *badger.DB,
	containerMgr *fio.ContainerManager,
	blockMgr *fio.BlockManager,
) ([]byte, error) {
	if containerMgr != nil {
		// Container mode requires a DB probe to avoid re-appending chunks from
		// previous sessions.
		err := dbio.PingNode(ckey, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return dbio.MaterializeChonkNode(ckey, cdata, db, containerMgr, blockMgr)
		}
		if err != nil {
			return nil, err
		}
		return nil, nil
	}

	return dbio.MaterializeChonkNode(ckey, cdata, db, nil, blockMgr)
}

func nextChunkIDV7() (uint64, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(id[8:16]), nil
}

func finalizeHochoFromChunkHashes(startIndex int64, orderedChunkHashes map[int64][]byte) (string, error) {
	if len(orderedChunkHashes) == 0 {
		return "", nil
	}
	hasher := cnst.GetHashAlgo(true)
	for idx := startIndex; ; idx += cnst.ChonkSize {
		chunkHash, ok := orderedChunkHashes[idx]
		if !ok {
			break
		}
		var lenPrefix [4]byte
		binary.BigEndian.PutUint32(lenPrefix[:], uint32(len(chunkHash)))
		if _, err := hasher.Write(lenPrefix[:]); err != nil {
			return "", err
		}
		if _, err := hasher.Write(chunkHash); err != nil {
			return "", err
		}
	}
	return base64.StdEncoding.EncodeToString(hasher.Sum(nil)), nil
}
