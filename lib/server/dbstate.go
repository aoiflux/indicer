package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"indicer/lib/dbopen"
	"indicer/lib/ingest"

	"connectrpc.com/connect"
	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// DefaultDrainTimeout bounds how long a pause waits for the uploads being
// stored before it cuts them.
const DefaultDrainTimeout = 5 * time.Minute

const (
	// pausedRetryAfter is the Retry-After, in seconds, of a refusal while the
	// database is paused: each refusal is cheap, and a pause may be short.
	pausedRetryAfter = "5"
	// busyRetryAfter answers a full request slot, which frees in moments.
	busyRetryAfter = "1"
	msgPaused      = "the database is paused for an export; retry later"

	headerRetryAfter   = "Retry-After"
	headerCacheControl = "Cache-Control"
	// cacheNoStore keeps a proxy from answering later requests with a
	// refusal or a state that has since changed.
	cacheNoStore = "no-store"

	bodyReady    = "ready\n"
	bodyNotReady = "not ready\n"
	bodyPaused   = "paused\n"

	logFieldPauseFor = "pause_for"

	// The stages a pause is given up at.
	stageUploads  = "uploads"
	stageRequests = "requests"
	stageQuiesced = "quiesced"
	stageSuspend  = "suspend"
)

var (
	// errPaused is the cause of every context a pause cuts.
	errPaused     = errors.New("the database is paused for an export")
	errPausing    = errors.New("the database is already paused")
	errPauseStuck = errors.New("work on the database did not stop")
	// A pause waits for every lease to end, its caller's among them.
	errPauseUnderLease = errors.New("server: a pause asked under a lease would wait for itself")
)

// dbRef is one open period of the database. It is never changed once
// published; the handle stays with dbState, as Suspend and Resume rewrite it.
type dbRef struct {
	db   *badger.DB
	sess *ingest.Session
	gen  uint64
}

type leaseKey struct{}

// leased is what a lease's context carries.
type leased struct {
	ref     *dbRef
	release func()
}

// refOf is the database a lease holds; nil outside a lease.
func refOf(ctx context.Context) *dbRef {
	if l, _ := ctx.Value(leaseKey{}).(*leased); l != nil {
		return l.ref
	}
	return nil
}

// releaseLease lets a pause go on once the request is done with the
// database but before its response is written, which a client that reads
// slowly could hold up for as long as it likes. The context stays live.
func releaseLease(ctx context.Context) {
	if l, _ := ctx.Value(leaseKey{}).(*leased); l != nil {
		l.release()
	}
}

// dbState owns the server's database: the requests using it hold leases, and
// a pause closes it between them.
type dbState struct {
	base context.Context
	h    *dbopen.Handle // only under op
	op   sync.Mutex     // one pause, or the final close
	gen  uint64         // under op
	ref  atomic.Pointer[dbRef]
	// suspendedAt is when the last pause suspended the database; under op.
	suspendedAt time.Time

	mu     sync.Mutex
	paused bool
	// cut ends this open period's leases; it is a child of base, so a stop
	// ends them too.
	cut       context.Context
	cutLeases context.CancelCauseFunc
	leases    tracker

	// Seams for tests.
	suspend func(*dbopen.Handle) (*dbopen.Suspended, error)
	resume  func(*dbopen.Suspended, context.Context, dbopen.ResumeOptions) (*dbopen.Handle, error)
}

func newDBState(base context.Context, h *dbopen.Handle) *dbState {
	d := &dbState{base: base, h: h, suspend: (*dbopen.Handle).Suspend, resume: (*dbopen.Suspended).Resume}
	d.cut, d.cutLeases = context.WithCancelCause(base)
	d.publish()
	return d
}

// current is the open database; nil while it is suspended or closed.
func (d *dbState) current() *dbRef { return d.ref.Load() }

// publish starts an open period on h.DB, which Resume replaces. Its session
// is new: a closed one refuses every call.
func (d *dbState) publish() {
	d.gen++
	d.ref.Store(&dbRef{db: d.h.DB, sess: ingest.NewSession(d.h.DB), gen: d.gen})
}

// lease admits a request to the database unless it is paused. The context
// it returns carries the database, and ends when a pause cuts its leases or
// the server stops; end must be called once the request is done with it,
// even after releaseLease.
func (d *dbState) lease(ctx context.Context) (context.Context, func(), bool) {
	d.mu.Lock()
	if d.paused {
		d.mu.Unlock()
		return nil, nil, false
	}
	d.leases.add()
	cut, ref := d.cut, d.ref.Load()
	d.mu.Unlock()
	var stop func() bool
	l := &leased{ref: ref}
	l.release = sync.OnceFunc(func() {
		stop()
		d.leases.done()
	})
	lctx, cancel := context.WithCancelCause(context.WithValue(ctx, leaseKey{}, l))
	stop = context.AfterFunc(cut, func() { cancel(context.Cause(cut)) })
	return lctx, func() {
		l.release()
		cancel(nil)
	}, true
}

