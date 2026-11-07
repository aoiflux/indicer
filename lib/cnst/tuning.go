package cnst

import (
	"runtime"

	"github.com/shirou/gopsutil/v3/mem"
)

// Tuning is how the storage engine runs on a database: how much of the
// machine it uses, how an ingest hashes and pipelines its chunks, and how
// wide the store and restore pipelines run. Unlike the layout, nothing of it
// is recorded with the data: each open takes the tuning its caller gives.
// The zero value is not DefaultTuning.
type Tuning struct {
	// LowResource is low-resource mode (--low): one worker, small caches and
	// write batches.
	LowResource bool
	// CompressLevel is the zstd level of chunks and records: fast, default
	// or best.
	CompressLevel string
	// HashStrategy is when an ingest hashes its evidence (SyncHashStrategy,
	// AsyncHashStrategy, HochoHashStrategy). It is kept as given: ingest
	// refuses a strategy it does not know.
	HashStrategy string
	// Pipeline is the chunk pass's pipeline (StorePipelineBatch or
	// StorePipelineStaged).
	Pipeline string
	// IOEngine and IOUringDepth say how container files are written. The
	// write backend is chosen once for the process.
	IOEngine     string
	IOUringDepth int
	// StoreWorkers and StoreQueue, above 0, replace the store pipeline's
	// width and queue depth for this machine.
	StoreWorkers, StoreQueue int
	// RestoreWorkers and RestoreQueue, above 0, replace restore's.
	RestoreWorkers, RestoreQueue int
	// RestoreBufferMB, above 0, is restore's write buffer in MiB (1 to 256).
	RestoreBufferMB int
}

// DefaultTuning is the tuning of a process given no flags.
func DefaultTuning() Tuning {
	return Tuning{
		CompressLevel: "best",
		HashStrategy:  SyncHashStrategy,
		Pipeline:      StorePipelineBatch,
		IOEngine:      StoreIOEngineAuto,
	}
}

// Globals is the tuning the process-wide settings describe, copied as they
// are, for opens whose caller gives none.
func Globals() Tuning {
	return Tuning{
		LowResource:     MEMOPT,
		CompressLevel:   CompressLevel,
		HashStrategy:    StoreHashStrategy,
		Pipeline:        StorePipelineMode,
		IOEngine:        StoreIOEngine,
		IOUringDepth:    StoreIOUringQueueDepth,
		StoreWorkers:    StoreWorkerCount,
		StoreQueue:      StoreTaskQueueDepth,
		RestoreWorkers:  RestoreWorkerCount,
		RestoreQueue:    RestoreTaskQueueDepth,
		RestoreBufferMB: RestoreWriteBufferMB,
	}
}

// MaxThreads is how many goroutines work that scales with the machine runs
// on: one in low-resource mode, otherwise two per CPU, since the work waits
// on file writes, the database and compression.
func (t Tuning) MaxThreads() int {
	if t.LowResource {
		return 1
	}
	return runtime.NumCPU() * 2
}

// StorePipeline returns the store pipeline's worker count and task queue
// depth: this machine's (DefaultStoreTuning), replaced by StoreWorkers and
// StoreQueue when those are set.
func (t Tuning) StorePipeline(containerMode bool) (workers, queueDepth int) {
	workers, queueDepth = DefaultStoreTuning(runtime.NumCPU(), t.LowResource, containerMode)
	if t.StoreWorkers > 0 {
		workers = t.StoreWorkers
	}
	if t.StoreQueue > 0 {
		queueDepth = t.StoreQueue
	}
	workers = max(workers, 1)
	return workers, max(queueDepth, workers)
}

// RestorePipeline returns restore's worker count and queue depth: this
// machine's (DefaultRestoreTuning), replaced by RestoreWorkers and
// RestoreQueue when those are set.
func (t Tuning) RestorePipeline() (workers, queueDepth int) {
	workers, queueDepth = DefaultRestoreTuning(runtime.NumCPU(), t.LowResource)
	if t.RestoreWorkers > 0 {
		workers = t.RestoreWorkers
	}
	if t.RestoreQueue > 0 {
		queueDepth = t.RestoreQueue
	}
	workers = max(workers, 1)
	return workers, max(queueDepth, workers)
}

// RestoreWriteBuffer is restore's write buffer in bytes.
func (t Tuning) RestoreWriteBuffer() int {
	switch {
	case t.RestoreBufferMB <= 0 && t.LowResource:
		return 32 * 1024 * 1024
	case t.RestoreBufferMB <= 0:
		return 256 * 1024 * 1024
	}
	return min(t.RestoreBufferMB, 256) * 1024 * 1024
}

// CacheLimit is the memory badger's caches and the container read cache may
// use: a quarter of available memory, from 64 MiB to 8 GiB, 256 MiB when
// memory cannot be probed, and 64 MiB in low-resource mode.
func (t Tuning) CacheLimit() int64 {
	if t.LowResource {
		return 64 * MB
	}
	vmemstat, err := mem.VirtualMemory()
	if err != nil {
		// Reliability first: a sane fallback when memory probing fails.
		return 256 * MB
	}
	return min(max(int64(vmemstat.Available/4), 64*MB), 8*GB)
}

// MaxBatchCount is how many transactions a write batch may have pending on
// a database of chunk-byte chunks: a quarter of available memory in chunks,
// from 1 (0 deadlocks badger) to MaxPendingBatchTxns, and 16 in low-resource
// mode or when memory or the chunk size is unknown.
func (t Tuning) MaxBatchCount(chunk int64) int {
	if t.LowResource {
		return 16
	}
	vmemstat, err := mem.VirtualMemory()
	if err != nil || chunk <= 0 {
		return 16
	}
	n := vmemstat.Available / 4 / uint64(chunk)
	return int(max(1, min(n, MaxPendingBatchTxns)))
}

// IOUringQueueDepth is the io-uring submission and completion queue depth:
// IOUringDepth, or for 0 sixteen per CPU, from 256 to 4096.
func (t Tuning) IOUringQueueDepth() int {
	if t.IOUringDepth > 0 {
		return t.IOUringDepth
	}
	return min(max(runtime.NumCPU()*16, 256), 4096)
}
