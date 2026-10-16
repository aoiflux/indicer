// Package lifecycle owns the process-level context and exit codes.
//
// The root context is cancelled by the first interrupt or termination signal,
// with a SignalError as its cause, so every command can stop at a safe point,
// remove its temporary files and close the database. A second signal means the
// user does not want to wait, and exits at once.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
)

// Exit codes. Scripts and the FFI rely on these values; they never change.
const (
	ExitOK         = 0   // success
	ExitFailure    = 1   // the operation failed
	ExitUsage      = 2   // bad flags or arguments, or completed with warnings that need attention
	ExitInternal   = 70  // an internal error (recovered panic); see the crash file
	ExitInterrupt  = 130 // stopped by SIGINT or Ctrl-C/Ctrl-Break
	ExitTerminated = 143 // stopped by SIGTERM
)

// SignalError is the cancellation cause when a signal stops the process.
type SignalError struct{ Signal os.Signal }

func (e *SignalError) Error() string { return fmt.Sprintf("interrupted by %v", e.Signal) }

// UsageError marks an error caused by how DUES was invoked. It exits with
// ExitUsage.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// Usagef returns a UsageError.
func Usagef(format string, args ...any) error {
	return &UsageError{Err: fmt.Errorf(format, args...)}
}

// WarningError marks an operation that completed but needs the user's
// attention, such as evidence stored while its index is incomplete. It exits
// with ExitUsage, not ExitFailure, because the main work did succeed.
type WarningError struct{ Err error }

func (e *WarningError) Error() string { return e.Err.Error() }
func (e *WarningError) Unwrap() error { return e.Err }

// InternalError marks an internal failure such as a recovered panic.
type InternalError struct{ Err error }

func (e *InternalError) Error() string { return e.Err.Error() }
func (e *InternalError) Unwrap() error { return e.Err }

// forceExit ends the process on a second signal. Tests replace it.
var forceExit = os.Exit

// Context returns a context cancelled by the first interrupt or termination
// signal, and a stop function that releases the signal handlers. A second
// signal exits the process immediately with the matching code.
func Context(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, shutdownSignals...)

	done := make(chan struct{})
	var once sync.Once
	go func() {
		var first os.Signal
		for {
			select {
			case sig := <-ch:
				if first == nil {
					first = sig
					cancel(&SignalError{Signal: sig})
					continue
				}
				forceExit(CodeForSignal(sig))
				return
			case <-done:
				return
			}
		}
	}()
	return ctx, func() {
		once.Do(func() {
			signal.Stop(ch)
			close(done)
			cancel(nil)
		})
	}
}

// ExitCode maps the error a command returned to the process exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var sig *SignalError
	if errors.As(err, &sig) {
		return CodeForSignal(sig.Signal)
	}
	var internal *InternalError
	if errors.As(err, &internal) {
		return ExitInternal
	}
	var usage *UsageError
	var warning *WarningError
	if errors.As(err, &usage) || errors.As(err, &warning) {
		return ExitUsage
	}
	return ExitFailure
}

// Cause returns the reason ctx ended: the SignalError for a signal, or err
// unchanged. Commands return Cause(ctx, err) so a cancelled operation exits
// with the signal's code rather than ExitFailure.
func Cause(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var sig *SignalError
	if errors.As(context.Cause(ctx), &sig) && !errors.As(err, &sig) {
		return fmt.Errorf("%w: %w", sig, err)
	}
	return err
}
