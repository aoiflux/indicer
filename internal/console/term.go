package console

import (
	"os"
	"strconv"
	"sync/atomic"

	"golang.org/x/term"
)

// Terminal says which standard streams are terminals. It holds facts only;
// what DUES shows on them is decided in internal/ui.
type Terminal struct {
	InTTY, OutTTY, ErrTTY bool
	// Shared is true when stdout and stderr are the same terminal, so text
	// written to one lands between the lines drawn on the other.
	Shared bool
}

// Detect reports the standard streams' terminal state now. A character
// device is not enough: the null device is one.
func Detect() Terminal {
	t := Terminal{
		InTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		OutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		ErrTTY: term.IsTerminal(int(os.Stderr.Fd())),
	}
	t.Shared = t.OutTTY && t.ErrTTY && sameTerminal()
	return t
}

// Size is the size of the terminal on stderr in columns and rows. It falls
// back to $COLUMNS and $LINES, then to 80x24, so a caller always gets a
// usable layout.
func Size() (width, height int) {
	w, h, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		w, h = envInt("COLUMNS", 80), envInt("LINES", 24)
	}
	return w, h
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

var nonInteractive atomic.Bool

// SetNonInteractive makes Interactive report false whatever stdin is. --ci
// uses it: DUES then behaves as if no terminal were attached, so prompts
// refuse as they do on a pipe.
func SetNonInteractive(v bool) { nonInteractive.Store(v) }

// Interactive reports whether DUES may prompt on stdin: stdin is a terminal
// and --ci is off.
func Interactive() bool {
	return !nonInteractive.Load() && term.IsTerminal(int(os.Stdin.Fd()))
}
