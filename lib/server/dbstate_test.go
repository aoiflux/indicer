package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/engine"
	"indicer/lib/fts"
	"indicer/lib/ingest"
	"indicer/lib/store"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/pb/pbconnect"

	"connectrpc.com/connect"
	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	// finalizeWait keeps a finalize of an upload held by a pause short.
	finalizeWait = 100 * time.Millisecond
	// shortWait is a pause's wait that a test means to run out.
	shortWait = 20 * time.Millisecond
	// pollEvery is how often a test looks again at what it waits for.
	pollEvery = 5 * time.Millisecond
	// holdProcedure is a test RPC that holds its lease until it ends.
	holdProcedure = "/dues.test.Hold/Hold"
	// leasePath is a test route that holds a lease.
	leasePath = "/test/lease"
	// stuckHeader asks the lease route to hold its lease until released.
	stuckHeader = "Test-Stuck"
)

// pauseServer serves a fresh database with lim, logging what it does; setup
// runs before it listens.
func pauseServer(t *testing.T, lim Limits, setup func(*Server)) (*liveServer, *observer.ObservedLogs) {
	t.Helper()
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.InfoLevel)
	return startServer(t, Config{Limits: lim}, Deps{Handle: h, Log: zap.New(core)}, setup), logs
}

// pauseIn runs a pause as server work, as an export does, and sends what it
// returned.
func pauseIn(ls *liveServer, opts pauseOptions, use func(*dbopen.Suspended) error) <-chan error {
	done := make(chan error, 1)
	ls.goWork(func(ctx context.Context) { done <- ls.whilePaused(ctx, opts, use) })
	return done
}

// await waits for what ch sends.
func await[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(queueWait):
		t.Fatalf("%s: never came", what)
	}
	var zero T
	return zero
}

// until waits for ok to hold.
func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(queueWait)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: never", what)
		}
		time.Sleep(pollEvery)
	}
}

// holdUse is a pause's use that holds the database suspended until it is
// released. Made after the server, its cleanup releases it before the
// server stops.
type holdUse struct {
	in      chan *dbopen.Suspended
	gate    chan struct{}
	release func()
	calls   atomic.Int32
}

func newHoldUse(t *testing.T) *holdUse {
	u := &holdUse{in: make(chan *dbopen.Suspended, 1), gate: make(chan struct{})}
	u.release = sync.OnceFunc(func() { close(u.gate) })
	t.Cleanup(u.release)
	return u
}

func (u *holdUse) use(held *dbopen.Suspended) error {
	if u.calls.Add(1) == 1 {
		u.in <- held
	}
	<-u.gate
	return nil
}

// wait waits until the database is suspended and in use.
func (u *holdUse) wait(t *testing.T) *dbopen.Suspended {
	t.Helper()
	return await(t, "the database in use while paused", u.in)
}

// noUse is a pause's use that must not be called.
func noUse(t *testing.T) func(*dbopen.Suspended) error {
	return func(*dbopen.Suspended) error {
		t.Error("the paused database was used")
		return nil
	}
}

func instant(*dbopen.Suspended) error { return nil }

// recordResumes has s's resumes recorded; fail, when set, may fail a
// resume, numbered from 1, instead of the real one.
func recordResumes(s *Server, fail func(call int) error) func() []dbopen.ResumeOptions {
	var mu sync.Mutex
	var seen []dbopen.ResumeOptions
	orig := s.dbs.resume
	s.dbs.resume = func(held *dbopen.Suspended, ctx context.Context, opts dbopen.ResumeOptions) (*dbopen.Handle, error) {
		mu.Lock()
		seen = append(seen, opts)
		n := len(seen)
		mu.Unlock()
		if fail != nil {
			if err := fail(n); err != nil {
				return nil, err
			}
		}
		return orig(held, ctx, opts)
	}
	return func() []dbopen.ResumeOptions {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func queuePaused(q *ingestQueue) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.paused
}

func gen(ls *liveServer) uint64 { return ls.dbs.current().gen }

// ready asks /readyz.
func ready(t *testing.T, ls *liveServer) (status int, body, retryAfter string) {
	t.Helper()
	resp, body := do(t, newReq(t, "GET", ls.url+"/readyz", nil))
	return resp.StatusCode, body, resp.Header.Get(headerRetryAfter)
}

// wantLive checks that the server serves its database.
func wantLive(t *testing.T, ls *liveServer) {
	t.Helper()
	if status, body, _ := ready(t, ls); status != http.StatusOK || body != bodyReady {
		t.Errorf("readyz: %d %q, want %d %q", status, body, http.StatusOK, bodyReady)
	}
	if ls.dbs.isPaused() || queuePaused(ls.uploads.queue) {
		t.Errorf("gate shut %v, queue paused %v; want both open", ls.dbs.isPaused(), queuePaused(ls.uploads.queue))
	}
	c := pbconnect.NewDuesServiceClient(testClient, ls.url)
	if _, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{})); err != nil {
		t.Errorf("a read: %v", err)
	}
}

// receive starts an upload of data, whose hash is given, and sends all of
// it; unlike received, it reads no database, so it runs while one is paused.
func receive(t *testing.T, ls *liveServer, path, hash string, data []byte) string {
	t.Helper()
	r := start(t, ls, path, "", hash, int64(len(data)))
	id := r.str("upload_id")
	if r.status != http.StatusOK || id == "" {
		t.Fatalf("start of %s: %d %v", path, r.status, r.body)
	}
	if r := chunk(t, ls, id, 0, data); r.status != http.StatusOK {
		t.Fatalf("chunk of %s: %d %v", path, r.status, r.body)
	}
	return id
}

