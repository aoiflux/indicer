package server

import (
	"context"
	"sync"
)

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
// jobs of other content may start.
type ingestQueue struct {
	jobs   int
	budget int64

	mu      sync.Mutex
	waiting []*queueJob // oldest first
	running int
	heavy   bool  // a heavy job is running
	lanes   int64 // what the jobs running hold
	held    map[string]bool
}

// queueJob is one upload to store: the keys it may not share with a job
// running or before it, whether it is heavy, and, when not, its lane.
type queueJob struct {
	keys  []string
	heavy bool
	lane  int64
	start chan struct{} // closed once it is admitted
}

func newIngestQueue(jobs int, budget int64) *ingestQueue {
	return &ingestQueue{jobs: max(jobs, 1), budget: budget, held: map[string]bool{}}
}

// acquire waits until j may run, and returns what to call once it ends.
// queued, when not nil, is called before it waits, if it must, with the
// number of jobs waiting before it. It fails only when ctx ends first.
func (q *ingestQueue) acquire(ctx context.Context, j *queueJob, queued func(ahead int)) (release func(), err error) {
	j.start = make(chan struct{})
	q.mu.Lock()
	q.waiting = append(q.waiting, j)
	q.dispatch()
	ahead := len(q.waiting) - 1
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
		return sync.OnceFunc(func() { q.release(j) }), nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.isAdmitted(j) {
		q.leave(j)
	} else {
		for i, w := range q.waiting {
			if w == j {
				q.waiting = append(q.waiting[:i], q.waiting[i+1:]...)
				break
			}
		}
	}
	q.dispatch()
	return nil, ctx.Err()
}

func (q *ingestQueue) release(j *queueJob) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.leave(j)
	q.dispatch()
}

// The methods below are called under q.mu.

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
	if q.heavy {
		return
	}
	passed := map[string]bool{}
	rest := make([]*queueJob, 0, len(q.waiting))
	for i, j := range q.waiting {
		free := !q.blocked(j, passed)
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
	close(j.start)
}

func (q *ingestQueue) leave(j *queueJob) {
	q.running--
	for _, k := range j.keys {
		delete(q.held, k)
	}
	if j.heavy {
		q.heavy = false
	} else {
		q.lanes -= j.lane
	}
}
