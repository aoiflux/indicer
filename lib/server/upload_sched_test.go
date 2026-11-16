package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"indicer/lib/engine"
	"indicer/lib/evidence"
	"indicer/lib/ingest"
	"indicer/lib/store"
)

// queueWait bounds every wait of these tests: past it, a job that should
// have started never will.
const queueWait = 10 * time.Second

// arrival is a job handed to a queue, as an upload's finalize hands it.
type arrival struct {
	j       *queueJob
	ahead   int // the jobs waiting before it when it arrived; -1 if admitted
	release chan func()
	err     chan error
	ended   bool
}

// arrive hands j to q and returns once j is admitted or waiting, so jobs
// arrive in the order a test calls arrive.
func arrive(ctx context.Context, t *testing.T, q *ingestQueue, j *queueJob) *arrival {
	t.Helper()
	a := &arrival{j: j, ahead: -1, release: make(chan func(), 1), err: make(chan error, 1)}
	waiting := make(chan int, 1)
	go func() {
		sl, err := q.acquire(ctx, j, func(ahead int) { waiting <- ahead })
		if err != nil {
			a.err <- err
			return
		}
		a.release <- sl.release
	}()
	select {
	case release := <-a.release:
		a.release <- release
	case a.ahead = <-waiting:
	case <-time.After(queueWait):
		t.Fatal("a job was neither admitted nor queued")
	}
	return a
}

// started reports whether a's job was admitted. A release admits the jobs
// it lets run before it returns, so this needs no waiting.
func (a *arrival) started() bool {
	select {
	case <-a.j.start:
		return true
	default:
		return false
	}
}

// done ends a's job, as its upload's ingest returning does.
func (a *arrival) done(t *testing.T) {
	t.Helper()
	select {
	case release := <-a.release:
		release()
		a.ended = true
	case <-time.After(queueWait):
		t.Fatal("a job never started")
	}
}

func job(keys ...string) *queueJob { return &queueJob{keys: keys} }

// wantStarted checks that of the jobs not yet ended, those named in want
// run, and only those.
func wantStarted(t *testing.T, what string, as map[string]*arrival, want ...string) {
	t.Helper()
	for name, a := range as {
		if a.ended {
			continue
		}
		if got, w := a.started(), slices.Contains(want, name); got != w {
			t.Errorf("%s: job %s started %v, want %v", what, name, got, w)
		}
	}
}

// TestQueueKeys: jobs of one key run one after another, in the order they
// came; a job waiting on its key is passed over by later jobs of other keys.
func TestQueueKeys(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(4, 1<<40, nil)
	as := map[string]*arrival{}
	as["a"] = arrive(ctx, t, q, job("path:/a", "content:1"))
	as["b"] = arrive(ctx, t, q, job("path:/b", "content:1")) // same content
	as["c"] = arrive(ctx, t, q, job("path:/a", "content:2")) // same path
	as["d"] = arrive(ctx, t, q, job("path:/d", "content:3"))
	as["e"] = arrive(ctx, t, q, job("path:/e", "content:1")) // after b
	wantStarted(t, "first", as, "a", "d")
	if as["b"].ahead != 0 || as["e"].ahead != 2 {
		t.Errorf("waiting ahead of b %d and e %d, want 0 and 2", as["b"].ahead, as["e"].ahead)
	}
	as["a"].done(t)
	wantStarted(t, "after a", as, "b", "c", "d")
	as["b"].done(t)
	wantStarted(t, "after b", as, "c", "d", "e")
}

// TestQueueJobs: no more than jobs run at once, and the oldest waiting
// starts first.
func TestQueueJobs(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(2, 1<<40, nil)
	as := map[string]*arrival{}
	for _, name := range []string{"a", "b", "c", "d"} {
		as[name] = arrive(ctx, t, q, job("path:/"+name))
	}
	wantStarted(t, "first", as, "a", "b")
	as["b"].done(t)
	wantStarted(t, "after b", as, "a", "c")
	as["a"].done(t)
	wantStarted(t, "after a", as, "c", "d")
}

