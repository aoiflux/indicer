// Package console is the only way DUES code outside package main writes to
// the terminal.
//
// Library code prints through Out, Err and Progress (or the Print helpers),
// never os.Stdout or os.Stderr, so a host can redirect or silence all of it:
// the TUI silences it while it owns the screen, the FFI library keeps it off
// the host's stdout, and the qwikpak CLI keeps stdout for its JSON. A guard
// test enforces this.
//
// Colour and layout are decided in internal/ui, which writes through this
// package too.
package console

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

type sinks struct {
	out, err, progress io.Writer
}

var current atomic.Pointer[sinks]

// The defaults are the process's stdout and stderr. Escape processing on
// Windows consoles is switched on by ui.Setup (EnableVT), not here: a
// library build must not change its host's console.
var defaultOut, defaultErr io.Writer = os.Stdout, os.Stderr

func init() { set(&sinks{out: defaultOut, err: defaultErr, progress: defaultErr}) }

func set(s *sinks) { current.Store(s) }

// Out is where results go: listings, reports, anything a script may read.
func Out() io.Writer { return current.Load().out }

// Err is where notices, warnings and errors go.
func Err() io.Writer { return current.Load().err }

// Progress is where progress bars and spinners go.
func Progress() io.Writer { return current.Load().progress }

// Configure sends results to out and everything else to err.
func Configure(out, err io.Writer) {
	set(&sinks{out: out, err: err, progress: err})
}

// Quiet discards all terminal output until the returned function restores
// the previous configuration.
func Quiet() (restore func()) {
	prev := current.Load()
	set(&sinks{out: io.Discard, err: io.Discard, progress: io.Discard})
	return func() { set(prev) }
}

// Wrap replaces the result and notice sinks with what f makes of the
// current ones, until restore runs. The progress sink stays as it is: the
// progress display wraps the other two so that their lines and its redraws
// do not garble each other on one terminal.
func Wrap(f func(out, err io.Writer) (io.Writer, io.Writer)) (restore func()) {
	prev := current.Load()
	out, err := f(prev.out, prev.err)
	set(&sinks{out: out, err: err, progress: prev.progress})
	return func() { set(prev) }
}

// ErrSink writes to whatever Err is at the moment of each write, for
// writers set up once, such as the log's last-resort warnings.
var ErrSink io.Writer = errSink{}

type errSink struct{}

func (errSink) Write(p []byte) (int, error) { return Err().Write(p) }

// Reset restores the default stdout/stderr configuration.
func Reset() { set(&sinks{out: defaultOut, err: defaultErr, progress: defaultErr}) }

// Print, Printf and Println write to Out.
func Print(a ...any)                 { fmt.Fprint(Out(), a...) }
func Printf(format string, a ...any) { fmt.Fprintf(Out(), format, a...) }
func Println(a ...any)               { fmt.Fprintln(Out(), a...) }

// Errorf and Errorln write to Err.
func Errorf(format string, a ...any) { fmt.Fprintf(Err(), format, a...) }
func Errorln(a ...any)               { fmt.Fprintln(Err(), a...) }