// wantStored waits until upload id ends, and checks it stored its content.
func wantStored(t *testing.T, ls *liveServer, id string) {
	t.Helper()
	st := statusUntil(t, ls, id, "ended", func(r reply) bool { return r.str("state") == stateDone || r.str("state") == stateFailed })
	res, _ := st.body["result"].(map[string]any)
	if got, _ := res["outcome"].(string); st.str("state") != stateDone || got != string(store.CommitStored) {
		t.Errorf("upload %s: %v, want it stored", id, st.body)
	}
}

// leasedCalls calls each RPC that holds a lease on the database.
func leasedCalls(ctx context.Context, c pbconnect.DuesServiceClient) map[string]error {
	_, evi := c.GetEviFiles(ctx, connect.NewRequest(&pb.GetEviFilesReq{}))
	_, parti := c.GetPartiFiles(ctx, connect.NewRequest(&pb.GetPartiFilesReq{}))
	_, idx := c.GetIdxFiles(ctx, connect.NewRequest(&pb.GetIdxFilesReq{}))
	_, exists := c.AppendIfExists(ctx, connect.NewRequest(&pb.AppendIfExistsReq{}))
	_, search := c.Search(ctx, connect.NewRequest(&pb.SearchReq{}))
	return map[string]error{"GetEviFiles": evi, "GetPartiFiles": parti, "GetIdxFiles": idx, "AppendIfExists": exists, "Search": search}
}

// wantRefusedPaused checks an RPC was refused as unavailable while paused,
// saying when to retry.
func wantRefusedPaused(t *testing.T, what string, err error) {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Meta().Get(headerRetryAfter) != pausedRetryAfter {
		t.Errorf("%s: %v, want unavailable with %s %s", what, err, headerRetryAfter, pausedRetryAfter)
	}
}

// leaseRoute adds a route holding a lease, served by h.
func leaseRoute(h http.HandlerFunc) func(*Server) {
	return func(s *Server) { s.handle("POST "+leasePath, route{scope: ScopeRead, lease: true}, h) }
}

// sendAsync sends req from its own goroutine and sends the status it got, 0
// when it got none.
func sendAsync(req *http.Request) <-chan int {
	out := make(chan int, 1)
	go func() {
		resp, err := testClient.Do(req)
		if err != nil {
			out <- 0
			return
		}
		_ = resp.Body.Close()
		out <- resp.StatusCode
	}()
	return out
}

// TestPauseGatesDatabaseRoutes: while the database is paused, the routes
// that read it are refused with when to retry, readyz says paused, and the
// upload routes still answer, a finalize as queued. Once it resumes, reads
// work on the reopened database, an upload begun meanwhile kept its part
// file, and the queued upload is stored.
func TestPauseGatesDatabaseRoutes(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.Upload.FinalizeWait = finalizeWait
	ls, _ := pauseServer(t, lim, nil)
	c := pbconnect.NewDuesServiceClient(testClient, ls.url)
	before := ls.db()
	keptData, queuedData, abortedData := []byte("received while paused"), []byte("stored after the pause"), []byte("aborted while paused")
	keptHash, queuedHash, abortedHash := plainHash(before, keptData), plainHash(before, queuedData), plainHash(before, abortedData)

	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "test"}, u.use)
	u.wait(t)
	for name, err := range leasedCalls(context.Background(), c) {
		wantRefusedPaused(t, name+" while paused", err)
	}
	if status, body, retry := ready(t, ls); status != http.StatusServiceUnavailable || body != bodyPaused || retry != pausedRetryAfter {
		t.Errorf("readyz while paused: %d %q, %s %q", status, body, headerRetryAfter, retry)
	}
	if resp, body := do(t, newReq(t, "GET", ls.url+"/healthz", nil)); resp.StatusCode != http.StatusOK {
		t.Errorf("healthz while paused: %d %q", resp.StatusCode, body)
	}
	kept := receive(t, ls, "/case/kept.bin", keptHash, keptData)
	if st := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": kept}); st.status != http.StatusOK || st.str("state") != stateReceiving {
		t.Errorf("status while paused: %d %v", st.status, st.body)
	}
	aborted := receive(t, ls, "/case/aborted.bin", abortedHash, abortedData)
	if r := postJSON(t, ls, "/web/upload/abort", map[string]string{"upload_id": aborted}); r.status != http.StatusOK {
		t.Errorf("abort while paused: %d %v", r.status, r.body)
	}
	queuedID := receive(t, ls, "/case/queued.bin", queuedHash, queuedData)
	if r := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": queuedID}); r.status != http.StatusAccepted || r.str("state") != stateFinalizing {
		t.Errorf("finalize while paused: %d %v", r.status, r.body)
	}
	statusUntil(t, ls, queuedID, "queued", queued)

	u.release()
	if err := await(t, "the pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
	wantLive(t, ls)
	if g := gen(ls); g != 2 {
		t.Errorf("gen %d after the resume, want 2", g)
	}
	if ls.db() == before {
		t.Error("the resumed database is the one suspended")
	}
	if parts := partFiles(t, ls); !slices.Contains(parts, kept+".part") {
		t.Errorf("part files after the resume: %v, want %s.part kept", parts, kept)
	}
	wantStored(t, ls, queuedID)
	res, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{}))
	if err != nil || len(res.Msg.EviFile) != 1 {
		t.Errorf("evidence after the resume: %v, want the queued upload", err)
	}
}