func (d *dbState) isPaused() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused
}

func (d *dbState) shut() {
	d.mu.Lock()
	d.paused = true
	d.mu.Unlock()
}

// open lets leases in again. The leases a pause did not cut keep their
// context, so the cut is renewed only when it was used.
func (d *dbState) open() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused = false
	if d.cut.Err() != nil {
		d.cut, d.cutLeases = context.WithCancelCause(d.base)
	}
}

// cutAll ends the leases running, with cause.
func (d *dbState) cutAll(cause error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cutLeases(cause)
}

// close closes the session and the database, or releases it when a pause
// left it suspended. It runs once the server is idle.
func (d *dbState) close(log *zap.Logger) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.shut()
	if ref := d.ref.Swap(nil); ref != nil {
		if err := ref.sess.Close(); err != nil {
			log.Warn("upload ingest session not closed", zap.Error(err))
		}
	}
	return d.h.Close()
}

// pauseOptions say what a pause is for and how long it waits.
type pauseOptions struct {
	// For names the pause in the log, e.g. "export <id>".
	For string
	// Drain bounds the wait for the uploads being stored; 0 is
	// DefaultDrainTimeout.
	Drain time.Duration
	// Quiesced, when set, runs on the open, idle database just before it is
	// suspended; an error gives the pause up.
	Quiesced func(ctx context.Context, db *badger.DB) error
}