// TestQueueHeavy: a heavy job waits for the jobs running, nothing after it
// starts while it waits or runs, and once it ends the others run.
func TestQueueHeavy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(4, 1<<40, nil)
	as := map[string]*arrival{}
	as["a"] = arrive(ctx, t, q, job("path:/a"))
	heavy := job("path:/h")
	heavy.heavy = true
	as["h"] = arrive(ctx, t, q, heavy)
	as["b"] = arrive(ctx, t, q, job("path:/b"))
	wantStarted(t, "first", as, "a")
	as["a"].done(t)
	wantStarted(t, "after a", as, "h")
	as["h"].done(t)
	wantStarted(t, "after h", as, "b")

	alone := newIngestQueue(4, 1<<40, nil)
	first := job("path:/x")
	first.heavy = true
	if a := arrive(ctx, t, alone, first); !a.started() {
		t.Error("a heavy job on an idle queue waits")
	}
}

// TestQueueBudget: jobs run while their lanes fit the budget; one that does
// not fit waits at the head, and a job alone always runs.
func TestQueueBudget(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(4, 100, nil)
	lane := func(name string, bytes int64) *queueJob { j := job("path:/" + name); j.lane = bytes; return j }
	as := map[string]*arrival{}
	as["a"] = arrive(ctx, t, q, lane("a", 60))
	as["b"] = arrive(ctx, t, q, lane("b", 60))
	as["c"] = arrive(ctx, t, q, lane("c", 10))
	wantStarted(t, "first", as, "a")
	as["a"].done(t)
	wantStarted(t, "after a", as, "b", "c")

	idle := newIngestQueue(4, 100, nil)
	if a := arrive(ctx, t, idle, lane("big", 1000)); !a.started() {
		t.Error("a job over the budget waits on an idle queue")
	}
}

// TestQueueCancel: a job whose context ends while it waits leaves the
// queue, and holds back nothing after it; a release twice is harmless.
func TestQueueCancel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(1, 1<<40, nil)
	as := map[string]*arrival{}
	as["a"] = arrive(ctx, t, q, job("path:/a"))
	gone, cancel := context.WithCancel(ctx)
	as["b"] = arrive(gone, t, q, job("path:/b", "content:1"))
	as["c"] = arrive(ctx, t, q, job("path:/c", "content:1"))
	cancel()
	select {
	case err := <-as["b"].err:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled job: %v", err)
		}
	case <-time.After(queueWait):
		t.Fatal("a cancelled job still waits")
	}
	delete(as, "b")
	release := <-as["a"].release
	release()
	release()
	as["a"].ended = true
	wantStarted(t, "after a", as, "c")
	q.mu.Lock()
	running := q.running
	q.mu.Unlock()
	if running != 1 {
		t.Errorf("%d running after a double release, want 1", running)
	}
}

// TestQueueAtOnce: many jobs of a few keys, some heavy, arriving and ending
// at once, never break the queue's rules, and every one runs.
func TestQueueAtOnce(t *testing.T) {
	t.Parallel()
	const jobs, n, keys = 3, 200, 5
	q := newIngestQueue(jobs, 1<<40, nil)
	var (
		mu      sync.Mutex
		running int
		heavy   bool
		held    = map[string]bool{}
		broken  []string
	)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			j := job(fmt.Sprintf("path:/%d", i%keys))
			j.heavy = i%17 == 0
			sl, err := q.acquire(t.Context(), j, nil)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			running++
			switch {
			case running > jobs:
				broken = append(broken, fmt.Sprintf("%d running", running))
			case heavy || j.heavy && running > 1:
				broken = append(broken, "a heavy job beside another")
			case held[j.keys[0]]:
				broken = append(broken, "two jobs of "+j.keys[0])
			}
			heavy = heavy || j.heavy
			held[j.keys[0]] = true
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			running--
			heavy = heavy && !j.heavy
			delete(held, j.keys[0])
			mu.Unlock()
			sl.release()
		})
	}
	wg.Wait()
	if len(broken) > 0 {
		t.Fatalf("rules broken: %v", broken)
	}
	if q.running != 0 || len(q.waiting) != 0 || len(q.held) != 0 || q.lanes != 0 {
		t.Fatalf("queue not empty: %d running, %d waiting, %d keys, %d bytes", q.running, len(q.waiting), len(q.held), q.lanes)
	}
}

