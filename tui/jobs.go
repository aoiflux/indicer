package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"indicer/internal/safe"

	tea "charm.land/bubbletea/v2"
)

// A job is one action or loader the TUI runs off its event loop. Each runs on
// its own goroutine under internal/safe, with a context derived from the
// command's, so cancelling the job, a signal, or the TUI stopping ends it.
//
// A reset runs alone: it starts only when no other job runs, and no job
// starts while it runs. Other jobs run side by side, as the engine allows: a
// long store does not lock the list or the statistics.
//
// bubbletea does not wait for the commands it runs when the program ends
// (TUI-3). The runner is what Run waits for before the database is closed.

type jobKind int

const (
	sharedJob jobKind = iota
	// loaderJob is a shared job that reads what a screen shows; the menu
	// reports only its failure.
	loaderJob
	exclusiveJob
)

// ErrJobsRunning is returned by Run when a job had not stopped by the time
// Run gave up waiting for it. The database must not be closed under it.
var ErrJobsRunning = errors.New("a TUI job was still running when the TUI ended")

// Causes a job's context can end with, besides the command's own.
var (
	errCanceled = errors.New("cancelled")
	errStopping = errors.New("the TUI is stopping")
	errClosed   = errors.New("the TUI is closing")
)

// busyError: a job could not start because of the jobs already running.
type busyError struct {
	reset   bool     // the job asked for is a reset
	running []string // names of the running jobs
}

func (e *busyError) Error() string {
	list := strings.Join(e.running, ", ")
	if e.reset {
		return fmt.Sprintf("a reset cannot start while other jobs run (%s); wait for them, or cancel them with ctrl+x on the menu", list)
	}
	return fmt.Sprintf("a reset is running (%s); wait for it to finish", list)
}

type job struct {
	id     int
	name   string
	kind   jobKind
	since  time.Time
	cancel context.CancelCauseFunc
	done   chan struct{}
	// result and err are set before done is closed.
	result any
	err    error
}

// jobInfo describes a running job.
type jobInfo struct {
	id    int
	name  string
	since time.Time
}

type runner struct {
	base context.Context

	mu     sync.Mutex
	nextID int
	active map[int]*job
	closed bool
}

func newRunner(ctx context.Context) *runner {
	return &runner{base: ctx, active: map[int]*job{}}
}

// start runs fn as a job named name, or says why it cannot start.
func (r *runner) start(name string, kind jobKind, fn func(context.Context) (any, error)) (*job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	for _, j := range r.active {
		if kind == exclusiveJob || j.kind == exclusiveJob {
			return nil, &busyError{reset: kind == exclusiveJob, running: r.namesLocked()}
		}
	}
	ctx, cancel := context.WithCancelCause(r.base)
	r.nextID++
	j := &job{id: r.nextID, name: name, kind: kind, since: time.Now(), cancel: cancel, done: make(chan struct{})}
	r.active[j.id] = j
	go func() {
		res, err := safe.Value(safe.Site{Component: "tui", Op: name}, func() (any, error) { return fn(ctx) })
		if err != nil && ctx.Err() != nil && !safe.IsPanic(err) {
			// Cancelled: say why, not just that the context ended.
			err = context.Cause(ctx)
		}
		cancel(nil)
		r.mu.Lock()
		j.result, j.err = res, err
		delete(r.active, j.id)
		r.mu.Unlock()
		close(j.done)
	}()
	return j, nil
}

func (r *runner) namesLocked() []string {
	ids := make([]int, 0, len(r.active))
	for id := range r.active {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = r.active[id].name
	}
	return names
}

// cancel ends the job id, if it still runs.
func (r *runner) cancel(id int, cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j := r.active[id]; j != nil {
		j.cancel(cause)
	}
}

// cancelAll ends every running job.
func (r *runner) cancelAll(cause error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, j := range r.active {
		j.cancel(cause)
	}
}

// running lists the running jobs, oldest first.
func (r *runner) running() []jobInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]jobInfo, 0, len(r.active))
	for _, j := range r.active {
		out = append(out, jobInfo{id: j.id, name: j.name, since: j.since})
	}
	slices.SortFunc(out, func(a, b jobInfo) int { return a.id - b.id })
	return out
}

func (r *runner) busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active) > 0
}

// close refuses every job from now on.
func (r *runner) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

// wait waits until no job runs, for at most timeout, and reports whether
// none does.
func (r *runner) wait(timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		var next *job
		for _, j := range r.active {
			next = j
			break
		}
		r.mu.Unlock()
		if next == nil {
			return true
		}
		select {
		case <-next.done:
		case <-deadline.C:
			return !r.busy()
		}
	}
}

// jobDoneMsg reports a finished job to the program.
type jobDoneMsg struct {
	id     int
	name   string
	kind   jobKind
	result any
	err    error
}

// await is the command that reports j's end.
func await(j *job) tea.Cmd {
	return func() tea.Msg {
		<-j.done
		return jobDoneMsg{id: j.id, name: j.name, kind: j.kind, result: j.result, err: j.err}
	}
}