// whilePaused suspends the database, calls use on it, and reopens it. It
// runs as server work, never under a lease. In order: the queue stops
// admitting uploads, and those being stored get opts.Drain, then are cut
// and requeued; the gate then refuses new leases, and those running get
// PauseLeaseWait, then are cut. Reads are thus refused for at most
// PauseLeaseWait plus IdleWait before the suspend. Work that will not stop
// gives the pause up with errPauseStuck, and the server goes on as before.
func (s *Server) whilePaused(ctx context.Context, opts pauseOptions, use func(*dbopen.Suspended) error) error {
	if refOf(ctx) != nil {
		return errPauseUnderLease
	}
	d, q, lim := s.dbs, s.uploads.queue, s.cfg.Limits
	if !d.op.TryLock() {
		return errPausing
	}
	defer d.op.Unlock()
	// No open database: a resume gave up, and the server is stopping.
	if s.draining.Load() || d.current() == nil {
		return errStopping
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	start := time.Now()
	drain := cmp.Or(opts.Drain, DefaultDrainTimeout)
	running, waiting := q.counts()
	s.log.Info("server pause started", zap.String(logFieldPauseFor, opts.For), zap.Int64("drain_ms", drain.Milliseconds()),
		zap.Int("uploads_storing", running), zap.Int("uploads_waiting", waiting), zap.Int("requests", d.leases.count()))
	q.pause()

	ok := q.runs.wait(ctx, drain)
	if ctx.Err() != nil {
		return s.abortPause(opts, stageUploads, false, context.Cause(ctx))
	}
	uploadsCut := 0
	if !ok {
		uploadsCut = q.cancelRunning(errPaused)
		s.log.Warn("server pause cancelled uploads", zap.String(logFieldPauseFor, opts.For), zap.Int("count", uploadsCut))
		ok = q.runs.wait(ctx, lim.IdleWait)
		if ctx.Err() != nil {
			return s.abortPause(opts, stageUploads, false, context.Cause(ctx))
		}
		if !ok {
			running, _ := q.counts()
			return s.abortPause(opts, stageUploads, false, fmt.Errorf("%w: %d uploads being stored", errPauseStuck, running))
		}
	}

	// The gate shuts only now, so reads go on while uploads drain.
	d.shut()
	ok = d.leases.wait(ctx, lim.PauseLeaseWait)
	if ctx.Err() != nil {
		return s.abortPause(opts, stageRequests, false, context.Cause(ctx))
	}
	requestsCut := 0
	if !ok {
		requestsCut = d.leases.count()
		d.cutAll(errPaused)
		s.log.Warn("server pause cancelled requests", zap.String(logFieldPauseFor, opts.For), zap.Int("count", requestsCut))
		ok = d.leases.wait(ctx, lim.IdleWait)
		if ctx.Err() != nil {
			return s.abortPause(opts, stageRequests, false, context.Cause(ctx))
		}
		if !ok {
			return s.abortPause(opts, stageRequests, false, fmt.Errorf("%w: %d requests", errPauseStuck, d.leases.count()))
		}
	}

	// Only slot holders call the session, and none runs, so Close does not
	// wait. Suspend refuses a database whose stores it left open.
	ref := d.current()
	if err := ref.sess.Close(); err != nil {
		s.log.Warn("upload ingest session not closed", zap.String(logFieldPauseFor, opts.For), zap.Error(err))
	}
	if opts.Quiesced != nil {
		if err := opts.Quiesced(ctx, ref.db); err != nil {
			return s.abortPause(opts, stageQuiesced, true, err)
		}
	}
	d.ref.Store(nil)
	held, err := d.suspend(d.h)
	switch {
	case held == nil:
		return s.abortPause(opts, stageSuspend, true, err)
	case err != nil:
		// Snapshot refuses a database that did not close cleanly, so use is
		// not called; Resume recovers it.
		d.suspendedAt = time.Now()
		return errors.Join(err, s.resumeDB(opts, held, true))
	}
	d.suspendedAt = time.Now()
	s.log.Info("server paused", zap.String(logFieldPauseFor, opts.For), zap.Uint64("gen", ref.gen),
		zap.Int64("wait_ms", time.Since(start).Milliseconds()), zap.Int("uploads_cut", uploadsCut), zap.Int("requests_cut", requestsCut))
	// A cut upload may have been cut in its alias cleanup, which recovery
	// finishes.
	return errors.Join(use(held), s.resumeDB(opts, held, uploadsCut > 0))
}

// abortPause gives a pause up before the database was suspended, and lets
// the work it held back go on. Under op. The gate may not have been shut.
func (s *Server) abortPause(opts pauseOptions, stage string, sessClosed bool, cause error) error {
	d := s.dbs
	if sessClosed {
		d.publish()
	}
	d.open()
	s.uploads.queue.resume()
	s.log.Error("server pause given up", zap.String(logFieldPauseFor, opts.For), zap.String("stage", stage), zap.Error(cause))
	return fmt.Errorf("server: pause given up at %s: %w", stage, cause)
}

// resumeDB reopens the suspended database, trying again after each of
// Limits.ResumeRetry. Under op. Uploads still arrive while it is suspended,
// so their part files are kept. When every attempt fails, the server stops;
// when the server stops first, the database stays suspended for close to
// release.
func (s *Server) resumeDB(opts pauseOptions, held *dbopen.Suspended, recoverIngests bool) error {
	d, lim := s.dbs, s.cfg.Limits
	ropts := dbopen.ResumeOptions{Recover: recoverIngests, KeepUploads: true}
	waits := append([]time.Duration{0}, lim.ResumeRetry[:]...)
	var err error
	for i, wait := range waits {
		if werr := sleepCtx(s.base, wait); werr != nil {
			return werr
		}
		h, rerr := d.resume(held, s.base, ropts)
		if rerr == nil {
			d.h = h
			d.publish()
			d.open()
			s.uploads.queue.resume()
			s.log.Info("server resumed", zap.String(logFieldPauseFor, opts.For), zap.Uint64("gen", d.gen),
				zap.Int64("paused_ms", time.Since(d.suspendedAt).Milliseconds()), zap.Int("attempts", i+1))
			return nil
		}
		if s.base.Err() != nil {
			return context.Cause(s.base)
		}
		err = rerr
		s.log.Warn("server resume failed", zap.String(logFieldPauseFor, opts.For), zap.Int("attempt", i+1), zap.Error(rerr))
	}
	err = fmt.Errorf("server: the database could not be reopened after %d attempts: %w", len(waits), err)
	s.log.Error("server resume given up", zap.String(logFieldPauseFor, opts.For), zap.Error(err))
	s.fail(err)
	return err
}

// sleepCtx waits d, or until ctx ends, and then returns ctx's cause if it
// ended.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

// refusePaused answers a request for the database while it is paused. An
// RPC route answers with a Connect error, which every RPC client reads.
func (s *Server) refusePaused(w http.ResponseWriter, r *http.Request, rt route) {
	w.Header().Set(headerRetryAfter, pausedRetryAfter)
	w.Header().Set(headerCacheControl, cacheNoStore)
	if rt.rpc {
		_ = s.rpcErr.Write(w, r, connect.NewError(connect.CodeUnavailable, errors.New(msgPaused)))
		return
	}
	httpError(w, r, http.StatusServiceUnavailable, msgPaused)
}

// fail stops the server with err; Serve returns it. Only the first is kept.
func (s *Server) fail(err error) {
	select {
	case s.fatal <- err:
	default:
	}
}
