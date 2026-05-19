package store

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// BenchmarkIngestDataPath benchmarks the core ingest synchronous probe path:
// - processChonk (with 1 synchronous PingNode call)
// - processRel (with 1 PingNode call)
// - processRevRel (with 1 GetReverseRelationNode call)
//
// This baseline establishes costs before hot-path probe reduction.
func BenchmarkIngestDataPath(b *testing.B) {
	benchmarks := []struct {
		name         string
		chunks       int
		uniqueChunks float64 // 0.5 = 50% unique, 1.0 = all unique
		description  string
	}{
		{name: "highly_unique/chunks_16", chunks: 16, uniqueChunks: 1.0, description: "all different chunks"},
		{name: "highly_unique/chunks_64", chunks: 64, uniqueChunks: 1.0, description: "all different chunks"},
		{name: "mostly_duplicate/chunks_16", chunks: 16, uniqueChunks: 0.1, description: "only ~10% unique chunks"},
		{name: "mostly_duplicate/chunks_64", chunks: 64, uniqueChunks: 0.1, description: "only ~10% unique chunks"},
		{name: "half_duplicate/chunks_16", chunks: 16, uniqueChunks: 0.5, description: "~50% unique chunks"},
		{name: "half_duplicate/chunks_64", chunks: 64, uniqueChunks: 0.5, description: "~50% unique chunks"},
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatalf("open os.DevNull: %v", err)
	}
	defer devNull.Close()

	oldStderr := os.Stderr
	oldStdout := os.Stdout
	os.Stderr = devNull
	os.Stdout = devNull
	defer func() {
		os.Stderr = oldStderr
		os.Stdout = oldStdout
	}()

	for _, bm := range benchmarks {
		bm := bm
		b.Run(bm.name, func(b *testing.B) {
			db, tempDir, chunkData := setupIngestBenchmarkDataset(b, bm.chunks, bm.uniqueChunks)
			defer os.RemoveAll(tempDir)
			defer db.Close()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := simulateIngestProbes(db, chunkData)
				if err != nil {
					b.Fatalf("ingest probes failed: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}

// BenchmarkIngestMetadataPath measures append-primary metadata ingest behavior
// across many files that share the same chunk sequence. This better
// approximates real ingest pressure than isolated microbenchmarks.
func BenchmarkIngestMetadataPath(b *testing.B) {
	ensureBenchmarkCodecs(b)

	workloads := []struct {
		name   string
		files  int
		chunks int
	}{
		{name: "files_8/chunks_16", files: 8, chunks: 16},
		{name: "files_32/chunks_16", files: 32, chunks: 16},
		{name: "files_8/chunks_64", files: 8, chunks: 64},
	}

	for _, workload := range workloads {
		workload := workload
		chunkHashes := buildBenchmarkChunkHashes(workload.chunks)

		b.Run("append_primary/"+workload.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				db := openRevRelBenchDB(b)
				if err := ingestMetadataWorkload(db, workload.files, chunkHashes); err != nil {
					db.Close()
					b.Fatalf("ingest metadata workload failed: %v", err)
				}
				if err := db.Close(); err != nil {
					b.Fatalf("close bench db: %v", err)
				}
			}
		})
	}
}

// BenchmarkIngestMetadataPathHotspots breaks down the files_32/chunks_16 ingest
// metadata workload so we can identify whether regressions come from key
// construction, relation writes, append-member writes, or batch overhead.
func BenchmarkIngestMetadataPathHotspots(b *testing.B) {
	ensureBenchmarkCodecs(b)

	const fileCount = 32
	const chunkCount = 16

	chunkHashes := buildBenchmarkChunkHashes(chunkCount)

	oldQuickOpt := cnst.QUICKOPT
	cnst.QUICKOPT = true
	defer func() {
		cnst.QUICKOPT = oldQuickOpt
	}()

	b.Run("key_construction_only/files_32/chunks_16", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
				fhash := []byte(fmt.Sprintf("hotspot-file-%03d", fileIdx))
				shard := 0
				if len(fhash) > 0 {
					shard = int(fhash[len(fhash)-1]) % cnst.ReverseRelationAppendShardCount
				}
				for chunkIdx, chash := range chunkHashes {
					index := int64(chunkIdx) * cnst.ChonkSize
					_ = util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)
					_ = util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, index)
					_ = util.AppendToBytesSlice(
						cnst.ReverseRelationAppendNamespace,
						chash,
						cnst.DataSeperator,
						index,
						cnst.DataSeperator,
						shard,
						cnst.DataSeperator,
						base64.RawURLEncoding.EncodeToString(fhash),
					)
				}
			}
		}
	})

	b.Run("process_rel_only/files_32/chunks_16", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		for i := 0; i < b.N; i++ {
			if err := ingestProcessRelOnlyWorkload(db, fileCount, chunkHashes, i*fileCount); err != nil {
				b.Fatalf("processRel-only workload failed: %v", err)
			}
		}
	})

	b.Run("process_revrel_append_primary/files_32/chunks_16", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		for i := 0; i < b.N; i++ {
			if err := ingestProcessRevRelWorkload(db, fileCount, chunkHashes, i*fileCount); err != nil {
				b.Fatalf("processRevRel append-primary workload failed: %v", err)
			}
		}
	})

	b.Run("append_member_only/files_32/chunks_16", func(b *testing.B) {
		db := openRevRelBenchDB(b)
		defer db.Close()

		for i := 0; i < b.N; i++ {
			if err := ingestAppendMemberOnlyWorkload(db, fileCount, chunkHashes, i*fileCount); err != nil {
				b.Fatalf("append-member-only workload failed: %v", err)
			}
		}
	})
}

