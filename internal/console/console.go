// Package console is the only way DUES code outside package main writes to
// the terminal.
//
// Library code prints through Out, Err and Progress (or the Print helpers),
// never os.Stdout or os.Stderr, so a host can redirect or silence all of it:
// the TUI silences it while it owns the screen, the FFI library keeps it off
// the host's stdout, and the qwikpak CLI keeps stdout for its JSON. A guard
// test enforces this.
//
// The package also owns fatih/color's output writers, so colored output
// follows the same configuration.
package console

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/fatih/color"
)

type sinks struct {
	out, err, progress io.Writer
}

var current atomic.Pointer[sinks]

// The defaults are color's own writers: stdout and stderr, made
// ANSI-capable on Windows consoles.
var defaultOut, defaultErr = color.Output, color.Error

func init() { set(&sinks{out: defaultOut, err: defaultErr, progress: defaultErr}) }

func set(s *sinks) {
	current.Store(s)
	color.Output, color.Error = s.out, s.err
}

// Out is where results go: listings, reports, anything a script may read.
func Out() io.Writer { return current.Load().out }

// Err is where notices, warnings and errors go.
func Err() io.Writer { return current.Load().err }

// Progress is where progress bars and spinners go.
func Progress() io.Writer { return current.Load().progress }

// Configure sends results to out and everything else to err. It must not
// run while other goroutines print: color's writers are plain variables.
func Configure(out, err io.Writer) {
	set(&sinks{out: out, err: err, progress: err})
}

// Quiet discards all terminal output until the returned function restores
// the previous configuration. Like Configure, it must not race with printing.
func Quiet() (restore func()) {
	prev := current.Load()
	set(&sinks{out: io.Discard, err: io.Discard, progress: io.Discard})
	return func() { set(prev) }
}

// Reset restores the default stdout/stderr configuration.
func Reset() { set(&sinks{out: defaultOut, err: defaultErr, progress: defaultErr}) }

// Print, Printf and Println write to Out.
func Print(a ...any)                 { fmt.Fprint(Out(), a...) }
func Printf(format string, a ...any) { fmt.Fprintf(Out(), format, a...) }
func Println(a ...any)               { fmt.Fprintln(Out(), a...) }

// Errorf and Errorln write to Err.
func Errorf(format string, a ...any) { fmt.Fprintf(Err(), format, a...) }
func Errorln(a ...any)               { fmt.Fprintln(Err(), a...) }