// turn is a job handed to a queue whose slot the test keeps, to cut,
// requeue or release it.
type turn struct {
	slot  chan slot
	ahead chan int
	err   chan error
}

func newTurn() *turn {
	return &turn{slot: make(chan slot, 1), ahead: make(chan int, 1), err: make(chan error, 1)}
}

func (tn *turn) queued(ahead int) { tn.ahead <- ahead }

func (tn *turn) end(sl slot, err error) {
	if err != nil {
		tn.err <- err
		return
	}
	tn.slot <- sl
}

// take hands j to q from its own goroutine.
func take(ctx context.Context, q *ingestQueue, j *queueJob) *turn {
	tn := newTurn()
	go func() { tn.end(q.acquire(ctx, j, tn.queued)) }()
	return tn
}

// requeueTurn requeues sl from its own goroutine.
func requeueTurn(ctx context.Context, sl slot) *turn {
	tn := newTurn()
	go func() { tn.end(sl.requeue(ctx, tn.queued)) }()
	return tn
}

// admitted waits for the turn's slot.
func (tn *turn) admitted(t *testing.T) slot {
	t.Helper()
	select {
	case sl := <-tn.slot:
		return sl
	case err := <-tn.err:
		t.Fatalf("a job gave up its turn: %v", err)
	case <-time.After(queueWait):
		t.Fatal("a job was never admitted")
	}
	return slot{}
}

// waiting waits until the turn's job is queued, and returns how many
// waited before it.
func (tn *turn) waiting(t *testing.T) int {
	t.Helper()
	select {
	case ahead := <-tn.ahead:
		return ahead
	case <-time.After(queueWait):
		t.Fatal("a job was never queued")
	}
	return 0
}

// seqs are the seqs of the jobs running, sorted, and of the jobs waiting,
// in their order.
func seqs(q *ingestQueue) (running, waiting []uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for j := range q.active {
		running = append(running, j.seq)
	}
	slices.Sort(running)
	for _, j := range q.waiting {
		waiting = append(waiting, j.seq)
	}
	return running, waiting
}

// wantSettled checks that a queue whose turns all ended holds nothing.
func wantSettled(t *testing.T, q *ingestQueue) {
	t.Helper()
	q.mu.Lock()
	running, waiting, heavy, lanes, held, active := q.running, len(q.waiting), q.heavy, q.lanes, len(q.held), len(q.active)
	q.mu.Unlock()
	if running != 0 || waiting != 0 || heavy || lanes != 0 || held != 0 || active != 0 {
		t.Errorf("queue not settled: %d running, %d waiting, heavy %v, %d bytes, %d keys, %d active", running, waiting, heavy, lanes, held, active)
	}
	if n := q.runs.count(); n != 0 {
		t.Errorf("%d turns counted as running, want 0", n)
	}
}

// TestQueuePauseHoldsAdmission: a paused queue admits no job, neither one
// arriving nor one a release would let run; resumed, it admits them in the
// order they came.
func TestQueuePauseHoldsAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(2, 1<<40, nil)
	q.pause()
	as := map[string]*arrival{}
	for _, name := range []string{"a", "b", "c"} {
		as[name] = arrive(ctx, t, q, job("path:/"+name))
	}
	wantStarted(t, "paused", as)
	if as["a"].ahead != 0 || as["b"].ahead != 1 || as["c"].ahead != 2 {
		t.Errorf("waiting ahead of a, b and c: %d, %d and %d; want 0, 1 and 2", as["a"].ahead, as["b"].ahead, as["c"].ahead)
	}
	q.resume()
	wantStarted(t, "resumed", as, "a", "b")
	q.pause()
	as["a"].done(t)
	wantStarted(t, "a released while paused", as, "b")
	q.resume()
	wantStarted(t, "resumed again", as, "b", "c")
}

