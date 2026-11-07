package cli

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"runtime"
	"slices"
	"sync"
	"time"

	"indicer/internal/appconfig"
	"indicer/internal/lifecycle"
	"indicer/internal/workpool"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/evidence"
	"indicer/lib/logging"
	"indicer/lib/store"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// maxStoreJobs bounds --store-jobs: past a few, evidence stored at once only
// compete for the disk.
const maxStoreJobs = 64

// heavyItem is the decoded size from which a folder's evidence is stored
// alone, with every worker: a large image keeps the machine busy by itself.
const heavyItem = 256 << 20

// StoreJobs turns --store-jobs into StoreOptions.Jobs: n, or for 0 a number
// for this machine. Low-resource mode (low) stores one at a time.
func StoreJobs(n int, low bool) (int, error) {
	switch {
	case n < 0 || n > maxStoreJobs:
		return 0, lifecycle.Usagef("--%s must be from 0 to %d, got %d", cnst.FlagStoreJobs, maxStoreJobs, n)
	case low:
		return 1, nil
	case n == 0:
		return min(4, max(1, runtime.NumCPU()/4)), nil
	}
	return n, nil
}

// folderJobs is how many evidence of a folder a store with Jobs n runs at
// once with tuning t: one for 0, at most maxStoreJobs, one in low-resource
// mode.
func folderJobs(t cnst.Tuning, n int) int {
	if t.LowResource {
		return 1
	}
	return min(max(n, 1), maxStoreJobs)
}

// innerWorkers is the chunk-pass width of one evidence among jobs stored at
// once in the database of e: its pipeline width shared out, or 0 (the
// configured width) when one runs at a time or --store-workers set it.
func innerWorkers(e *engine.Engine, jobs int) int {
	t := e.Tuning()
	if jobs <= 1 || t.StoreWorkers > 0 {
		return 0
	}
	workers, _ := t.StorePipeline(e.Container())
	return max(2, workers/jobs)
}