// TestPauseWaitsForLease: a pause does not suspend the database under a
// request using it; new requests are refused meanwhile.
func TestPauseWaitsForLease(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.PauseLeaseWait = queueWait
	entered, leave := make(chan struct{}), make(chan struct{})
	causes := make(chan error, 1)
	var suspends atomic.Int32
	ls, _ := pauseServer(t, lim, func(s *Server) {
		leaseRoute(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(stuckHeader) == "" {
				w.WriteHeader(http.StatusOK)
				return
			}
			close(entered)
			<-leave
			causes <- context.Cause(r.Context())
			w.WriteHeader(http.StatusOK)
		})(s)
		orig := s.dbs.suspend
		s.dbs.suspend = func(h *dbopen.Handle) (*dbopen.Suspended, error) {
			suspends.Add(1)
			return orig(h)
		}
	})
	unblock := sync.OnceFunc(func() { close(leave) })
	t.Cleanup(unblock)
	req := newReq(t, "POST", ls.url+leasePath, nil)
	req.Header.Set(stuckHeader, "1")
	answered := sendAsync(req)
	await(t, "the leased request", entered)
	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "test"}, u.use)
	until(t, "the gate to shut", ls.dbs.isPaused)
	r := post(t, newReq(t, "POST", ls.url+leasePath, nil))
	if r.status != http.StatusServiceUnavailable || r.header.Get(headerRetryAfter) != pausedRetryAfter || r.str("error") != msgPaused {
		t.Errorf("a request while the gate is shut: %d %v, %s %q", r.status, r.body, headerRetryAfter, r.header.Get(headerRetryAfter))
	}
	if n := suspends.Load(); n != 0 {
		t.Errorf("suspended %d times under a request", n)
	}
	unblock()
	if cause := await(t, "the leased request's end", causes); cause != nil {
		t.Errorf("the request's context ended: %v", cause)
	}
	if status := await(t, "the leased answer", answered); status != http.StatusOK {
		t.Errorf("the leased request: %d", status)
	}
	u.wait(t)
	u.release()
	if err := await(t, "the pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if n := suspends.Load(); n != 1 {
		t.Errorf("suspended %d times, want 1", n)
	}
}

// TestPauseCutsLease: a request that outlasts PauseLeaseWait is cut, and its
// client is told the database is paused, not that it cancelled; the pause
// then goes on.
func TestPauseCutsLease(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.PauseLeaseWait = shortWait
	entered := make(chan struct{})
	causes := make(chan error, 1)
	ls, logs := pauseServer(t, lim, func(s *Server) {
		h := connect.NewUnaryHandler(holdProcedure, func(ctx context.Context, _ *connect.Request[pb.GetEviFilesReq]) (*connect.Response[pb.GetEviFilesRes], error) {
			close(entered)
			<-ctx.Done()
			causes <- context.Cause(ctx)
			return nil, ctx.Err()
		}, connect.WithInterceptors(rpcErrors{s}))
		s.handle("POST "+holdProcedure, route{scope: ScopeRead, rpc: true, lease: true}, h)
	})
	hold := connect.NewClient[pb.GetEviFilesReq, pb.GetEviFilesRes](testClient, ls.url+holdProcedure)
	answered := make(chan error, 1)
	go func() {
		_, err := hold.CallUnary(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{}))
		answered <- err
	}()
	await(t, "the leased call", entered)
	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "test"}, u.use)
	if cause := await(t, "the cut", causes); !errors.Is(cause, errPaused) {
		t.Errorf("the call's cause: %v, want %v", cause, errPaused)
	}
	wantRefusedPaused(t, "the cut call", await(t, "the cut call's answer", answered))
	u.wait(t)
	if n := logs.FilterMessage("server pause cancelled requests").FilterField(zap.Int("count", 1)).Len(); n != 1 {
		t.Errorf("%d logs of 1 request cut, want 1", n)
	}
	u.release()
	if err := await(t, "the pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
	wantLive(t, ls)
}

// TestLeaseEndsBeforeResponse: an RPC's lease ends when its handler returns,
// before the response is written, so a client that reads slowly does not
// hold a pause; ending it again at the end of the request changes nothing.
func TestLeaseEndsBeforeResponse(t *testing.T) {
	t.Parallel()
	ls, _ := pauseServer(t, testLimits(), nil)
	ctx, end, ok := ls.dbs.lease(t.Context())
	if !ok {
		t.Fatal("lease refused on an open database")
	}
	t.Cleanup(end)
	var during int
	call := rpcErrors{ls.Server}.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		during = ls.dbs.leases.count()
		return nil, nil
	})
	if _, err := call(ctx, connect.NewRequest(&pb.GetEviFilesReq{})); err != nil {
		t.Fatal(err)
	}
	if during != 1 {
		t.Errorf("%d leases while the handler ran, want 1", during)
	}
	if n := ls.dbs.leases.count(); n != 0 {
		t.Errorf("%d leases once the handler returned, want 0", n)
	}
	if ctx.Err() != nil {
		t.Errorf("the request's context ended with its lease: %v", context.Cause(ctx))
	}
	end()
	if n := ls.dbs.leases.count(); n != 0 {
		t.Errorf("%d leases after the request ended, want 0", n)
	}
}