// TestQueueRequeueKeepsPlace: a job a pause cut takes its turn again ahead
// of the jobs that came after it.
func TestQueueRequeueKeepsPlace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(1, 1<<40, nil)
	a := take(ctx, q, job("path:/a")).admitted(t)
	as := map[string]*arrival{}
	as["b"] = arrive(ctx, t, q, job("path:/b"))
	as["c"] = arrive(ctx, t, q, job("path:/c"))
	q.pause()
	if n := q.cancelRunning(errPaused); n != 1 {
		t.Errorf("%d jobs cut, want 1", n)
	}
	if cause := context.Cause(a.ctx()); !errors.Is(cause, errPaused) {
		t.Errorf("the cut job's cause: %v, want %v", cause, errPaused)
	}
	again := requeueTurn(ctx, a)
	if ahead := again.waiting(t); ahead != 0 {
		t.Errorf("%d jobs waiting ahead of the requeued job, want 0", ahead)
	}
	running, waiting := seqs(q)
	if want := []uint64{a.j.seq, as["b"].j.seq, as["c"].j.seq}; len(running) != 0 || !slices.Equal(waiting, want) {
		t.Errorf("running %v, waiting %v; want none running and %v waiting", running, waiting, want)
	}
	q.resume()
	a2 := again.admitted(t)
	if a2.j.seq != a.j.seq || a2.j == a.j {
		t.Errorf("the requeued turn: seq %d, same job %v; want seq %d in a new job", a2.j.seq, a2.j == a.j, a.j.seq)
	}
	wantStarted(t, "requeued job admitted", as)
	a2.release()
	wantStarted(t, "after a", as, "b")
	as["b"].done(t)
	wantStarted(t, "after b", as, "c")
	as["c"].done(t)
	wantSettled(t, q)
}

// TestQueueCancelRunning: a pause cuts only the jobs running that have not
// begun to commit; a cut job's flush is vetoed, and one that began to commit
// is never cut.
func TestQueueCancelRunning(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(2, 1<<40, nil)
	a := take(ctx, q, job("path:/a")).admitted(t)
	b := take(ctx, q, job("path:/b")).admitted(t)
	c := arrive(ctx, t, q, job("path:/c"))
	if err := b.beforeFlush(); err != nil {
		t.Fatalf("flush before any cut: %v", err)
	}
	q.pause()
	if n := q.cancelRunning(errPaused); n != 1 {
		t.Errorf("%d jobs cut, want 1", n)
	}
	if cause := context.Cause(a.ctx()); !errors.Is(cause, errPaused) {
		t.Errorf("the running job's cause: %v, want %v", cause, errPaused)
	}
	if err := b.ctx().Err(); err != nil {
		t.Errorf("the committing job was cut: %v", err)
	}
	if err := c.j.ctx.Err(); err != nil {
		t.Errorf("the waiting job was cut: %v", err)
	}
	if err := a.beforeFlush(); !errors.Is(err, errPaused) {
		t.Errorf("flush of a cut job: %v, want %v", err, errPaused)
	}
	if n := q.cancelRunning(errPaused); n != 0 {
		t.Errorf("%d jobs cut again, want 0", n)
	}
	q.resume()
	a.release()
	b.release()
	if !c.started() {
		t.Error("the waiting job did not start once the others ended")
	}
	c.done(t)
	wantSettled(t, q)
}