// folderItems tells the scheduler which evidence of a folder may hold the
// same content, and which are large. Evidence of different decoded sizes
// cannot hold the same content; among those of one size, only evidence with
// equal fingerprints can (D1). Those are chained, so a parallel store never
// stores one content twice at once, and the first in folder order is stored
// while the others become its aliases, as in a store of one at a time. An
// evidence that cannot be opened chains with nothing: its own store fails, or
// decides alone. The opens run jobs at a time. An evidence of heavy bytes or
// more decoded is heavy. sizes are the decoded sizes, -1 for an evidence
// that could not be opened.
func folderItems(ctx context.Context, db *badger.DB, items []evidence.PlanItem, decode bool, jobs int, heavy int64) (_ []workpool.Item, sizes []int64) {
	out := make([]workpool.Item, len(items))
	sizes = make([]int64, len(items))
	open := func(i int) (*evidence.Source, error) {
		return evidence.Open(ctx, items[i].Path, evidence.Options{Decode: decode})
	}
	_, _ = workpool.Run(ctx, jobs, make([]workpool.Item, len(items)), func(_ context.Context, i int) error {
		sizes[i] = -1
		if src, err := open(i); err == nil {
			sizes[i] = src.Size()
			src.Close()
		}
		return nil
	}, func(int, error) {})

	bySize := map[int64][]int{}
	for i, size := range sizes {
		out[i].Heavy = size >= heavy
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
	_, _ = workpool.Run(ctx, jobs, make([]workpool.Item, len(shared)), func(_ context.Context, k int) error {
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
	return out, sizes
}

// jobsBudget is the memory the evidence run at once may hold between them: a
// quarter of the memory free now, as for one store's workers and queue.
// Tests replace it.
var jobsBudget = func() int64 { return appconfig.MemoryBudget(appconfig.Machine()) }

// jobsInMemory is how many evidence may run at once, of jobs asked for, in
// jobsBudget. Each evidence beside others holds what passes chunk passes of
// its size hold (store.PassMemory): its own, and one for an object its index
// pass stores. A heavy one runs alone, as one store does, and is not counted. Jobs the user gave that do not fit are
// refused; jobs chosen for this machine (auto) come down until they fit.
func jobsInMemory(db *badger.DB, jobs int, auto bool, items []workpool.Item, sizes []int64, passes int) (int, error) {
	if jobs <= 1 {
		return jobs, nil
	}
	budget := jobsBudget()
	lane := func(i, jobs int) int64 {
		return int64(passes) * store.PassMemory(db, sizes[i], innerWorkers(engine.For(db), jobs))
	}
	fit, need := fitJobs(jobs, items, lane, budget)
	if fit == jobs {
		return jobs, nil
	}
	if !auto {
		return 0, lifecycle.Usagef("--%s %d: %d evidence at once with %d KiB chunks need %d MiB in flight, over the %d MiB budget; lower --%s",
			cnst.FlagStoreJobs, jobs, jobs, dbmeta.ChunkSize(db)>>10, need>>20, budget>>20, cnst.FlagStoreJobs)
	}
	logging.GetLogger().Info("Store jobs reduced to fit memory",
		zap.Int("jobs", jobs), zap.Int("fit", fit), zap.Int64("need_bytes", need), zap.Int64("budget_bytes", budget))
	return fit, nil
}

// fitJobs is the most jobs, from 1 up to jobs, whose worst case fits in
// budget: the j items that are not heavy and hold the most, lane(i, j)
// bytes each, running together. need is that worst case for jobs. One at a
// time always fits.
func fitJobs(jobs int, items []workpool.Item, lane func(i, jobs int) int64, budget int64) (fit int, need int64) {
	worst := func(j int) int64 {
		var lanes []int64
		for i, it := range items {
			if !it.Heavy {
				lanes = append(lanes, lane(i, j))
			}
		}
		slices.SortFunc(lanes, func(a, b int64) int { return cmp.Compare(b, a) })
		var sum int64
		for _, b := range lanes[:min(j, len(lanes))] {
			sum += b
		}
		return sum
	}
	need = worst(jobs)
	for fit = jobs; fit > 1; fit-- {
		if worst(fit) <= budget {
			break
		}
	}
	return fit, need
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
	// held is what each evidence after the head wrote, in order: text, and
	// what it asked to run once its report reaches w (atReport).
	held map[int][]func(io.Writer)
}

func newOrderedOutput(w io.Writer) *orderedOutput {
	return &orderedOutput{w: w, held: map[int][]func(io.Writer){}}
}

// of is evidence i's writer.
func (o *orderedOutput) of(i int) io.Writer { return itemWriter{o, i} }

// reported moves on past evidence i, which must be the head: the next one
// writes through from now on, after what it wrote so far.
func (o *orderedOutput) reported(i int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.head = i + 1
	o.flush(o.head)
}

// skipTo makes i the head: the evidence before it never ran.
func (o *orderedOutput) skipTo(i int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for ; o.head < i; o.head++ {
		delete(o.held, o.head)
	}
	o.flush(o.head)
}

// flush writes what evidence i held. It is called under o.mu.
func (o *orderedOutput) flush(i int) {
	for _, part := range o.held[i] {
		part(o.w)
	}
	delete(o.held, i)
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
	text := bytes.Clone(p)
	o.held[iw.i] = append(o.held[iw.i], func(w io.Writer) { _, _ = w.Write(text) })
	return len(p), nil
}

// later runs f once the evidence's report reaches w: at once for the head,
// otherwise when every evidence before it has been reported.
func (iw itemWriter) later(f func(io.Writer)) {
	o := iw.o
	o.mu.Lock()
	defer o.mu.Unlock()
	if iw.i == o.head {
		f(o.w)
		return
	}
	o.held[iw.i] = append(o.held[iw.i], f)
}

// atReport runs f once what was written to w so far reaches where w writes:
// at once, except for an evidence of a folder whose report is held, which
// runs it when every evidence before it has been reported, so that f sees
// what they did. f writes to the writer it is given.
func atReport(w io.Writer, f func(io.Writer)) {
	if l, ok := w.(interface{ later(func(io.Writer)) }); ok {
		l.later(f)
		return
	}
	f(w)
}

// poolFields are the log fields of a run of items that took took: how many,
// and how many a second; and, when the pool ran them (pool not nil), how long
// each worker was busy and held back with items left, and the share of the
// workers' time spent in items (workpool.Stats).
func poolFields(items int, took time.Duration, pool *workpool.Stats) []zap.Field {
	perSecond := 0.0
	if took > 0 {
		perSecond = float64(items) / took.Seconds()
	}
	fields := []zap.Field{zap.Int("items", items), zap.Float64("items_per_s", math.Round(perSecond*10)/10)}
	if pool == nil {
		return fields
	}
	ms := func(ds []time.Duration) []int64 {
		out := make([]int64, len(ds))
		for i, d := range ds {
			out[i] = d.Milliseconds()
		}
		return out
	}
	return append(fields, zap.Int64s("busy_ms", ms(pool.Busy)), zap.Int64s("held_ms", ms(pool.Held)),
		zap.Float64("utilization", math.Round(pool.Utilization()*100)/100))
}
