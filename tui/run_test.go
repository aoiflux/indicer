package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// program runs Run on acts, reading its keys from the returned writer.
func program(t *testing.T, ctx context.Context, acts Actions) (keys io.Writer, done <-chan error) {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })
	ch := make(chan error, 1)
	go func() {
		ch <- Run(ctx, Config{Info: testInfo, Actions: acts, Options: []tea.ProgramOption{
			tea.WithInput(r), tea.WithOutput(io.Discard), tea.WithWindowSize(100, 30),
		}})
	}()
	return w, ch
}

func ended(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

// startStore opens the store screen, types a path and starts the store.
func startStore(t *testing.T, keys io.Writer, g *gate) {
	t.Helper()
	if _, err := io.WriteString(keys, "\rx\r"); err != nil {
		t.Fatal(err)
	}
	g.waitStarted(t)
}

// TestRunWaitsForJobs (TUI-3): when the command's context ends (a signal),
// Run returns its cause only once every job has stopped, so the database is
// never closed under one.
func TestRunWaitsForJobs(t *testing.T) {
	var stopped atomic.Bool
	g := newGate()
	acts := Actions{Store: func(ctx context.Context, _ string, _ StoreOptions) (Outcome, error) {
		err := g.wait(ctx)
		time.Sleep(100 * time.Millisecond) // finishing what it wrote
		stopped.Store(true)
		return Outcome{}, err
	}}
	signal := errors.New("interrupted")
	ctx, cancel := context.WithCancelCause(t.Context())
	keys, done := program(t, ctx, acts)
	startStore(t, keys, g)
	cancel(signal)
	if err := ended(t, done); !errors.Is(err, signal) {
		t.Fatalf("Run: %v, want %v", err, signal)
	}
	if !stopped.Load() {
		t.Fatal("Run returned before the store stopped")
	}
	if !errors.Is(g.ended(), signal) {
		t.Fatalf("the store ended with %v, want %v", g.ended(), signal)
	}
}

// TestRunLeavesAStuckJob: a job that does not stop within the drain timeout
// is ErrJobsRunning, naming it, and the caller leaves the database for
// recovery.
func TestRunLeavesAStuckJob(t *testing.T) {
	saved := drainTimeout
	drainTimeout = 100 * time.Millisecond
	t.Cleanup(func() { drainTimeout = saved })
	g := newGate()
	g.ignoreCancel = true
	t.Cleanup(func() { close(g.release) })

	ctx, cancel := context.WithCancel(t.Context())
	keys, done := program(t, ctx, Actions{Store: storeWith(g, Outcome{})})
	startStore(t, keys, g)
	cancel()
	err := ended(t, done)
	if !errors.Is(err, ErrJobsRunning) || !strings.Contains(err.Error(), "store x") {
		t.Fatalf("Run: %v, want %v naming the store", err, ErrJobsRunning)
	}
}

// TestRunQuitsThroughTheDialog: ctrl+c while a store runs asks; y stops the
// store, and the program ends once it has stopped.
func TestRunQuitsThroughTheDialog(t *testing.T) {
	g := newGate()
	keys, done := program(t, t.Context(), Actions{Store: storeWith(g, Outcome{})})
	startStore(t, keys, g)
	if _, err := io.WriteString(keys, "\x03"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("ctrl+c with a store running ended the program: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if g.ended() != nil {
		t.Fatalf("ctrl+c stopped the store before asking: %v", g.ended())
	}
	if _, err := io.WriteString(keys, "y"); err != nil {
		t.Fatal(err)
	}
	if err := ended(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !errors.Is(g.ended(), errStopping) {
		t.Fatalf("the store ended with %v, want %v", g.ended(), errStopping)
	}
}

// TestRunQuits: q on the menu ends the program at once.
func TestRunQuits(t *testing.T) {
	keys, done := program(t, t.Context(), Actions{})
	if _, err := io.WriteString(keys, "q"); err != nil {
		t.Fatal(err)
	}
	if err := ended(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