// TestQueueLaneAtAdmission: a job's lane is priced once, only once it may
// run and never while the queue is paused; a job priced already, or heavy,
// is not priced.
func TestQueueLaneAtAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const perByte = 10
	var mu sync.Mutex
	var priced []int64
	q := newIngestQueue(1, 1<<40, func(size int64) int64 {
		mu.Lock()
		priced = append(priced, size)
		mu.Unlock()
		return size * perByte
	})
	wantPriced := func(what string, want ...int64) {
		t.Helper()
		mu.Lock()
		got := slices.Clone(priced)
		mu.Unlock()
		if !slices.Equal(got, want) {
			t.Errorf("%s: priced sizes %v, want %v", what, got, want)
		}
	}
	unpriced := func(name string, size int64) *queueJob {
		j := job("path:/" + name)
		j.size, j.lane = size, laneUnknown
		return j
	}
	as := map[string]*arrival{}
	q.pause()
	as["x"] = arrive(ctx, t, q, unpriced("x", 1))
	wantPriced("paused")
	q.resume()
	wantPriced("resumed", 1)
	as["y"] = arrive(ctx, t, q, unpriced("y", 2))
	known := job("path:/z")
	known.size, known.lane = 3, 7
	as["z"] = arrive(ctx, t, q, known)
	heavy := job("path:/h")
	heavy.size, heavy.heavy = 4, true
	as["h"] = arrive(ctx, t, q, heavy)
	for _, name := range []string{"x", "y", "z", "h"} {
		as[name].done(t)
	}
	wantPriced("all done", 1, 2)
	q.mu.Lock()
	lanes := []int64{as["x"].j.lane, as["y"].j.lane, as["z"].j.lane, as["h"].j.lane}
	q.mu.Unlock()
	if want := []int64{1 * perByte, 2 * perByte, 7, 0}; !slices.Equal(lanes, want) {
		t.Errorf("lanes %v, want %v", lanes, want)
	}
	wantSettled(t, q)
}

// TestQueueTurnsSettle: a turn given back by a requeue is not given back
// again by its release, and once every turn ends, light or heavy, requeued
// or not, the queue holds nothing.
func TestQueueTurnsSettle(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(2, 1<<40, nil)
	lane := func(name string, bytes int64) *queueJob { j := job("path:/" + name); j.lane = bytes; return j }
	a := take(ctx, q, lane("a", 10)).admitted(t)
	b := take(ctx, q, lane("b", 20)).admitted(t)
	q.pause()
	if n := q.cancelRunning(errPaused); n != 2 {
		t.Errorf("%d jobs cut, want 2", n)
	}
	again := requeueTurn(ctx, a)
	again.waiting(t)
	a.release()
	b.release()
	if running, waiting := seqs(q); len(running) != 0 || len(waiting) != 1 {
		t.Errorf("after the requeue: running %v, waiting %v; want none and 1", running, waiting)
	}
	q.resume()
	a2 := again.admitted(t)
	a.release()
	q.mu.Lock()
	running, lanes := q.running, q.lanes
	q.mu.Unlock()
	if running != 1 || lanes != 10 {
		t.Errorf("a stale release: %d running holding %d bytes, want 1 holding 10", running, lanes)
	}
	a2.release()

	heavy := job("path:/h")
	heavy.heavy = true
	h := take(ctx, q, heavy).admitted(t)
	q.pause()
	if n := q.cancelRunning(errPaused); n != 1 {
		t.Errorf("%d heavy jobs cut, want 1", n)
	}
	hAgain := requeueTurn(ctx, h)
	hAgain.waiting(t)
	h.release()
	q.resume()
	h2 := hAgain.admitted(t)
	q.mu.Lock()
	isHeavy := q.heavy
	q.mu.Unlock()
	if !isHeavy {
		t.Error("the requeued heavy job runs as light")
	}
	h.release()
	h2.release()
	wantSettled(t, q)
}

// finalizeAt finalizes the upload id from its own goroutine, as a client
// does, and sends what the server answered.
func finalizeAt(ls *liveServer, id string, out chan<- reply) {
	go func() {
		b, _ := json.Marshal(map[string]string{"upload_id": id})
		resp, err := http.Post(ls.url+"/web/upload/finalize", "application/json", bytes.NewReader(b))
		if err != nil {
			out <- reply{body: map[string]any{"error": err.Error()}}
			return
		}
		defer resp.Body.Close()
		r := reply{status: resp.StatusCode}
		_ = json.NewDecoder(resp.Body).Decode(&r.body)
		out <- r
	}()
}

// received starts an upload of data at path and sends all of it.
func received(t *testing.T, ls *liveServer, path string, data []byte) string {
	t.Helper()
	id := start(t, ls, path, "", plainHash(ls.db(), data), int64(len(data))).str("upload_id")
	if r := chunk(t, ls, id, 0, data); r.status != http.StatusOK {
		t.Fatalf("chunk of %s: %d %v", path, r.status, r.body)
	}
	return id
}

