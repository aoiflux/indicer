package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"indicer/internal/safe"
)

// startGate starts a job that runs until g releases it.
func startGate(t *testing.T, r *runner, name string, kind jobKind, g *gate) *job {
	t.Helper()
	j, err := r.start(name, kind, func(ctx context.Context) (any, error) { return nil, g.wait(ctx) })
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	g.waitStarted(t)
	return j
}

func finished(t *testing.T, j *job) {
	t.Helper()
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("job %s did not finish", j.name)
	}
}

// TestRunnerResetRunsAlone: a reset starts only when no job runs, and no job
// starts while it runs; other jobs run side by side.
func TestRunnerResetRunsAlone(t *testing.T) {
	t.Parallel()
	r := newRunner(t.Context())
	store, search := newGate(), newGate()
	js := startGate(t, r, "store a", sharedJob, store)
	startGate(t, r, "search b", sharedJob, search)
	if _, err := r.start("list", loaderJob, func(context.Context) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("a loader beside two jobs: %v", err)
	}

	var busy *busyError
	_, err := r.start("reset", exclusiveJob, func(context.Context) (any, error) {
		t.Error("the reset ran beside other jobs")
		return nil, nil
	})
	if !errors.As(err, &busy) || !busy.reset || !strings.Contains(err.Error(), "store a") || !strings.Contains(err.Error(), "search b") {
		t.Fatalf("a reset beside other jobs: %v, want a busy error naming them", err)
	}

	close(store.release)
	close(search.release)
	finished(t, js)
	if !r.wait(5 * time.Second) {
		t.Fatal("jobs did not finish")
	}
	reset := newGate()
	jr := startGate(t, r, "reset", exclusiveJob, reset)
	for _, kind := range []jobKind{sharedJob, loaderJob} {
		_, err := r.start("store c", kind, func(context.Context) (any, error) {
			t.Error("a job ran beside a reset")
			return nil, nil
		})
		if !errors.As(err, &busy) || !strings.Contains(err.Error(), "a reset is running") {
			t.Fatalf("a job (kind %d) beside a reset: %v, want a busy error", kind, err)
		}
	}
	if _, err := r.start("reset", exclusiveJob, func(context.Context) (any, error) {
		t.Error("a reset ran beside a reset")
		return nil, nil
	}); !errors.As(err, &busy) {
		t.Fatalf("a reset beside a reset: %v, want a busy error", err)
	}
	close(reset.release)
	finished(t, jr)
}

// TestRunnerCancelGivesCause: a cancelled job ends with the cause it was
// cancelled with, not just "context canceled".
func TestRunnerCancelGivesCause(t *testing.T) {
	t.Parallel()
	r := newRunner(t.Context())
	a, b, c := newGate(), newGate(), newGate()
	ja := startGate(t, r, "a", sharedJob, a)
	jb := startGate(t, r, "b", sharedJob, b)
	r.cancel(ja.id, errCanceled)
	finished(t, ja)
	if !errors.Is(ja.err, errCanceled) || errorText(ja.err) != "cancelled" {
		t.Fatalf("cancelled job: %v", ja.err)
	}
	select {
	case <-jb.done:
		t.Fatal("cancelling one job ended another")
	case <-time.After(20 * time.Millisecond):
	}
	jc := startGate(t, r, "c", sharedJob, c)
	r.cancelAll(errStopping)
	finished(t, jb)
	finished(t, jc)
	for _, j := range []*job{jb, jc} {
		if !errors.Is(j.err, errStopping) {
			t.Fatalf("job %s: %v, want %v", j.name, j.err, errStopping)
		}
	}
	if r.busy() {
		t.Fatal("the runner is busy after every job ended")
	}
}

// TestRunnerFollowsTheCommand: the command's context ending (a signal) ends
// every job with its cause.
func TestRunnerFollowsTheCommand(t *testing.T) {
	t.Parallel()
	signal := errors.New("interrupted")
	ctx, cancel := context.WithCancelCause(t.Context())
	r := newRunner(ctx)
	g := newGate()
	j := startGate(t, r, "store a", sharedJob, g)
	cancel(signal)
	finished(t, j)
	if !errors.Is(j.err, signal) || !errors.Is(g.ended(), signal) {
		t.Fatalf("job: %v (its context: %v), want %v", j.err, g.ended(), signal)
	}
}