// TestCutAnswersOnlyCancellation: a call cut by a pause is told the database
// is paused only when the cut ended it; an answer the handler already chose
// passes unchanged.
func TestCutAnswersOnlyCancellation(t *testing.T) {
	t.Parallel()
	ls, _ := pauseServer(t, testLimits(), nil)
	ctx, cut := context.WithCancelCause(t.Context())
	cut(errPaused)
	for _, c := range []struct {
		err  error
		want connect.Code
	}{
		{context.Canceled, connect.CodeUnavailable},
		{fmt.Errorf("chunk map: %w", context.Canceled), connect.CodeUnavailable},
		{invalidArg("bad"), connect.CodeInvalidArgument},
		{connect.NewError(connect.CodeNotFound, errors.New("none")), connect.CodeNotFound},
	} {
		err := ls.rpcError(ctx, holdProcedure, c.err)
		if got := connect.CodeOf(err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
		var ce *connect.Error
		if c.want == connect.CodeUnavailable && (!errors.As(err, &ce) || ce.Meta().Get(headerRetryAfter) != pausedRetryAfter) {
			t.Errorf("%v: no %s %s", c.err, headerRetryAfter, pausedRetryAfter)
		}
	}
}

// TestPauseGivesUpOnStuckLease: a request that ignores its cut gives the
// pause up; the server goes on with the same database.
func TestPauseGivesUpOnStuckLease(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.PauseLeaseWait = shortWait
	entered, leave := make(chan struct{}), make(chan struct{})
	causes := make(chan error, 1)
	ls, _ := pauseServer(t, lim, leaseRoute(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(stuckHeader) == "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		close(entered)
		<-leave
		causes <- context.Cause(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	unblock := sync.OnceFunc(func() { close(leave) })
	t.Cleanup(unblock)
	req := newReq(t, "POST", ls.url+leasePath, nil)
	req.Header.Set(stuckHeader, "1")
	answered := sendAsync(req)
	await(t, "the stuck request", entered)
	before := ls.db()
	err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test"}, noUse(t)))
	if !errors.Is(err, errPauseStuck) || !strings.Contains(err.Error(), stageRequests) {
		t.Fatalf("pause: %v, want %v at %s", err, errPauseStuck, stageRequests)
	}
	wantLive(t, ls)
	if g := gen(ls); g != 1 || ls.db() != before {
		t.Errorf("gen %d, same database %v; want gen 1 on the same database", g, ls.db() == before)
	}
	if r := post(t, newReq(t, "POST", ls.url+leasePath, nil)); r.status != http.StatusOK {
		t.Errorf("a request after the pause gave up: %d %v", r.status, r.body)
	}
	unblock()
	if cause := await(t, "the stuck request's end", causes); !errors.Is(cause, errPaused) {
		t.Errorf("the stuck request's cause: %v, want %v", cause, errPaused)
	}
	if status := await(t, "the stuck answer", answered); status != http.StatusOK {
		t.Errorf("the stuck request: %d", status)
	}
}

// TestPauseDrainsIngest: an upload being stored when a pause begins is
// stored before the database is suspended, and is not cut. Reads go on while
// it drains: the gate shuts only after it.
func TestPauseDrainsIngest(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.Upload.FinalizeWait = finalizeWait
	storing, proceed := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var cut atomic.Bool
	var resumes func() []dbopen.ResumeOptions
	ls, _ := pauseServer(t, lim, func(s *Server) {
		resumes = recordResumes(s, nil)
		next := s.uploads.runIngest
		s.uploads.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			if calls.Add(1) == 1 {
				close(storing)
			}
			<-proceed
			res, err := next(ctx, req)
			if ctx.Err() != nil {
				cut.Store(true)
			}
			return res, err
		}
	})
	unblock := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(unblock)
	c := pbconnect.NewDuesServiceClient(testClient, ls.url)
	data := []byte("stored before the pause")
	id := receive(t, ls, "/case/drained.bin", plainHash(ls.db(), data), data)
	answers := make(chan reply, 1)
	finalizeAt(ls, id, answers)
	await(t, "the ingest", storing)

	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "test", Drain: queueWait}, u.use)
	until(t, "the queue to pause", func() bool { return queuePaused(ls.uploads.queue) })
	if _, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{})); err != nil {
		t.Errorf("a read while the uploads drain: %v", err)
	}
	if status, body, _ := ready(t, ls); status != http.StatusOK || body != bodyReady {
		t.Errorf("readyz while the uploads drain: %d %q", status, body)
	}
	unblock()
	u.wait(t)
	wantStored(t, ls, id)
	u.release()
	if err := await(t, "the pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if n := calls.Load(); n != 1 || cut.Load() {
		t.Errorf("%d ingests, cut %v; want 1, not cut", n, cut.Load())
	}
	if got := resumes(); len(got) != 1 || got[0] != (dbopen.ResumeOptions{KeepUploads: true}) {
		t.Errorf("resumes %+v, want one keeping uploads, without recovery", got)
	}
	await(t, "the finalize answer", answers)
}