// statusUntil asks for the status of upload id until ok accepts it.
func statusUntil(t *testing.T, ls *liveServer, id string, what string, ok func(reply) bool) reply {
	t.Helper()
	deadline := time.Now().Add(queueWait)
	for {
		st := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": id})
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload %s never %s: %v", id, what, st.body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func queued(r reply) bool { q, _ := r.body["queued"].(bool); return q }

// TestParallelUploads: with four upload jobs, four uploads of different
// content are stored at once, each with a share of the store workers.
func TestParallelUploads(t *testing.T) {
	t.Parallel()
	const jobs = 4
	lim := DefaultUploadLimits()
	lim.Jobs = jobs
	var inside atomic.Int32
	together := make(chan struct{})
	var mu sync.Mutex
	var workers []int
	ls := uploadServer(t, &lim, func(m *uploads) {
		next := m.runIngest
		m.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			if inside.Add(1) == jobs {
				close(together)
			}
			select {
			case <-together:
			case <-time.After(queueWait):
			}
			mu.Lock()
			workers = append(workers, req.MaxWorkers)
			mu.Unlock()
			return next(ctx, req)
		}
	})
	db := ls.db()
	answers := make(chan reply, jobs)
	for i := range jobs {
		data := bytes.Repeat(fmt.Appendf(nil, "upload %d ", i), 10_000)
		finalizeAt(ls, received(t, ls, fmt.Sprintf("/case/%d.bin", i), data), answers)
	}
	for range jobs {
		if r := <-answers; r.status != http.StatusOK || r.str("outcome") != "stored" {
			t.Errorf("finalize: %d %v", r.status, r.body)
		}
	}
	select {
	case <-together:
	default:
		t.Fatalf("the %d uploads were never stored at once", jobs)
	}
	want := ingest.InnerWorkers(engine.For(db), jobs)
	for _, w := range workers {
		if w != want {
			t.Errorf("an upload beside %d others stored with %d workers, want %d", jobs-1, w, want)
		}
	}
	if n := evidenceCount(t, db); n != jobs {
		t.Errorf("%d evidence records, want %d", n, jobs)
	}
}

// TestUploadQueued: an upload finalized while another is stored, with one
// upload job, waits its turn, and its status says so; it is stored once
// the first is.
func TestUploadQueued(t *testing.T) {
	t.Parallel()
	lim := DefaultUploadLimits()
	lim.FinalizeWait = 100 * time.Millisecond
	hold, holding := make(chan struct{}), make(chan struct{})
	ls := uploadServer(t, &lim, func(m *uploads) {
		next := m.runIngest
		m.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			if req.SourcePath == "/case/first.bin" {
				close(holding)
				<-hold
			}
			return next(ctx, req)
		}
	})
	unhold := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(unhold)
	answers := make(chan reply, 2)
	first := received(t, ls, "/case/first.bin", []byte("stored first"))
	finalizeAt(ls, first, answers)
	select {
	case <-holding:
	case <-time.After(queueWait):
		t.Fatal("the first upload was never stored")
	}
	second := received(t, ls, "/case/second.bin", []byte("stored second"))
	finalizeAt(ls, second, answers)
	st := statusUntil(t, ls, second, "queued", queued)
	if st.str("state") != stateFinalizing {
		t.Errorf("a queued upload is %q, want %q", st.str("state"), stateFinalizing)
	}
	if st := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": first}); queued(st) {
		t.Errorf("the upload being stored says it is queued: %v", st.body)
	}
	unhold()
	for _, id := range []string{first, second} {
		st := statusUntil(t, ls, id, "done", func(r reply) bool { return r.str("state") == stateDone })
		if _, ok := st.body["queued"]; ok {
			t.Errorf("a done upload has queued: %v", st.body)
		}
	}
	for range 2 {
		<-answers
	}
}