// setupIngestBenchmarkDataset creates a DB and generates chunk data patterns.
// uniqueChunks controls the deduplication ratio (0.0 = all identical, 1.0 = all unique).
func setupIngestBenchmarkDataset(b *testing.B, chunkCount int, uniqueChunks float64) (*badger.DB, string, [][]byte) {
	tempDir := b.TempDir()

	opts := badger.DefaultOptions(tempDir).
		WithLogger(nil).
		WithValueLogFileSize(1 << 20)
	db, err := badger.Open(opts)
	if err != nil {
		b.Fatalf("open badger: %v", err)
	}

	// Generate chunk data with controlled deduplication.
	chunkData := make([][]byte, chunkCount)
	uniqueCount := int(float64(chunkCount) * uniqueChunks)
	if uniqueCount < 1 {
		uniqueCount = 1
	}

	for i := 0; i < chunkCount; i++ {
		// Cycle through a pool of unique chunks based on the deduplication ratio.
		sourceIdx := i % uniqueCount
		chunkData[i] = make([]byte, cnst.ChonkSize)
		binary.LittleEndian.PutUint64(chunkData[i][0:8], uint64(sourceIdx))
		copy(chunkData[i][8:], []byte("benchmark-chunk"))
	}

	return db, tempDir, chunkData
}

func buildBenchmarkChunkHashes(chunkCount int) [][]byte {
	chunkHashes := make([][]byte, chunkCount)
	for i := 0; i < chunkCount; i++ {
		chunk := make([]byte, cnst.ChonkSize)
		binary.LittleEndian.PutUint64(chunk[0:8], uint64(i))
		copy(chunk[8:], []byte("metadata-benchmark-chunk"))
		chunkHashes[i], _ = util.GetChonkHash(chunk, cnst.GetHashAlgo())
	}
	return chunkHashes
}

func buildBenchmarkChunkData(chunkCount int) [][]byte {
	chunkData := make([][]byte, chunkCount)
	for i := 0; i < chunkCount; i++ {
		chunkData[i] = make([]byte, cnst.ChonkSize)
		binary.LittleEndian.PutUint64(chunkData[i][0:8], uint64(i))
		copy(chunkData[i][8:], []byte("simhash-benchmark-chunk"))
	}
	return chunkData
}

