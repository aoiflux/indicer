package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, ExitOK},
		{errors.New("boom"), ExitFailure},
		{Usagef("bad --chonksize %d", 0), ExitUsage},
		{fmt.Errorf("store: %w", &WarningError{Err: errors.New("index incomplete")}), ExitUsage},
		{&InternalError{Err: errors.New("panic")}, ExitInternal},
		{fmt.Errorf("restore: %w", &SignalError{Signal: os.Interrupt}), ExitInterrupt},
		{&SignalError{Signal: syscall.SIGTERM}, ExitTerminated},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestCauseAttachesSignal(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&SignalError{Signal: os.Interrupt})
	err := Cause(ctx, context.Canceled)
	if ExitCode(err) != ExitInterrupt || !errors.Is(err, context.Canceled) {
		t.Fatalf("Cause = %v (exit %d); want the signal attached and the original kept", err, ExitCode(err))
	}
	if Cause(ctx, nil) != nil {
		t.Fatal("Cause turned success into an error")
	}
	plain := errors.New("x")
	if Cause(context.Background(), plain) != plain {
		t.Fatal("Cause changed an error from an uncancelled context")
	}
}

// The message names the signal once, and leaves out a bare "context
// canceled", which says nothing the signal does not: an interrupted store
// used to end with "interrupted by interrupt: context canceled".
func TestCauseMessage(t *testing.T) {
	sig := &SignalError{Signal: os.Interrupt}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(sig)
	if got := Cause(ctx, context.Canceled).Error(); got != sig.Error() {
		t.Errorf("Cause(ctx, context.Canceled) = %q, want %q", got, sig.Error())
	}
	wrapped := fmt.Errorf("read chunk 3: %w", context.Canceled)
	err := Cause(ctx, wrapped)
	if want := sig.Error() + ": read chunk 3: context canceled"; err.Error() != want {
		t.Errorf("Cause(ctx, wrapped) = %q, want %q", err, want)
	}
	if !errors.Is(err, wrapped) || !errors.Is(err, context.Canceled) || ExitCode(err) != ExitInterrupt {
		t.Errorf("Cause(ctx, wrapped) lost the original error or the signal: %v", err)
	}
}

func TestSignalNames(t *testing.T) {
	interrupt := (&SignalError{Signal: os.Interrupt}).Error()
	if !strings.HasPrefix(interrupt, "interrupted") || strings.Contains(interrupt, "by interrupt") {
		t.Errorf("interrupt reads %q", interrupt)
	}
	if term := (&SignalError{Signal: syscall.SIGTERM}).Error(); !strings.Contains(term, "SIGTERM") {
		t.Errorf("SIGTERM reads %q", term)
	}
	for _, s := range shutdownSignals {
		if msg := (&SignalError{Signal: s}).Error(); msg == "" || strings.Contains(msg, "%!") {
			t.Errorf("signal %v reads %q", s, msg)
		}
	}
}

func TestContextStopReleases(t *testing.T) {
	ctx, stop := Context(context.Background())
	stop()
	stop()
	<-ctx.Done()
	if context.Cause(ctx) != context.Canceled {
		t.Fatalf("cause after stop = %v, want context.Canceled", context.Cause(ctx))
	}
}

func TestIsWarning(t *testing.T) {
	warning := &WarningError{Err: errors.New("index incomplete")}
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("boom"), false},
		{Usagef("bad flag"), false},
		{fmt.Errorf("store: %w", warning), true},
		{errors.Join(warning, Usagef("bad flag")), false},
		{errors.Join(warning, &SignalError{Signal: os.Interrupt}), false},
		{errors.Join(warning, &InternalError{Err: errors.New("panic")}), false},
	}
	for _, c := range cases {
		if got := IsWarning(c.err); got != c.want {
			t.Errorf("IsWarning(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestForceExitRunsHooks: a second signal runs the hooks before the process
// exits with the signal's code, and a hook that blocks does not keep the
// process alive past the budget.
func TestForceExitRunsHooks(t *testing.T) {
	var code int
	exited := make(chan struct{})
	prevExit, prevBudget := forceExit, forceExitBudget
	forceExit = func(c int) { code = c; close(exited) }
	forceExitBudget = 50 * time.Millisecond
	t.Cleanup(func() { forceExit, forceExitBudget = prevExit, prevBudget })

	ran := make(chan struct{})
	removeRan := OnForceExit(func() { close(ran) })
	defer removeRan()
	block := make(chan struct{})
	defer close(block)
	removeSlow := OnForceExit(func() { <-block })
	defer removeSlow()

	ch := make(chan os.Signal, 2)
	ctx, cancel := context.WithCancelCause(context.Background())
	go watch(ch, cancel, make(chan struct{}))
	ch <- os.Interrupt
	ch <- os.Interrupt
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("a blocking hook kept the process from exiting")
	}
	if code != ExitInterrupt {
		t.Fatalf("exit code %d, want %d", code, ExitInterrupt)
	}
	var sig *SignalError
	if !errors.As(context.Cause(ctx), &sig) {
		t.Fatalf("first signal did not cancel with a SignalError: %v", context.Cause(ctx))
	}
	select {
	case <-ran:
		t.Fatal("a hook registered earlier ran before the blocking later one")
	default:
	}
}

func TestForceExitHookOrder(t *testing.T) {
	var got []string
	removeA := OnForceExit(func() { got = append(got, "a") })
	defer removeA()
	removeGone := OnForceExit(func() { got = append(got, "gone") })
	removeGone()
	removeB := OnForceExit(func() { got = append(got, "b") })
	defer removeB()
	runForceExitHooks()
	if strings.Join(got, ",") != "b,a" {
		t.Fatalf("hooks ran %v, want b then a", got)
	}
}