// TestPauseRequeuesIngest: an upload still being stored once the drain ends
// is cut before it commits, waits through the pause with its part file, and
// is stored after it, resuming the attempt it began; the resume recovers.
func TestPauseRequeuesIngest(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.IdleWait = queueWait
	lim.Upload.FinalizeWait = finalizeWait
	atFlush := make(chan struct{})
	var calls atomic.Int32
	var resumed atomic.Bool
	var resumes func() []dbopen.ResumeOptions
	ls, logs := pauseServer(t, lim, func(s *Server) {
		resumes = recordResumes(s, nil)
		next := s.uploads.runIngest
		s.uploads.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			if calls.Add(1) == 1 {
				veto := req.BeforeFlush
				req.BeforeFlush = func() error {
					close(atFlush)
					<-ctx.Done()
					return veto()
				}
				return next(ctx, req)
			}
			res, err := next(ctx, req)
			resumed.Store(res.Resumed)
			return res, err
		}
	})
	data := bytes.Repeat([]byte("cut, then stored again "), 10_000)
	id := receive(t, ls, "/case/requeued.bin", plainHash(ls.db(), data), data)
	answers := make(chan reply, 1)
	finalizeAt(ls, id, answers)
	await(t, "the ingest at its flush", atFlush)

	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "test", Drain: shortWait}, u.use)
	u.wait(t)
	if parts := partFiles(t, ls); !slices.Contains(parts, id+".part") {
		t.Errorf("part files while paused: %v, want %s.part", parts, id)
	}
	if st := statusUntil(t, ls, id, "queued again", queued); st.str("state") != stateFinalizing {
		t.Errorf("the cut upload while paused: %v", st.body)
	}
	u.release()
	if err := await(t, "the pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
	wantStored(t, ls, id)
	if n := calls.Load(); n != 2 || !resumed.Load() {
		t.Errorf("%d ingests, the last resumed %v; want 2, resumed", n, resumed.Load())
	}
	if n := evidenceCount(t, ls.db()); n != 1 {
		t.Errorf("%d evidence records, want 1", n)
	}
	if parts := partFiles(t, ls); len(parts) != 0 {
		t.Errorf("part files left: %v", parts)
	}
	if got := resumes(); len(got) != 1 || got[0] != (dbopen.ResumeOptions{Recover: true, KeepUploads: true}) {
		t.Errorf("resumes %+v, want one recovering and keeping uploads", got)
	}
	if n := logs.FilterMessage("upload requeued").FilterField(zap.String("upload_id", id)).Len(); n != 1 {
		t.Errorf("%d requeue logs, want 1", n)
	}
	await(t, "the finalize answer", answers)
}

// TestPauseSparesCommittingIngest: an upload that began to commit is not
// cut. The pause waits IdleWait more for it, and gives up if it does not
// end by then.
func TestPauseSparesCommittingIngest(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		ends  bool
		limit time.Duration
	}{
		{"ends", true, queueWait},
		{"stuck", false, testLimits().IdleWait},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			lim := testLimits()
			lim.IdleWait = c.limit
			lim.Upload.FinalizeWait = finalizeWait
			committing, proceed := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			var cut atomic.Bool
			ls, logs := pauseServer(t, lim, func(s *Server) {
				next := s.uploads.runIngest
				s.uploads.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
					calls.Add(1)
					if err := req.BeforeFlush(); err != nil {
						return ingest.Result{}, err
					}
					close(committing)
					<-proceed
					cut.Store(ctx.Err() != nil)
					return next(ctx, req)
				}
			})
			unblock := sync.OnceFunc(func() { close(proceed) })
			t.Cleanup(unblock)
			data := []byte("committing when the pause began")
			id := receive(t, ls, "/case/committing.bin", plainHash(ls.db(), data), data)
			answers := make(chan reply, 1)
			finalizeAt(ls, id, answers)
			await(t, "the ingest committing", committing)

			if c.ends {
				u := newHoldUse(t)
				done := pauseIn(ls, pauseOptions{For: "test", Drain: shortWait}, u.use)
				until(t, "the drain to end", func() bool {
					return logs.FilterMessage("server pause cancelled uploads").FilterField(zap.Int("count", 0)).Len() > 0
				})
				unblock()
				u.wait(t)
				wantStored(t, ls, id)
				u.release()
				if err := await(t, "the pause", done); err != nil {
					t.Fatalf("pause: %v", err)
				}
			} else {
				err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test", Drain: shortWait}, noUse(t)))
				if !errors.Is(err, errPauseStuck) || !strings.Contains(err.Error(), stageUploads) {
					t.Fatalf("pause: %v, want %v at %s", err, errPauseStuck, stageUploads)
				}
				wantLive(t, ls)
				if g := gen(ls); g != 1 {
					t.Errorf("gen %d after the pause gave up, want 1", g)
				}
				unblock()
				wantStored(t, ls, id)
			}
			if n := calls.Load(); n != 1 || cut.Load() {
				t.Errorf("%d ingests, cut %v; want 1, not cut", n, cut.Load())
			}
			await(t, "the finalize answer", answers)
		})
	}
}

// TestPauseGivesUpOnStuckIngest: an upload that ignores its cut gives the
// pause up; the queue admits again, and the upload is stored once it moves.
func TestPauseGivesUpOnStuckIngest(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.Upload.FinalizeWait = finalizeWait
	storing, proceed := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ls, _ := pauseServer(t, lim, func(s *Server) {
		next := s.uploads.runIngest
		s.uploads.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			if calls.Add(1) == 1 {
				close(storing)
				<-proceed
			}
			return next(ctx, req)
		}
	})
	unblock := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(unblock)
	data := []byte("stuck when the pause began")
	id := receive(t, ls, "/case/stuck.bin", plainHash(ls.db(), data), data)
	answers := make(chan reply, 1)
	finalizeAt(ls, id, answers)
	await(t, "the ingest", storing)

	err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test", Drain: shortWait}, noUse(t)))
	if !errors.Is(err, errPauseStuck) || !strings.Contains(err.Error(), stageUploads) {
		t.Fatalf("pause: %v, want %v at %s", err, errPauseStuck, stageUploads)
	}
	wantLive(t, ls)
	if g := gen(ls); g != 1 {
		t.Errorf("gen %d after the pause gave up, want 1", g)
	}
	unblock()
	// Its turn was cut, so it is stored in a turn of its own.
	wantStored(t, ls, id)
	if n := calls.Load(); n != 2 {
		t.Errorf("%d ingests, want 2", n)
	}
	await(t, "the finalize answer", answers)
}