func concatBenchmarkChunkData(chunkData [][]byte) []byte {
	if len(chunkData) == 0 {
		return nil
	}

	mappedFile := make([]byte, 0, len(chunkData)*len(chunkData[0]))
	for _, chunk := range chunkData {
		mappedFile = append(mappedFile, chunk...)
	}
	return mappedFile
}

func ingestBatchOwnerWorkerWorkload(db *badger.DB, mappedFile []byte, fileCount int, fileOffset int, enableSimhash bool) error {
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("simhash-file-%06d", fileOffset+fileIdx))
		if err := ingestBatchOwnerWorkerOnce(db, fhash, mappedFile, enableSimhash); err != nil {
			return err
		}
	}
	return nil
}

func ingestBatchOwnerWorkerOnce(db *badger.DB, fhash, mappedFile []byte, enableSimhash bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workerCount := cnst.GetMaxThreadCount()
	taskCh := make(chan chunkTask, workerCount*2)
	workerResCh := make(chan workerRes, workerCount+1)
	writerErrCh := make(chan error, 1)

	var simhashWriter *simhashAsyncWriter
	if enableSimhash {
		simhashWriter = newSimhashAsyncWriter(db, workerCount)
		defer func() {
			_ = simhashWriter.wait()
		}()
	}

	go runBatchOwnerWriter(fhash, db, taskCh, nil, nil, cancel, writerErrCh)
	seenChunks := &sync.Map{}

	active := 0
	for idx := int64(0); idx < int64(len(mappedFile)); idx += cnst.ChonkSize {
		chonkEnd := idx + cnst.ChonkSize
		if chonkEnd > int64(len(mappedFile)) {
			chonkEnd = int64(len(mappedFile))
		}

		go batchOwnerHashWorker(ctx, mappedFile, idx, chonkEnd, db, nil, nil, seenChunks, taskCh, workerResCh, simhashWriter)
		active++

		if active > workerCount {
			res := <-workerResCh
			if res.err != nil {
				return res.err
			}
			active--
		}
	}

	for active > 0 {
		res := <-workerResCh
		if res.err != nil {
			return res.err
		}
		active--
	}

	close(taskCh)
	if writerErr := <-writerErrCh; writerErr != nil {
		return writerErr
	}

	return nil
}

func ingestMetadataWorkload(db *badger.DB, fileCount int, chunkHashes [][]byte) error {
	return ingestMetadataWorkloadWithOffset(db, fileCount, chunkHashes, 0)
}

func ingestMetadataWorkloadWithOffset(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("ingest-file-%06d", fileOffset+fileIdx))
		batch, err := util.InitBatch(db)
		if err != nil {
			return err
		}
		revRelBuffer := newRevRelAppendBuffer(len(chunkHashes))

		for chunkIdx, chash := range chunkHashes {
			index := int64(chunkIdx) * cnst.ChonkSize
			if err := processRel(index, fhash, chash, db, batch); err != nil {
				batch.Cancel()
				return err
			}
			if err := processRevRel(index, fhash, chash, batch, revRelBuffer); err != nil {
				batch.Cancel()
				return err
			}
		}

		if err := revRelBuffer.flush(fhash, batch); err != nil {
			batch.Cancel()
			return err
		}

		if err := batch.Flush(); err != nil {
			return err
		}
	}

	return nil
}

func ingestProcessRelOnlyWorkload(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("ingest-rel-file-%06d", fileOffset+fileIdx))
		batch := db.NewWriteBatch()

		for chunkIdx, chash := range chunkHashes {
			index := int64(chunkIdx) * cnst.ChonkSize
			if err := processRel(index, fhash, chash, db, batch); err != nil {
				batch.Cancel()
				return err
			}
		}

		if err := batch.Flush(); err != nil {
			return err
		}
	}

	return nil
}

func ingestProcessRevRelWorkload(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	return ingestProcessRevRelWorkloadMode(db, fileCount, chunkHashes, fileOffset, false)
}