// TestUploadQueuedAtStop: an upload still waiting when the server stops
// fails with 503 and leaves no part file, the stop is clean, and the
// server's ingest session and database are closed.
func TestUploadQueuedAtStop(t *testing.T) {
	t.Parallel()
	holding := make(chan struct{})
	ls := uploadServer(t, nil, func(m *uploads) {
		m.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			close(holding)
			<-ctx.Done()
			return ingest.Result{}, ctx.Err()
		}
	})
	answers := make(chan reply, 2)
	finalizeAt(ls, received(t, ls, "/case/first.bin", []byte("stored first")), answers)
	select {
	case <-holding:
	case <-time.After(queueWait):
		t.Fatal("the first upload was never stored")
	}
	second := received(t, ls, "/case/second.bin", []byte("stored second"))
	finalizeAt(ls, second, answers)
	statusUntil(t, ls, second, "queued", queued)
	sess := ls.dbs.current().sess
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	m := ls.uploads
	m.mu.Lock()
	s := m.sessions[second]
	state, uerr, isQueued := s.state, s.err, s.queued
	m.mu.Unlock()
	if state != stateFailed || uerr == nil || uerr.status != http.StatusServiceUnavailable || isQueued {
		t.Errorf("the queued upload at stop: %s, %v, queued %v; want failed with 503", state, uerr, isQueued)
	}
	if parts := partFiles(t, ls); len(parts) != 0 {
		t.Errorf("part files left: %v", parts)
	}
	if _, err := sess.Run(t.Context(), ingest.Request{Path: "unused"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the ingest session after a clean stop: %v, want it closed", err)
	}
	if ls.dbs.h.DB != nil {
		t.Error("the database is still open after a clean stop")
	}
	wantReopens(t, ls.root)
}

// TestUploadStuckAtStop: an ingest that ignores the stop makes it unclean,
// and the stop returns rather than wait on the ingest session for it. The
// session and the database are left open.
func TestUploadStuckAtStop(t *testing.T) {
	t.Parallel()
	holding, hold := make(chan struct{}), make(chan struct{})
	ls := uploadServer(t, nil, func(m *uploads) {
		m.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			close(holding)
			<-hold
			return ingest.Result{}, ctx.Err()
		}
	})
	t.Cleanup(func() { close(hold) })
	answers := make(chan reply, 1)
	finalizeAt(ls, received(t, ls, "/case/stuck.bin", []byte("never stored")), answers)
	select {
	case <-holding:
	case <-time.After(queueWait):
		t.Fatal("the upload was never stored")
	}
	sess := ls.dbs.current().sess
	if err := ls.stop(t); !errors.Is(err, ErrUncleanShutdown) {
		t.Fatalf("stop: %v, want %v", err, ErrUncleanShutdown)
	}
	// Left open: closing it would have waited on the stuck ingest.
	missing := ingest.Request{Path: filepath.Join(t.TempDir(), "missing")}
	if _, err := sess.Run(t.Context(), missing); err == nil || strings.Contains(err.Error(), "closed") {
		t.Errorf("the ingest session after an unclean stop: %v, want it open", err)
	}
	if ls.dbs.h.DB == nil || ls.dbs.h.DB.IsClosed() {
		t.Error("the database was closed after an unclean stop")
	}
}