// TestPauseQuiescedFails: a pause whose last step before the suspend fails
// is given up; the database goes on with a new session.
func TestPauseQuiescedFails(t *testing.T) {
	t.Parallel()
	ls, _ := pauseServer(t, testLimits(), nil)
	boom := errors.New("simulated quiesce failure")
	before := ls.dbs.current()
	var seen atomic.Pointer[badger.DB]
	err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test", Quiesced: func(_ context.Context, db *badger.DB) error {
		seen.Store(db)
		return boom
	}}, noUse(t)))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), stageQuiesced) {
		t.Fatalf("pause: %v, want %v at %s", err, boom, stageQuiesced)
	}
	if seen.Load() != before.db {
		t.Error("Quiesced was not given the open database")
	}
	after := ls.dbs.current()
	if after.gen != 2 || after.db != before.db || after.sess == before.sess {
		t.Errorf("after the pause: gen %d, same database %v, same session %v; want gen 2, the same database, a new session",
			after.gen, after.db == before.db, after.sess == before.sess)
	}
	if _, err := before.sess.Run(t.Context(), ingest.Request{Path: "unused"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the session before the pause: %v, want it closed", err)
	}
	wantLive(t, ls)
	data := []byte("stored after the pause gave up")
	id := receive(t, ls, "/case/after.bin", plainHash(ls.db(), data), data)
	postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id})
	wantStored(t, ls, id)
}

// TestPauseSuspendRefused: a database the suspend refuses, here because its
// full-text index is held, is given back; uploads go on.
func TestPauseSuspendRefused(t *testing.T) {
	t.Parallel()
	ls, _ := pauseServer(t, testLimits(), nil)
	held := make(chan *fts.Index, 1)
	t.Cleanup(func() {
		select {
		case x := <-held:
			_ = x.Release()
		default:
		}
	})
	err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test", Quiesced: func(_ context.Context, db *badger.DB) error {
		x, err := fts.Acquire(db, true)
		if err != nil {
			return err
		}
		held <- x
		return nil
	}}, noUse(t)))
	if err == nil || !strings.Contains(err.Error(), stageSuspend) || !strings.Contains(err.Error(), "full-text index") {
		t.Fatalf("pause: %v, want the suspend's refusal", err)
	}
	if err := await(t, "the held index", held).Release(); err != nil {
		t.Fatal(err)
	}
	if g := gen(ls); g != 2 {
		t.Errorf("gen %d after the refusal, want 2", g)
	}
	wantLive(t, ls)
	data := []byte("stored after the refusal")
	id := receive(t, ls, "/case/after.bin", plainHash(ls.db(), data), data)
	postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id})
	wantStored(t, ls, id)
}

// TestPauseSuspendUnclean: a database that did not close cleanly is not
// used, and is resumed with recovery.
func TestPauseSuspendUnclean(t *testing.T) {
	t.Parallel()
	boom := errors.New("simulated unclean close")
	var resumes func() []dbopen.ResumeOptions
	ls, _ := pauseServer(t, testLimits(), func(s *Server) {
		resumes = recordResumes(s, nil)
		s.dbs.suspend = func(h *dbopen.Handle) (*dbopen.Suspended, error) {
			held, err := h.Suspend()
			if err != nil {
				return held, err
			}
			return held, boom
		}
	})
	err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test"}, noUse(t)))
	if !errors.Is(err, boom) {
		t.Fatalf("pause: %v, want %v", err, boom)
	}
	if got := resumes(); len(got) != 1 || got[0] != (dbopen.ResumeOptions{Recover: true, KeepUploads: true}) {
		t.Errorf("resumes %+v, want one recovering and keeping uploads", got)
	}
	if g := gen(ls); g != 2 {
		t.Errorf("gen %d after the resume, want 2", g)
	}
	wantLive(t, ls)
}

// TestResumeRetries: a database that does not reopen at once is tried again.
func TestResumeRetries(t *testing.T) {
	t.Parallel()
	boom := errors.New("simulated resume failure")
	var resumes func() []dbopen.ResumeOptions
	ls, logs := pauseServer(t, testLimits(), func(s *Server) {
		resumes = recordResumes(s, func(call int) error {
			if call <= resumeRetries {
				return boom
			}
			return nil
		})
	})
	if err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test"}, instant)); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if n := len(resumes()); n != resumeRetries+1 {
		t.Errorf("%d resumes, want %d", n, resumeRetries+1)
	}
	if n := logs.FilterMessage("server resume failed").Len(); n != resumeRetries {
		t.Errorf("%d failed resumes logged, want %d", n, resumeRetries)
	}
	wantLive(t, ls)
}

// TestResumeGivesUpStopsServer: a database that never reopens stops the
// server, which releases it.
func TestResumeGivesUpStopsServer(t *testing.T) {
	t.Parallel()
	boom := errors.New("simulated resume failure")
	var resumes func() []dbopen.ResumeOptions
	ls, _ := pauseServer(t, testLimits(), func(s *Server) {
		resumes = recordResumes(s, func(int) error { return boom })
	})
	err := await(t, "the pause", pauseIn(ls, pauseOptions{For: "test"}, instant))
	if !errors.Is(err, boom) {
		t.Fatalf("pause: %v, want %v", err, boom)
	}
	served := await(t, "the server to stop", ls.done)
	ls.done <- served // for the cleanup
	if !errors.Is(served, boom) {
		t.Errorf("Serve: %v, want %v", served, boom)
	}
	if n := len(resumes()); n != resumeRetries+1 {
		t.Errorf("%d resumes, want %d", n, resumeRetries+1)
	}
	if ls.dbs.h.DB != nil {
		t.Error("the database is open after the server stopped")
	}
	wantReopens(t, ls.root)
}

