package server

import (
	"cmp"
	"context"
	"slices"
	"sync"
)

// laneUnknown is the lane of a job not yet priced: its lane reads the
// database, which a pause may have closed, so dispatch prices it.
const laneUnknown int64 = -1

// ingestQueue admits finalized uploads to be stored, by the rules the
// evidence of a folder run by (internal/workpool, whose items are all known
// at its start):
//   - jobs that share a key run one after another, first come first: the
//     same client path would resume one ingest attempt from two jobs, and the
//     first of one content is stored while the later become its aliases;
//   - a heavy job runs alone, and no job after it starts while it waits, so
//     it is not starved;
//   - the jobs running hold their lanes within budget; a job that does not
//     fit waits at the head, for the same reason, and a job alone always runs.
//
// A waiting job whose key a job before it holds is passed over, so later
// jobs of other content may start. A paused queue admits nothing; a job a
// pause cut comes back in its place.
type ingestQueue struct {
	jobs   int
	budget int64
	// lane prices a job; it is called under mu, only while not paused.
	lane func(size int64) int64

	mu      sync.Mutex
	waiting []*queueJob // by seq
	running int
	heavy   bool  // a heavy job is running
	lanes   int64 // what the jobs running hold
	held    map[string]bool
	seq     uint64
	paused  bool
	active  map[*queueJob]struct{}
	// runs counts the jobs admitted and not yet left.
	runs tracker
}

// queueJob is one turn of an upload to store: the keys it may not share with
// a job running or before it, whether it is heavy, and, when not, its lane.
// A requeued upload takes a new turn, so a turn's fields are never rewritten
// once another goroutine may read them.
type queueJob struct {
	keys  []string
	heavy bool
	size  int64
	lane  int64
	seq   uint64
	start chan struct{} // closed once it is admitted
	// ctx is this turn's; a pause cuts it with cancel.
	ctx    context.Context
	cancel context.CancelCauseFunc
	// Under q.mu: left once it gave its turn back, cut once a pause cut it,
	// committing once its ingest began to commit, which no pause cuts.
	left, cut, committing bool
}

// slot is an admitted turn.
type slot struct {
	q *ingestQueue
	j *queueJob
}

func newIngestQueue(jobs int, budget int64, lane func(size int64) int64) *ingestQueue {
	return &ingestQueue{jobs: max(jobs, 1), budget: budget, lane: lane, held: map[string]bool{}, active: map[*queueJob]struct{}{}}
}

// acquire waits until j may run, and returns its slot, which must be
// released once it ends. queued, when not nil, is called before it waits,
// if it must, with the number of jobs waiting before it. It fails only when
// ctx ends first.
func (q *ingestQueue) acquire(ctx context.Context, j *queueJob, queued func(ahead int)) (slot, error) {
	j.ctx, j.cancel = context.WithCancelCause(ctx)
	j.start = make(chan struct{})
	q.mu.Lock()
	if j.seq == 0 {
		q.seq++
		j.seq = q.seq
	}
	q.insert(j)
	q.dispatch()
	return q.await(ctx, j, queued)
}

// await waits for j's admission. It is called with q.mu held, and unlocks it.
func (q *ingestQueue) await(ctx context.Context, j *queueJob, queued func(ahead int)) (slot, error) {
	ahead := slices.Index(q.waiting, j)
	admitted := q.isAdmitted(j)
	q.mu.Unlock()
	if !admitted && queued != nil {
		queued(ahead)
	}
	select {
	case <-j.start:
	case <-ctx.Done():
	}
	// Admitted as ctx ended, it gives its turn back rather than start work
	// that would be cancelled at once.
	if ctx.Err() == nil {
		return slot{q: q, j: j}, nil
	}
	j.cancel(nil)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.isAdmitted(j) {
		q.leave(j)
	} else if i := slices.Index(q.waiting, j); i >= 0 {
		q.waiting = slices.Delete(q.waiting, i, i+1)
	}
	q.dispatch()
	return slot{}, ctx.Err()
}

// ctx is the turn's context: a pause cuts it with errPaused.
func (sl slot) ctx() context.Context { return sl.j.ctx }

// release gives the turn back, unless a requeue did.
func (sl slot) release() {
	q := sl.q
	q.mu.Lock()
	if !sl.j.left {
		q.leave(sl.j)
		q.dispatch()
	}
	q.mu.Unlock()
	sl.j.cancel(nil)
}