// TestUploadsOfOneKeyWait: with room for more, an upload of the client path
// or the content of one being stored waits for it. The content's later
// upload is then its alias, and the path's is stored on its own.
func TestUploadsOfOneKeyWait(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		paths    [2]string
		data     [2]string
		outcome  string
		evidence int
	}{
		{"path", [2]string{"/case/one.bin", "/case/one.bin"}, [2]string{"first content", "second content"}, string(store.CommitStored), 2},
		{"content", [2]string{"/case/one.bin", "/case/two.bin"}, [2]string{"one content", "one content"}, string(store.CommitAliased), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lim := DefaultUploadLimits()
			lim.Jobs = 4
			lim.FinalizeWait = 100 * time.Millisecond
			hold, holding := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			ls := uploadServer(t, &lim, func(m *uploads) {
				next := m.runIngest
				m.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
					if calls.Add(1) == 1 {
						close(holding)
						<-hold
					}
					return next(ctx, req)
				}
			})
			unhold := sync.OnceFunc(func() { close(hold) })
			t.Cleanup(unhold)
			answers := make(chan reply, 2)
			first := received(t, ls, c.paths[0], []byte(c.data[0]))
			finalizeAt(ls, first, answers)
			select {
			case <-holding:
			case <-time.After(queueWait):
				t.Fatal("the first upload was never stored")
			}
			second := received(t, ls, c.paths[1], []byte(c.data[1]))
			finalizeAt(ls, second, answers)
			statusUntil(t, ls, second, "queued", queued)
			unhold()
			for id, want := range map[string]string{first: string(store.CommitStored), second: c.outcome} {
				st := statusUntil(t, ls, id, "done", func(r reply) bool { return r.str("state") == stateDone })
				res, _ := st.body["result"].(map[string]any)
				if got, _ := res["outcome"].(string); got != want {
					t.Errorf("upload %s: outcome %q, want %q", id, got, want)
				}
			}
			if n := evidenceCount(t, ls.db()); n != c.evidence {
				t.Errorf("%d evidence records, want %d", n, c.evidence)
			}
			for range 2 {
				<-answers
			}
		})
	}
}

// TestQueueJobKeys: an upload's job is keyed by its client path and its
// decoded content, a large one runs alone, and one that cannot be opened
// keeps only its path.
func TestQueueJobKeys(t *testing.T) {
	t.Parallel()
	m := uploadServer(t, nil, nil).uploads
	dir := t.TempDir()
	part := func(name string, data []byte, size int64) *session {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
		return &session{path: path, clientPath: "/case/" + name}
	}
	content := func(s *session) string {
		src, err := evidence.Open(t.Context(), s.path, evidence.Options{Decode: true})
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		fp, err := src.Fingerprint(m.hasher)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s%d:%s", contentKey, src.Size(), fp)
	}
	data := []byte("a small upload")
	small := part("small.bin", data, int64(len(data)))
	large := part("large.bin", data, ingest.HeavySize)
	missing := &session{path: filepath.Join(dir, "missing"), clientPath: "/case/missing.bin"}
	for _, c := range []struct {
		s     *session
		keys  []string
		size  int64
		heavy bool
	}{
		{small, []string{pathKey + small.clientPath, content(small)}, int64(len(data)), false},
		{large, []string{pathKey + large.clientPath, content(large)}, ingest.HeavySize, true},
		{missing, []string{pathKey + missing.clientPath}, -1, false},
	} {
		j, size := m.queueJob(t.Context(), c.s)
		if !slices.Equal(j.keys, c.keys) || size != c.size || j.heavy != c.heavy {
			t.Errorf("%s: keys %q, size %d, heavy %v; want %q, %d, %v", c.s.clientPath, j.keys, size, j.heavy, c.keys, c.size, c.heavy)
		}
		// The queue prices a job only once it may run: the database may be
		// paused when it is finalized.
		want := laneUnknown
		if j.heavy {
			want = 0
		}
		if j.lane != want || j.size != size {
			t.Errorf("%s: lane %d, size %d, heavy %v; want lane %d and size %d", c.s.clientPath, j.lane, j.size, j.heavy, want, size)
		}
	}
}

// TestUploadJobsCapped: more upload jobs than uploads may be in progress
// are cut to that, with a note.
func TestUploadJobsCapped(t *testing.T) {
	t.Parallel()
	lim := DefaultUploadLimits()
	lim.Jobs = lim.MaxSessions * 2
	ls := uploadServer(t, &lim, nil)
	if ls.uploads.queue.jobs != lim.MaxSessions {
		t.Errorf("%d jobs, want %d", ls.uploads.queue.jobs, lim.MaxSessions)
	}
	if !strings.Contains(strings.Join(ls.Notes, "\n"), fmt.Sprintf("not %d", lim.Jobs)) {
		t.Errorf("notes %q say nothing of the cut", ls.Notes)
	}
}