// TestStopWhilePaused: a server stopped while its database is paused stops
// cleanly without reopening it; the uploads it held fail and leave nothing.
func TestStopWhilePaused(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.Upload.FinalizeWait = finalizeWait
	var resumes func() []dbopen.ResumeOptions
	ls, _ := pauseServer(t, lim, func(s *Server) { resumes = recordResumes(s, nil) })
	data := []byte("queued at the stop")
	hash := plainHash(ls.db(), data)
	inUse := make(chan struct{})
	done := pauseIn(ls, pauseOptions{For: "test"}, func(*dbopen.Suspended) error {
		close(inUse)
		<-ls.base.Done()
		return nil
	})
	await(t, "the database in use while paused", inUse)
	id := receive(t, ls, "/case/queued.bin", hash, data)
	postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id})
	statusUntil(t, ls, id, "queued", queued)
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := await(t, "the pause", done); !errors.Is(err, errStopping) {
		t.Errorf("pause: %v, want %v", err, errStopping)
	}
	if n := len(resumes()); n != 0 {
		t.Errorf("%d resumes after the stop, want 0", n)
	}
	m := ls.uploads
	m.mu.Lock()
	s := m.sessions[id]
	state, uerr := s.state, s.err
	m.mu.Unlock()
	if state != stateFailed || uerr == nil || uerr.status != http.StatusServiceUnavailable {
		t.Errorf("the queued upload at stop: %s, %v; want failed with 503", state, uerr)
	}
	if parts := partFiles(t, ls); len(parts) != 0 {
		t.Errorf("part files left: %v", parts)
	}
	if ls.dbs.h.DB != nil {
		t.Error("the database is open after the server stopped")
	}
	wantReopens(t, ls.root)
}

// TestStopDuringDrain: a stop gives up a pause still draining uploads, and
// is clean.
func TestStopDuringDrain(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.Upload.FinalizeWait = finalizeWait
	storing := make(chan struct{})
	ls, _ := pauseServer(t, lim, func(s *Server) {
		s.uploads.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			close(storing)
			<-ctx.Done()
			return ingest.Result{}, ctx.Err()
		}
	})
	data := []byte("being stored at the stop")
	id := receive(t, ls, "/case/draining.bin", plainHash(ls.db(), data), data)
	answers := make(chan reply, 1)
	finalizeAt(ls, id, answers)
	await(t, "the ingest", storing)
	done := pauseIn(ls, pauseOptions{For: "test", Drain: queueWait}, noUse(t))
	until(t, "the queue to pause", func() bool { return queuePaused(ls.uploads.queue) })
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := await(t, "the pause", done); !errors.Is(err, errStopping) || !strings.Contains(err.Error(), stageUploads) {
		t.Errorf("pause: %v, want %v at %s", err, errStopping, stageUploads)
	}
}

// TestOnePauseAtATime: a pause asked while one runs, or while the server
// stops, is refused.
func TestOnePauseAtATime(t *testing.T) {
	t.Parallel()
	ls, _ := pauseServer(t, testLimits(), nil)
	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "first"}, u.use)
	u.wait(t)
	if err := ls.whilePaused(t.Context(), pauseOptions{For: "second"}, noUse(t)); !errors.Is(err, errPausing) {
		t.Errorf("a second pause: %v, want %v", err, errPausing)
	}
	u.release()
	if err := await(t, "the first pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := ls.whilePaused(t.Context(), pauseOptions{For: "stopped"}, noUse(t)); !errors.Is(err, errStopping) {
		t.Errorf("a pause of a stopped server: %v, want %v", err, errStopping)
	}
}

// TestPauseUnderLease: a pause asked by a request using the database would
// wait for itself, so it is refused at once.
func TestPauseUnderLease(t *testing.T) {
	t.Parallel()
	refused := make(chan error, 1)
	ls, _ := pauseServer(t, testLimits(), func(s *Server) {
		leaseRoute(func(w http.ResponseWriter, r *http.Request) {
			refused <- s.whilePaused(r.Context(), pauseOptions{For: "test"}, noUse(t))
			w.WriteHeader(http.StatusOK)
		})(s)
	})
	if r := post(t, newReq(t, "POST", ls.url+leasePath, nil)); r.status != http.StatusOK {
		t.Errorf("the leased request: %d %v", r.status, r.body)
	}
	if err := await(t, "the refusal", refused); !errors.Is(err, errPauseUnderLease) {
		t.Errorf("a pause under a lease: %v, want %v", err, errPauseUnderLease)
	}
	wantLive(t, ls)
}

