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
		release, err := q.acquire(ctx, j, func(ahead int) { waiting <- ahead })
		if err != nil {
			a.err <- err
			return
		}
		a.release <- release
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
	q := newIngestQueue(4, 1<<40)
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
	q := newIngestQueue(2, 1<<40)
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
	q := newIngestQueue(4, 1<<40)
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

	alone := newIngestQueue(4, 1<<40)
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
	q := newIngestQueue(4, 100)
	lane := func(name string, bytes int64) *queueJob { j := job("path:/" + name); j.lane = bytes; return j }
	as := map[string]*arrival{}
	as["a"] = arrive(ctx, t, q, lane("a", 60))
	as["b"] = arrive(ctx, t, q, lane("b", 60))
	as["c"] = arrive(ctx, t, q, lane("c", 10))
	wantStarted(t, "first", as, "a")
	as["a"].done(t)
	wantStarted(t, "after a", as, "b", "c")

	idle := newIngestQueue(4, 100)
	if a := arrive(ctx, t, idle, lane("big", 1000)); !a.started() {
		t.Error("a job over the budget waits on an idle queue")
	}
}

// TestQueueCancel: a job whose context ends while it waits leaves the
// queue, and holds back nothing after it; a release twice is harmless.
func TestQueueCancel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := newIngestQueue(1, 1<<40)
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
	q := newIngestQueue(jobs, 1<<40)
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
			release, err := q.acquire(t.Context(), j, nil)
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
			release()
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
	id := start(t, ls, path, "", plainHash(ls.deps.DB, data), int64(len(data))).str("upload_id")
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
	db := ls.deps.DB
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
// server's ingest session is closed.
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
	if _, err := m.ingest.Run(t.Context(), ingest.Request{Path: "unused"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the ingest session after a clean stop: %v, want it closed", err)
	}
}

// TestUploadStuckAtStop: an ingest that ignores the stop makes it unclean,
// and the stop returns rather than wait on the ingest session for it.
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
	if err := ls.stop(t); !errors.Is(err, ErrUncleanShutdown) {
		t.Fatalf("stop: %v, want %v", err, ErrUncleanShutdown)
	}
	// Left open: closing it would have waited on the stuck ingest.
	missing := ingest.Request{Path: filepath.Join(t.TempDir(), "missing")}
	if _, err := ls.uploads.ingest.Run(t.Context(), missing); err == nil || strings.Contains(err.Error(), "closed") {
		t.Errorf("the ingest session after an unclean stop: %v, want it open", err)
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
			if n := evidenceCount(t, ls.deps.DB); n != c.evidence {
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
		if j.heavy != (j.lane == 0) {
			t.Errorf("%s: lane %d, heavy %v; a heavy job holds no lane, the others one", c.s.clientPath, j.lane, j.heavy)
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
