package store

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"testing"

	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/engine"
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

	defer console.Quiet()()

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
	_, err := ingestChunksBatchOwner(context.Background(), ChunkPass{
		DB: db, ID: fhash, Source: byteReaderAt(mappedFile), Size: int64(len(mappedFile)), Quiet: true,
		Simhash: enableSimhash,
	})
	return err
}

func ingestMetadataWorkload(db *badger.DB, fileCount int, chunkHashes [][]byte) error {
	return ingestMetadataWorkloadWithOffset(db, fileCount, chunkHashes, 0)
}

func ingestMetadataWorkloadWithOffset(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("ingest-file-%06d", fileOffset+fileIdx))
		batch := util.InitBatch(db, engine.For(db).MaxBatchCount())
		for chunkIdx, chash := range chunkHashes {
			index := int64(chunkIdx) * cnst.ChonkSize
			if err := processRel(index, fhash, chash, db, batch); err != nil {
				batch.Cancel()
				return err
			}
			if err := processRevRel(false, index, fhash, chash, db, batch); err != nil {
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
	return ingestProcessRevRelWorkloadMode(db, fileCount, chunkHashes, fileOffset)
}

func ingestProcessRevRelWorkloadMode(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	for fileIdx := 0; fileIdx < fileCount; fileIdx++ {
		fhash := []byte(fmt.Sprintf("ingest-rev-file-%06d", fileOffset+fileIdx))
		batch := db.NewWriteBatch()
		for chunkIdx, chash := range chunkHashes {
			index := int64(chunkIdx) * cnst.ChonkSize
			if err := processRevRel(true, index, fhash, chash, db, batch); err != nil {
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

		if err := dbio.SetReverseRelationAppendMembers(db, fhash, members, batch); err != nil {
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

// BenchmarkIngestBatchOwnership measures the metadata write throughput of the
// single-writer batch-owner path. (The shared-batch fan-out path it was once
// compared with is gone.)
//
// The workload is N files each with M chunks, where every file shares the
// same set of chunk hashes.
//
// The benchmark measures only the write-loop side (processRel + processRevRel
// issued directly, as in BenchmarkIngestMetadataPath), not hashing work.
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
// worker path with signatures on and off. This makes the simhash
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
// No chunk is stored, making this directly comparable to
// ingestMetadataWorkload which only exercises processRel + processRevRel.
func ingestBatchOwnerMetadataWorkload(db *badger.DB, fileCount int, chunkHashes [][]byte) error {
	return ingestBatchOwnerMetadataWorkloadWithOffset(db, fileCount, chunkHashes, 0)
}

func ingestBatchOwnerMetadataWorkloadWithOffset(db *badger.DB, fileCount int, chunkHashes [][]byte, fileOffset int) error {
	baseTasks := make([]chunkTask, len(chunkHashes))
	for chunkIdx, chash := range chunkHashes {
		index := int64(chunkIdx) * cnst.ChonkSize
		baseTasks[chunkIdx] = chunkTask{index: index, chash: chash}
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
	batch := util.InitBatch(db, engine.For(db).MaxBatchCount())

	for _, task := range tasks {
		if err := processRel(task.index, fhash, task.chash, db, batch); err != nil {
			batch.Cancel()
			return err
		}
		if err := processRevRel(false, task.index, fhash, task.chash, db, batch); err != nil {
			batch.Cancel()
			return err
		}
	}

	return batch.Flush()
}