// TestStreamFileAcrossPause: a StreamFile upload sent while the database is
// paused is stored once it resumes, and only then answered.
func TestStreamFileAcrossPause(t *testing.T) {
	t.Parallel()
	ls, _ := pauseServer(t, testLimits(), nil)
	c := pbconnect.NewDuesServiceClient(testClient, ls.url)
	data := bytes.Repeat([]byte("streamed across a pause "), 10_000)
	hash := plainHash(ls.db(), data)
	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "test"}, u.use)
	u.wait(t)
	type answer struct {
		res *connect.Response[pb.StreamFileRes]
		err error
	}
	answers := make(chan answer, 1)
	go func() {
		stream := c.StreamFile(context.Background())
		meta := &pb.StreamFileReq{Payload: &pb.StreamFileReq_FileMeta{FileMeta: &pb.StreamFileMeta{
			FilePath: "/client/paused.bin", FileHash: hash, Size: int64(len(data)),
		}}}
		if err := stream.Send(meta); err != nil {
			answers <- answer{err: err}
			return
		}
		if err := stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_File{File: data}}); err != nil {
			answers <- answer{err: err}
			return
		}
		res, err := stream.CloseAndReceive()
		answers <- answer{res, err}
	}()
	m := ls.uploads
	until(t, "the stream to queue", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, s := range m.sessions {
			if s.queued {
				return true
			}
		}
		return false
	})
	select {
	case a := <-answers:
		t.Fatalf("the stream was answered while paused: %v", a.err)
	default:
	}
	u.release()
	if err := await(t, "the pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
	a := await(t, "the stream's answer", answers)
	if a.err != nil || !a.res.Msg.GetDone() || a.res.Msg.GetEviFile().GetFilePath() != "/client/paused.bin" {
		t.Fatalf("the stream: %v, %v", a.err, a.res)
	}
	if n := evidenceCount(t, ls.db()); n != 1 {
		t.Errorf("%d evidence records, want 1", n)
	}
}

// TestCheckHashWhilePaused: an upload started while the database is paused
// is checked against the database's own hash.
func TestCheckHashWhilePaused(t *testing.T) {
	t.Parallel()
	h, err := dbopen.Open(context.Background(), dbopen.Request{
		Path: filepath.Join(t.TempDir(), "db"), Mode: dbopen.ReadWrite, Layout: dbopen.Requested{ChunkHash: cnst.SHA3},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	ls := startServer(t, Config{}, Deps{Handle: h}, nil)
	data := []byte("hashed with sha3")
	hash := plainHash(ls.db(), data)
	u := newHoldUse(t)
	done := pauseIn(ls, pauseOptions{For: "test"}, u.use)
	u.wait(t)
	short := base64.StdEncoding.EncodeToString(data)
	if r := start(t, ls, "/case/bad.bin", "", short, int64(len(data))); r.status != http.StatusBadRequest ||
		!strings.Contains(r.str("error"), cnst.SHA3) || !strings.Contains(r.str("error"), "32-byte") {
		t.Errorf("a wrong hash while paused: %d %v, want the 32-byte %s hash named", r.status, r.body, cnst.SHA3)
	}
	receive(t, ls, "/case/good.bin", hash, data)
	u.release()
	if err := await(t, "the pause", done); err != nil {
		t.Fatalf("pause: %v", err)
	}
}

// TestNewRefusesReadOnly: a server needs its database writable, and leaves
// one it refuses open for its caller to close.
func TestNewRefusesReadOnly(t *testing.T) {
	t.Parallel()
	w := openDB(t)
	root := w.Root
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := dbopen.Open(context.Background(), dbopen.Request{Path: root, Mode: dbopen.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	if _, err := New(Config{Limits: testLimits()}, Deps{Handle: ro}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("New on a read-only database: %v", err)
	}
	if ro.DB == nil || ro.DB.IsClosed() {
		t.Error("New closed the database it refused")
	}
	if _, err := New(Config{Limits: testLimits()}, Deps{}); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("New without a database: %v", err)
	}
}

// TestPauseResumeStress: reads and uploads go on through many pauses; reads
// are served or refused as paused, and every upload is stored.
func TestPauseResumeStress(t *testing.T) {
	t.Parallel()
	const readers, uploads, cycles = 4, 8, 10
	lim := testLimits()
	lim.IdleWait = queueWait
	lim.Upload.Jobs = 2
	lim.Upload.FinalizeWait = finalizeWait
	ls, logs := pauseServer(t, lim, nil)
	c := pbconnect.NewDuesServiceClient(testClient, ls.url)
	ids := make([]string, uploads)
	for i := range uploads {
		data := bytes.Repeat(fmt.Appendf(nil, "stress upload %d ", i), 10_000)
		ids[i] = receive(t, ls, fmt.Sprintf("/case/stress/%d.bin", i), plainHash(ls.db(), data), data)
	}
	answers := make(chan reply, uploads)
	for _, id := range ids {
		finalizeAt(ls, id, answers)
	}
	stop := make(chan struct{})
	var served, refused atomic.Int32
	var mu sync.Mutex
	var bad []string
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{}))
				switch {
				case err == nil:
					served.Add(1)
				case connect.CodeOf(err) == connect.CodeUnavailable:
					refused.Add(1)
				default:
					mu.Lock()
					bad = append(bad, err.Error())
					mu.Unlock()
				}
			}
		})
	}
	for i := range cycles {
		if err := await(t, "a pause", pauseIn(ls, pauseOptions{For: fmt.Sprintf("cycle %d", i), Drain: shortWait}, instant)); err != nil {
			t.Errorf("cycle %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	if len(bad) > 0 {
		t.Errorf("reads neither served nor refused as paused: %q", bad)
	}
	if served.Load() == 0 {
		t.Error("no read was served")
	}
	for _, id := range ids {
		wantStored(t, ls, id)
	}
	for range uploads {
		await(t, "a finalize answer", answers)
	}
	for _, e := range logs.All() {
		if strings.Contains(fmt.Sprint(e.ContextMap()), "session is closed") {
			t.Errorf("%s: %v", e.Message, e.ContextMap())
		}
	}
	if n := evidenceCount(t, ls.db()); n != uploads {
		t.Errorf("%d evidence records, want %d", n, uploads)
	}
	if _, ok := engine.For(ls.db()).Layout(); !ok {
		t.Error("the database the last resume opened is not registered")
	}
	if g := gen(ls); g != cycles+1 {
		t.Errorf("gen %d after %d pauses, want %d", g, cycles, cycles+1)
	}
}
