package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
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

func TestContextStopReleases(t *testing.T) {
	ctx, stop := Context(context.Background())
	stop()
	stop()
	<-ctx.Done()
	if context.Cause(ctx) != context.Canceled {
		t.Fatalf("cause after stop = %v, want context.Canceled", context.Cause(ctx))
	}
}