// requeue gives the turn back and waits for a new one in the same place, so
// no job that came later with one of its keys runs first.
func (sl slot) requeue(ctx context.Context, queued func(ahead int)) (slot, error) {
	q, old := sl.q, sl.j
	old.cancel(nil)
	q.mu.Lock()
	if !old.left {
		q.leave(old)
	}
	j := &queueJob{keys: old.keys, heavy: old.heavy, size: old.size, lane: old.lane, seq: old.seq, start: make(chan struct{})}
	j.ctx, j.cancel = context.WithCancelCause(ctx)
	q.insert(j)
	q.dispatch()
	return q.await(ctx, j, queued)
}

// beforeFlush is the ingest's last point before it commits: a turn already
// cut stops there, and one not cut yet is spared from then on, so a pause
// never leaves an evidence half indexed.
func (sl slot) beforeFlush() error {
	sl.q.mu.Lock()
	defer sl.q.mu.Unlock()
	if sl.j.cut {
		return errPaused
	}
	sl.j.committing = true
	return nil
}

// pause stops admitting jobs; the jobs waiting keep their places.
func (q *ingestQueue) pause() {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
}

func (q *ingestQueue) resume() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.paused = false
	q.dispatch()
}

// cancelRunning cuts the jobs running that have not begun to commit, with
// cause, and returns how many it cut.
func (q *ingestQueue) cancelRunning(cause error) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for j := range q.active {
		if j.committing || j.cut {
			continue
		}
		j.cut = true
		j.cancel(cause)
		n++
	}
	return n
}

// counts are the jobs running and those waiting.
func (q *ingestQueue) counts() (running, waiting int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running, len(q.waiting)
}

// The methods below are called under q.mu.

// insert puts j among the waiting jobs by its seq.
func (q *ingestQueue) insert(j *queueJob) {
	i, _ := slices.BinarySearchFunc(q.waiting, j.seq, func(w *queueJob, seq uint64) int { return cmp.Compare(w.seq, seq) })
	q.waiting = slices.Insert(q.waiting, i, j)
}

func (q *ingestQueue) isAdmitted(j *queueJob) bool {
	select {
	case <-j.start:
		return true
	default:
		return false
	}
}

// dispatch admits the waiting jobs that may run now, oldest first.
func (q *ingestQueue) dispatch() {
	if q.paused || q.heavy {
		return
	}
	passed := map[string]bool{}
	rest := make([]*queueJob, 0, len(q.waiting))
	for i, j := range q.waiting {
		free := !q.blocked(j, passed)
		if free && !j.heavy && j.lane == laneUnknown {
			j.lane = q.lane(j.size)
		}
		switch {
		case j.heavy:
			next := i
			if q.running == 0 && free {
				q.admit(j)
				next++
			}
			q.waiting = append(rest, q.waiting[next:]...)
			return
		case !free:
			for _, k := range j.keys {
				passed[k] = true
			}
			rest = append(rest, j)
		case q.running >= q.jobs, q.running > 0 && q.lanes+j.lane > q.budget:
			q.waiting = append(rest, q.waiting[i:]...)
			return
		default:
			q.admit(j)
		}
	}
	q.waiting = rest
}

// blocked reports whether a job running, or one passed over before j,
// holds a key of j.
func (q *ingestQueue) blocked(j *queueJob, passed map[string]bool) bool {
	for _, k := range j.keys {
		if q.held[k] || passed[k] {
			return true
		}
	}
	return false
}

func (q *ingestQueue) admit(j *queueJob) {
	q.running++
	for _, k := range j.keys {
		q.held[k] = true
	}
	if j.heavy {
		q.heavy = true
	} else {
		q.lanes += j.lane
	}
	q.active[j] = struct{}{}
	q.runs.add()
	close(j.start)
}

// leave gives j's turn back, once.
func (q *ingestQueue) leave(j *queueJob) {
	if j.left {
		return
	}
	j.left = true
	q.running--
	for _, k := range j.keys {
		delete(q.held, k)
	}
	if j.heavy {
		q.heavy = false
	} else {
		q.lanes -= j.lane
	}
	delete(q.active, j)
	q.runs.done()
}
