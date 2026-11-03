package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"

	"indicer/internal/lifecycle"
	"indicer/internal/workpool"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/evidence"

	"github.com/dgraph-io/badger/v4"
)

// maxStoreJobs bounds --store-jobs: past a few, evidence stored at once only
// compete for the disk.
const maxStoreJobs = 64

// heavyItem is the decoded size from which a folder's evidence is stored
// alone, with every worker: a large image keeps the machine busy by itself.
const heavyItem = 256 << 20

// StoreJobs turns --store-jobs into StoreOptions.Jobs: n, or for 0 a number
// for this machine. Low-resource mode stores one at a time.
func StoreJobs(n int) (int, error) {
	switch {
	case n < 0 || n > maxStoreJobs:
		return 0, lifecycle.Usagef("--%s must be from 0 to %d, got %d", cnst.FlagStoreJobs, maxStoreJobs, n)
	case cnst.MEMOPT:
		return 1, nil
	case n == 0:
		return min(4, max(1, runtime.NumCPU()/4)), nil
	}
	return n, nil
}

// folderJobs is how many evidence of a folder a store with Jobs n runs at
// once: one for 0, at most maxStoreJobs, one in low-resource mode.
func folderJobs(n int) int {
	if cnst.MEMOPT {
		return 1
	}
	return min(max(n, 1), maxStoreJobs)
}

// innerWorkers is the chunk-pass width of one evidence among jobs stored at
// once: the machine's pipeline width shared out, or 0 (the configured width)
// when one runs at a time or --store-workers set it.
func innerWorkers(jobs int) int {
	if jobs <= 1 || cnst.StoreWorkerCount > 0 {
		return 0
	}
	workers, _ := cnst.GetStorePipelineTuning(cnst.CONTAINERMODE)
	return max(2, workers/jobs)
}

// folderItems tells the scheduler which evidence of a folder may hold the
// same content, and which are large. Evidence of different decoded sizes
// cannot hold the same content; among those of one size, only evidence with
// equal fingerprints can (D1). Those are chained, so a parallel store never
// stores one content twice at once, and the first in folder order is stored
// while the others become its aliases, as in a store of one at a time. An
// evidence that cannot be opened chains with nothing: its own store fails, or
// decides alone. The opens run jobs at a time.
func folderItems(ctx context.Context, db *badger.DB, items []evidence.PlanItem, decode bool, jobs int) []workpool.Item {
	out := make([]workpool.Item, len(items))
	sizes := make([]int64, len(items))
	open := func(i int) (*evidence.Source, error) {
		return evidence.Open(ctx, items[i].Path, evidence.Options{Decode: decode})
	}
	_ = workpool.Run(ctx, jobs, make([]workpool.Item, len(items)), func(_ context.Context, i int) error {
		sizes[i] = -1
		if src, err := open(i); err == nil {
			sizes[i] = src.Size()
			src.Close()
		}
		return nil
	}, func(int, error) {})

	bySize := map[int64][]int{}
	for i, size := range sizes {
		out[i].Heavy = size >= heavyItem
		if size >= 0 {
			bySize[size] = append(bySize[size], i)
		}
	}
	var shared []int
	for _, group := range bySize {
		if len(group) > 1 {
			shared = append(shared, group...)
		}
	}
	keys := make([]string, len(shared))
	_ = workpool.Run(ctx, jobs, make([]workpool.Item, len(shared)), func(_ context.Context, k int) error {
		i := shared[k]
		src, err := open(i)
		if err != nil {
			return nil
		}
		defer src.Close()
		if fp, err := src.Fingerprint(dbmeta.FileHasher(db)); err == nil {
			keys[k] = fmt.Sprintf("%d:%s", sizes[i], fp)
		}
		return nil
	}, func(int, error) {})
	for k, i := range shared {
		out[i].Key = keys[k]
	}
	return out
}

// orderedOutput gives each evidence of a folder its own writer onto w, so
// their reports reach w in folder order however they finish: the first
// evidence not yet reported writes through, and the others are held until
// every one before them is reported. With one evidence at a time, every
// write goes straight through.
type orderedOutput struct {
	mu   sync.Mutex
	w    io.Writer
	head int
	held map[int]*bytes.Buffer
}

func newOrderedOutput(w io.Writer) *orderedOutput {
	return &orderedOutput{w: w, held: map[int]*bytes.Buffer{}}
}

// of is evidence i's writer.
func (o *orderedOutput) of(i int) io.Writer { return itemWriter{o, i} }

// reported moves on past evidence i, which must be the head: the next one
// writes through from now on, after what it wrote so far.
func (o *orderedOutput) reported(i int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.head = i + 1
	if b := o.held[o.head]; b != nil {
		_, _ = o.w.Write(b.Bytes())
		delete(o.held, o.head)
	}
}

// skipTo makes i the head: the evidence before it never ran.
func (o *orderedOutput) skipTo(i int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for ; o.head < i; o.head++ {
		delete(o.held, o.head)
	}
	if b := o.held[o.head]; b != nil {
		_, _ = o.w.Write(b.Bytes())
		delete(o.held, o.head)
	}
}

type itemWriter struct {
	o *orderedOutput
	i int
}

func (iw itemWriter) Write(p []byte) (int, error) {
	o := iw.o
	o.mu.Lock()
	defer o.mu.Unlock()
	if iw.i == o.head {
		return o.w.Write(p)
	}
	b := o.held[iw.i]
	if b == nil {
		b = new(bytes.Buffer)
		o.held[iw.i] = b
	}
	return b.Write(p)
}