func ingestProcessRevRelWorkloadMode(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int, buffered bool) error {
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("ingest-rev-file-%06d", fileOffset+fileIdx))
		batch := db.NewWriteBatch()
		var revRelBuffer *revRelAppendBuffer
		if buffered {
			revRelBuffer = newRevRelAppendBuffer(len(chunkHashes))
		}

		for chunkIdx, chash := range chunkHashes {
			index := int64(chunkIdx) * cnst.ChonkSize
			if err := processRevRel(index, fhash, chash, batch, revRelBuffer); err != nil {
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

		if err := batch.Flush(); err != nil {
			return err
		}
	}

	return nil
}

func ingestAppendMemberOnlyWorkload(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("ingest-app-file-%06d", fileOffset+fileIdx))
		batch := db.NewWriteBatch()
		members := make([]dbio.ReverseRelationAppendMember, 0, len(chunkHashes))

		for chunkIdx, chash := range chunkHashes {
			index := int64(chunkIdx) * cnst.ChonkSize
			members = append(members, dbio.ReverseRelationAppendMember{Chash: chash, Index: index})
		}

		if err := dbio.SetReverseRelationAppendMembers(fhash, members, batch); err != nil {
			batch.Cancel()
			return err
		}

		if err := batch.Flush(); err != nil {
			return err
		}
	}

	return nil
}

// simulateIngestProbes measures the cost of just the DB probe operations,
// not the full write path. This isolates the PingNode and GetReverseRelationNode costs.
func simulateIngestProbes(db *badger.DB, chunkData [][]byte) error {
	// Simulate probing chunks for a single file without writes.
	fileHash := []byte("test-file-hash")
	seenChunks := &sync.Map{}

	for i, cdata := range chunkData {
		chash, err := util.GetChonkHash(cdata, cnst.GetHashAlgo())
		if err != nil {
			return err
		}

		// Simulate processChonk synchronous probe with in-run dedup memoization.
		if err := probeChonkHotpath(chash, db, seenChunks); err != nil {
			return err
		}

		// Simulate processRel probe (1 PingNode call, no writes).
		if err := probeRelHotpath(int64(i), fileHash, db); err != nil {
			return err
		}

		// Simulate processRevRel probe (1 GetReverseRelationNode call, no writes).
		if err := probeRevRelHotpath(chash, db); err != nil {
			return err
		}
	}

	return nil
}

// processChonkHotpath is a write-path simulation helper retained for hotspot
// attribution. The active benchmark path uses probe-only helpers below.
func processChonkHotpath(chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	// First PingNode: check simhash namespace.
	sigKey := util.AppendToBytesSlice(cnst.ChonkSimhashNamespace, chash)
	err := dbio.PingNode(sigKey, db)
	if err == badger.ErrKeyNotFound {
		// Skip simhash enqueue for benchmark; focus on chunk write.
	} else if err != nil {
		return err
	}

	// Second PingNode: check chunk namespace (hot-path probe to measure).
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	err = dbio.PingNode(ckey, db)
	if err == badger.ErrKeyNotFound {
		// Instead of calling SetBatchChonkNode (which requires full encryption setup),
		// directly write the chunk hash to batch to measure just the DB write cost.
		return dbio.SetBatchNode(ckey, chash, batch)
	}

	return err
}

// processRelHotpath is a write-path simulation helper retained for attribution.
func processRelHotpath(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)

	err := dbio.PingNode(relKey, db)
	if err == badger.ErrKeyNotFound {
		return dbio.SetBatchNode(relKey, chash, batch)
	}

	return err
}

// processRevRelHotpath is a write-path simulation helper retained for
// append-member write attribution.
func processRevRelHotpath(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	_ = db
	return dbio.SetReverseRelationAppendMember(chash, index, fhash, batch)
}

// Probe-only versions (no writes) to measure just the read cost.

