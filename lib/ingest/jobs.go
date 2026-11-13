package ingest

import (
	"runtime"

	"indicer/lib/engine"
	"indicer/lib/store"

	"github.com/dgraph-io/badger/v4"
)

// MaxJobs bounds how many evidence run at once: past a few, they only
// compete for the disk.
const MaxJobs = 64

// HeavySize is the decoded size from which an evidence runs alone, with
// every worker: a large image keeps the machine busy by itself.
const HeavySize = 256 << 20

// IndexedPasses are the chunk passes an evidence being stored and indexed
// holds at once (LaneMemory).
const IndexedPasses = 2

const (
	// AutoJobs runs one evidence per cpusPerAutoJob CPUs, at most maxAutoJobs.
	cpusPerAutoJob = 4
	maxAutoJobs    = 4
	// minSharedWorkers is the narrowest chunk pass InnerWorkers shares out.
	minSharedWorkers = 2
)

// AutoJobs is how many evidence this machine runs at once when the number is
// left to DUES (0 jobs asked for).
func AutoJobs() int {
	return min(maxAutoJobs, max(1, runtime.NumCPU()/cpusPerAutoJob))
}

// InnerWorkers is the chunk-pass width of one evidence among jobs run at once
// in the database of e: its pipeline width shared out, or 0 (the configured
// width) when one runs at a time or the user chose the width.
func InnerWorkers(e *engine.Engine, jobs int) int {
	t := e.Tuning()
	if jobs <= 1 || t.StoreWorkersChosen {
		return 0
	}
	workers, _ := t.StorePipeline(e.Container())
	return max(minSharedWorkers, workers/jobs)
}

// LaneMemory is about the most one evidence of size decoded bytes holds among
// jobs run at once in db: passes chunk passes of its size, at InnerWorkers'
// width (store.PassMemory). A store holds its own pass and, while indexing,
// one for an object its index pass stores; a reindex holds only the latter.
func LaneMemory(db *badger.DB, size int64, jobs, passes int) int64 {
	return int64(passes) * store.PassMemory(db, size, InnerWorkers(engine.For(db), jobs))
}
