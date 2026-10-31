// Package index is the index pass of an ingest (W6). It walks an evidence's
// decoded image with scan.Scan and writes one partition record for every
// partition-table entry, whatever its filesystem, and one indexed record for
// every file, symlink and alternate data stream, keyed by deterministic IDs:
// indexing the same evidence again rewrites the same keys (I7).
//
// An indexed record says where its file's bytes are (D2, D10): extents of the
// evidence with holes, inline bytes, or a materialised object stored through
// the chunk pipeline under the record's own ID. Its hash is the plain hash of
// those bytes (ING-20).
package index

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"runtime"
	"sort"
	"strings"
	"sync"

	"indicer/internal/progress"
	"indicer/lib/dbmeta"
	"indicer/lib/parser/core"
	"indicer/lib/parser/scan"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// Options configure an index pass.
type Options struct {
	DB *badger.DB
	// EvidenceID is the raw ID of the evidence being indexed; its record
	// must exist.
	EvidenceID []byte
	// EvidenceName is the evidence's display name, which partition records
	// are named after.
	EvidenceName string
	// Image is the decoded evidence. The scan walks it, and file content is
	// hashed from it.
	Image core.Image
	// Workers hash file content; 0 picks one per CPU, at most 16.
	Workers int
	Limits  core.Limits
	// Entropy also computes each file's entropy from the bytes the pass
	// reads, for graph enrichment, so enrichment need not read them again.
	Entropy bool
}

// Partition is one partition record a pass wrote.
type Partition struct {
	ID      []byte
	Ordinal int
	Offset  int64
	Size    int64
	FSType  core.FSType
	Status  core.PartitionStatus
}

// Result is what an index pass covered.
type Result struct {
	// State is complete, or partial when a partition could not be walked
	// whole or a file's decoded content could not be stored.
	State      structs.IndexState
	Reason     string
	Coverage   structs.IndexCoverage
	Partitions []Partition
	Report     *core.ImageReport
}

// Run indexes the evidence. A source read error, a database error or a
// cancelled ctx stops the pass with an error, and the result covers what was
// done; the records written stay, and a later pass over the same evidence
// rewrites them. Run returns once nothing it started is still running (I6).
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.DB == nil || len(opts.EvidenceID) == 0 || opts.Image == nil {
		return Result{}, errors.New("index: a database, an evidence ID and an image are required")
	}
	ix := newIndexer(ctx, opts)
	rep, scanErr := scan.Scan(ix.ctx, opts.Image, ix.visitor(), scan.Options{Limits: opts.Limits})
	endErr := ix.stop()
	res := ix.result(rep)
	var err error
	switch {
	case ctx.Err() != nil:
		err = context.Cause(ctx)
	case ix.failure() != nil:
		// A worker's error is why the scan stopped.
		err = ix.failure()
	case scanErr != nil:
		err = scanErr
	default:
		err = endErr
	}
	if res.State == structs.IndexPartial {
		ix.phase.Warn(res.Reason)
	}
	ix.phase.Done(err)
	return res, err
}

// indexer is the state of one pass. The visitor callbacks run on the scan's
// goroutine; hash workers run beside it and only write records.
type indexer struct {
	opts    Options
	ctx     context.Context
	cancel  context.CancelCauseFunc
	newHash func() hash.Hash

	jobs    chan hashJob
	workers sync.WaitGroup

	mu  sync.Mutex
	err error

	// phase counts records written; read counts the bytes hashed.
	phase *progress.Phase
	read  *progress.Meter

	// Owned by the scan goroutine.
	part     *openPartition
	seen     int // records of the partition so far, for the progress item
	parts    []Partition
	cov      structs.IndexCoverage
	problems []string
}

func newIndexer(parent context.Context, opts Options) *indexer {
	ctx, cancel := context.WithCancelCause(parent)
	n := opts.Workers
	if n <= 0 {
		n = min(max(runtime.NumCPU(), 2), 16)
	}
	ix := &indexer{
		opts:    opts,
		ctx:     ctx,
		cancel:  cancel,
		newHash: dbmeta.FileHasher(opts.DB),
		jobs:    make(chan hashJob, 2*n),
		phase:   progress.Start(parent, "index", progress.Files, progress.Unknown),
	}
	ix.read = ix.phase.Meter("read", progress.Bytes)
	for range n {
		ix.workers.Add(1)
		go ix.hashWorker()
	}
	return ix
}

func (ix *indexer) visitor() scan.Visitor {
	return scan.Visitor{
		BeginPartition: ix.beginPartition,
		File:           ix.file,
		EndPartition:   ix.endPartition,
	}
}

// fail records the pass's first error and stops the scan.
func (ix *indexer) fail(err error) {
	ix.mu.Lock()
	if ix.err == nil {
		ix.err = err
	}
	ix.mu.Unlock()
	ix.cancel(err)
}

func (ix *indexer) failure() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.err
}

// stop waits for the hash workers. A partition the scan left open (it stopped
// part way) has its batch flushed, so a later delete finds its members.
func (ix *indexer) stop() error {
	var err error
	if p := ix.part; p != nil {
		p.pending.Wait()
		err = p.batch.Flush()
		ix.cov.FilesIndexed += p.indexed.Load()
		ix.part = nil
	}
	close(ix.jobs)
	ix.workers.Wait()
	ix.cancel(nil)
	return err
}

func (ix *indexer) result(rep *core.ImageReport) Result {
	res := Result{State: structs.IndexComplete, Coverage: ix.cov, Partitions: ix.parts, Report: rep}
	res.Coverage.Partitions = len(ix.parts)
	if len(ix.problems) > 0 {
		res.State = structs.IndexPartial
		res.Reason = summarise(ix.problems)
	}
	return res
}

// problem notes why the index is partial.
func (ix *indexer) problem(format string, args ...any) {
	ix.problems = append(ix.problems, fmt.Sprintf(format, args...))
}

// summarise joins the distinct problems, most frequent first, keeping the
// reason short however many files had one.
func summarise(problems []string) string {
	counts := map[string]int{}
	for _, p := range problems {
		counts[p]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	var parts []string
	for i, k := range keys {
		if i == 4 {
			parts = append(parts, fmt.Sprintf("%d more", len(keys)-i))
			break
		}
		if counts[k] > 1 {
			k = fmt.Sprintf("%s (x%d)", k, counts[k])
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, "; ")
}