// probeChonkHotpath measures the synchronous chunk probe cost without writes.
func probeChonkHotpath(chash []byte, db *badger.DB, seenChunks *sync.Map) error {
	hashKey := string(chash)
	if seenChunks != nil {
		if _, alreadySeen := seenChunks.Load(hashKey); alreadySeen {
			return nil
		}
	}

	// Current synchronous hot path only probes chunk existence.
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	err := dbio.PingNode(ckey, db)
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}
	if seenChunks != nil {
		seenChunks.Store(hashKey, struct{}{})
	}

	return nil
}

// probeRelHotpath measures just the PingNode cost for relations.
func probeRelHotpath(index int64, fhash []byte, db *badger.DB) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)
	err := dbio.PingNode(relKey, db)
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}
	return nil
}

// probeRevRelHotpath measures just the GetReverseRelationNode cost.
func probeRevRelHotpath(chash []byte, db *badger.DB) error {
	// For simplicity, probe one reverse-relation entry per chunk
	// (in real code, there can be many relations per chunk).
	revRelKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, 0)
	_, err := dbio.GetReverseRelationNode(revRelKey, db)
	if err != nil && err != badger.ErrKeyNotFound {
		return err
	}
	return nil
}

// BenchmarkIngestBatchOwnership compares the metadata write throughput of the
// current shared-batch fan-out path against the single-writer batch-owner path.
//
// Both modes use the same workload: N files each with M chunks, where every
// file shares the same set of chunk hashes so reverse-relation map merges are
// exercised on every write.
//
// The benchmark measures only the write-loop side (processRel + processRevRel
// issued directly, as in BenchmarkIngestMetadataPath) so the two modes differ
// only in their batch-ownership model, not in hashing work.
func BenchmarkIngestBatchOwnership(b *testing.B) {
	ensureBenchmarkCodecs(b)

	workloads := []struct {
		name   string
		files  int
		chunks int
	}{
		{name: "files_8/chunks_16", files: 8, chunks: 16},
		{name: "files_32/chunks_16", files: 32, chunks: 16},
		{name: "files_8/chunks_64", files: 8, chunks: 64},
	}

	for _, workload := range workloads {
		workload := workload
		chunkHashes := buildBenchmarkChunkHashes(workload.chunks)

		b.Run("batch_owner/"+workload.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				db := openRevRelBenchDB(b)
				if err := ingestBatchOwnerMetadataWorkload(db, workload.files, chunkHashes); err != nil {
					db.Close()
					b.Fatalf("batch_owner workload failed: %v", err)
				}
				if err := db.Close(); err != nil {
					b.Fatalf("close bench db: %v", err)
				}
			}
		})
	}
}