// TestRunnerKeepsTheJobsOwnError: an error the job returned itself is kept.
func TestRunnerKeepsTheJobsOwnError(t *testing.T) {
	t.Parallel()
	r := newRunner(t.Context())
	failed := errors.New("disk full")
	j, err := r.start("store a", sharedJob, func(context.Context) (any, error) { return Outcome{Lines: []string{"partial"}}, failed })
	if err != nil {
		t.Fatal(err)
	}
	finished(t, j)
	if !errors.Is(j.err, failed) {
		t.Fatalf("job error: %v", j.err)
	}
	if o, _ := j.result.(Outcome); len(o.Lines) != 1 {
		t.Fatalf("job result: %#v, want the outcome it returned", j.result)
	}
}

// TestRunnerContainsPanics: a job that panics ends with a panic error, and
// the runner goes on.
func TestRunnerContainsPanics(t *testing.T) {
	t.Parallel()
	r := newRunner(t.Context())
	j, err := r.start("store a", sharedJob, func(context.Context) (any, error) { panic("boom") })
	if err != nil {
		t.Fatal(err)
	}
	finished(t, j)
	var pe *safe.PanicError
	if !errors.As(j.err, &pe) {
		t.Fatalf("job error: %v, want a panic error", j.err)
	}
	if text := errorText(j.err); !strings.Contains(text, pe.ID) || strings.Contains(text, "boom") {
		t.Fatalf("shown as %q: want the panic ID and not its value", text)
	}
	if r.busy() {
		t.Fatal("the runner is busy after the job panicked")
	}
	if _, err := r.start("store b", sharedJob, func(context.Context) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("a job after a panic: %v", err)
	}
}

// TestRunnerClosedRefusesJobs: once closed, nothing starts, so a command
// bubbletea runs after the program ended returns at once (TUI-3).
func TestRunnerClosedRefusesJobs(t *testing.T) {
	t.Parallel()
	r := newRunner(t.Context())
	r.close()
	_, err := r.start("store a", sharedJob, func(context.Context) (any, error) {
		t.Error("a job ran after the runner closed")
		return nil, nil
	})
	if !errors.Is(err, errClosed) {
		t.Fatalf("start after close: %v", err)
	}
	if errorText(err) != "stopped: the TUI is closing" {
		t.Fatalf("shown as %q", errorText(err))
	}
}

// TestRunnerWait: wait returns once every job ends, or reports the ones
// still running at its timeout.
func TestRunnerWait(t *testing.T) {
	t.Parallel()
	r := newRunner(t.Context())
	if !r.wait(time.Millisecond) {
		t.Fatal("wait with no job reported one running")
	}
	stuck := newGate()
	stuck.ignoreCancel = true
	startGate(t, r, "store a", sharedJob, stuck)
	r.cancelAll(errStopping)
	start := time.Now()
	if r.wait(50 * time.Millisecond) {
		t.Fatal("wait reported no job while one ignored its cancellation")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("wait took %v", took)
	}
	if got := r.running(); len(got) != 1 || got[0].name != "store a" {
		t.Fatalf("running: %v", got)
	}
	close(stuck.release)
	if !r.wait(5 * time.Second) {
		t.Fatal("wait did not return once the job ended")
	}
}

// TestAwaitReportsTheJob: the command a job comes with reports its end.
func TestAwaitReportsTheJob(t *testing.T) {
	t.Parallel()
	r := newRunner(t.Context())
	j, err := r.start("search a", sharedJob, func(context.Context) (any, error) { return Outcome{Lines: []string{"1 object"}}, nil })
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := await(j)().(jobDoneMsg)
	if !ok || msg.id != j.id || msg.name != "search a" || msg.err != nil {
		t.Fatalf("await: %#v", msg)
	}
	if o, _ := msg.result.(Outcome); len(o.Lines) != 1 {
		t.Fatalf("await result: %#v", msg.result)
	}
}
