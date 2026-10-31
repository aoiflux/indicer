package ui

import (
	"runtime"
	"sync/atomic"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
)

var current atomic.Pointer[Mode]

// Before Setup (tests, the FFI library, the compression product) nothing
// is a terminal: no colour, no motion, no fun.
func init() { current.Store(&Mode{}) }

// Current is the Mode decided by Setup.
func Current() Mode { return *current.Load() }

// Setup decides what this process shows, from the flags PreScan found and
// the environment (os.Environ form). Main calls it before anything prints;
// the returned function undoes it on the way out.
func Setup(o Options, env []string) (teardown func()) {
	prev := current.Load()
	m := Resolve(o, env, console.Detect(), runtime.GOOS)
	restoreVT := func() {}
	if m.Out.TTY || m.Err.TTY {
		var outOK, errOK bool
		restoreVT, outOK, errOK = console.EnableVT()
		if !outOK {
			m.Out = noEscapes(m.Out)
		}
		if !errOK {
			m.Err = noEscapes(m.Err)
			m.Fun, m.Taskbar = false, false
		}
	}
	console.SetNonInteractive(m.CI)
	current.Store(&m)
	return func() {
		// A display that could not stop (a panic in a worker) may have left
		// progress on the tab.
		if taskbarShown.Swap(false) {
			_, _ = console.Progress().Write([]byte(osc94(tbClear, 0)))
		}
		restoreVT()
		console.SetNonInteractive(false)
		current.Store(prev)
	}
}

// noEscapes is a stream whose console refused escape processing.
func noEscapes(c Caps) Caps {
	c.Profile = colorprofile.NoTTY
	c.Motion = false
	return c
}

// SetForTest makes m current until restore runs.
func SetForTest(m Mode) (restore func()) {
	prev := current.Swap(&m)
	return func() { current.Store(prev) }
}