// BenchmarkIngestBatchOwnershipSteadyState removes per-iteration DB open/close
// overhead by keeping one DB open per sub-benchmark. Each iteration uses a
// unique file-id range so relation/reverse-relation writes continue to exercise
// merge/update behavior instead of degenerating into no-op duplicates.
func BenchmarkIngestBatchOwnershipSteadyState(b *testing.B) {
	ensureBenchmarkCodecs(b)

	workloads := []struct {
		name   string
		files  int
		chunks int
	}{
		{name: "files_8/chunks_16", files: 8, chunks: 16},
		{name: "files_32/chunks_16", files: 32, chunks: 16},
		{name: "files_8/chunks_64", files: 8, chunks: 64},
	}

	for _, workload := range workloads {
		workload := workload
		chunkHashes := buildBenchmarkChunkHashes(workload.chunks)

		b.Run("batch_owner_via_metadata_helper/"+workload.name, func(b *testing.B) {
			db := openRevRelBenchDB(b)
			defer db.Close()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ingestMetadataWorkloadWithOffset(db, workload.files, chunkHashes, i*workload.files); err != nil {
					b.Fatalf("shared_batch steady-state workload failed: %v", err)
				}
			}
		})

		b.Run("batch_owner/"+workload.name, func(b *testing.B) {
			db := openRevRelBenchDB(b)
			defer db.Close()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ingestBatchOwnerMetadataWorkloadWithOffset(db, workload.files, chunkHashes, i*workload.files); err != nil {
					b.Fatalf("batch_owner steady-state workload failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkIngestBatchOwnershipSimhashToggle measures the live batch-owner
// worker path with ENABLESIMHASH flipped on and off. This makes the simhash
// cost visible in the same ingest shape the production code uses.
func BenchmarkIngestBatchOwnershipSimhashToggle(b *testing.B) {
	ensureBenchmarkCodecs(b)

	workloads := []struct {
		name   string
		files  int
		chunks int
	}{
		{name: "files_8/chunks_16", files: 8, chunks: 16},
		{name: "files_32/chunks_16", files: 32, chunks: 16},
		{name: "files_8/chunks_64", files: 8, chunks: 64},
	}

	for _, workload := range workloads {
		workload := workload
		chunkData := buildBenchmarkChunkData(workload.chunks)
		mappedFile := concatBenchmarkChunkData(chunkData)

		b.Run("simhash_off/"+workload.name, func(b *testing.B) {
			db := openRevRelBenchDB(b)
			defer db.Close()

			oldEnableSimhash := cnst.ENABLESIMHASH
			cnst.ENABLESIMHASH = false
			defer func() {
				cnst.ENABLESIMHASH = oldEnableSimhash
			}()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ingestBatchOwnerWorkerWorkload(db, mappedFile, workload.files, i*workload.files, false); err != nil {
					b.Fatalf("batch-owner simhash-off workload failed: %v", err)
				}
			}
		})

		b.Run("simhash_on/"+workload.name, func(b *testing.B) {
			db := openRevRelBenchDB(b)
			defer db.Close()

			oldEnableSimhash := cnst.ENABLESIMHASH
			cnst.ENABLESIMHASH = true
			defer func() {
				cnst.ENABLESIMHASH = oldEnableSimhash
			}()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := ingestBatchOwnerWorkerWorkload(db, mappedFile, workload.files, i*workload.files, true); err != nil {
					b.Fatalf("batch-owner simhash-on workload failed: %v", err)
				}
			}
		})
	}
}

// ingestBatchOwnerMetadataWorkload drives batchOwnerWriteLoop directly,
// feeding pre-computed chunk hashes through the task channel so we measure
// the writer's DB throughput in isolation from hashing work.
// cdata is set to nil so batchOwnerProcessChonk skips the chunk existence
// probe and store, making this directly comparable to ingestMetadataWorkload
// which only exercises processRel + processRevRel.
func ingestBatchOwnerMetadataWorkload(db *badger.DB, fileCount int, chunkHashes [][]byte) error {
	return ingestBatchOwnerMetadataWorkloadWithOffset(db, fileCount, chunkHashes, 0)
}

func ingestBatchOwnerMetadataWorkloadWithOffset(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	baseTasks := make([]chunkTask, len(chunkHashes))
	for chunkIdx, chash := range chunkHashes {
		index := int64(chunkIdx) * cnst.ChonkSize
		baseTasks[chunkIdx] = chunkTask{index: index, chash: chash, cdata: nil}
	}

	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("ingest-file-%06d", fileOffset+fileIdx))

		if err := batchOwnerWriteLoopBenchmarkDirect(fhash, db, baseTasks); err != nil {
			return err
		}
	}
	return nil
}

// batchOwnerWriteLoopBenchmarkDirect mirrors batchOwnerWriteLoop for benchmark
// metadata-only tasks without channel/plumbing overhead, so the benchmark
// focuses on relation/reverse-relation write behavior.
func batchOwnerWriteLoopBenchmarkDirect(fhash []byte, db *badger.DB, tasks []chunkTask) error {
	batch, err := util.InitBatch(db)
	if err != nil {
		return err
	}

	var seenChunks map[string]struct{}
	revRelBuffer := newRevRelAppendBuffer(len(tasks))

	for _, task := range tasks {
		if len(task.cdata) != 0 {
			if seenChunks == nil {
				seenChunks = make(map[string]struct{}, len(tasks))
			}
			if err := batchOwnerProcessChonk(task.cdata, task.chash, db, batch, nil, nil, seenChunks); err != nil {
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
