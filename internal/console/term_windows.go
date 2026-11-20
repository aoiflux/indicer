//go:build windows

package console

import (
	"os"
	"slices"

	"golang.org/x/sys/windows"
)

// sameTerminal is true on Windows once both streams are consoles: a process
// has one console, so they share it.
func sameTerminal() bool { return true }

// EnableVT turns on escape-sequence processing for stdout and stderr where
// they are consoles, so colour and cursor movement work in the Windows
// console host. outOK and errOK are false when a console refused (older
// than Windows 10 1511); DUES then writes no escapes to that stream. A
// stream that is not a console is OK as it is. restore puts back only the
// modes it changed.
//
// Main calls it; nothing calls it from init, so a library build (the FFI
// DLL) leaves its host's console alone.
func EnableVT() (restore func(), outOK, errOK bool) {
	var undo []func()
	enable := func(f *os.File) bool {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) != nil {
			return true
		}
		want := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.ENABLE_PROCESSED_OUTPUT
		if want == mode {
			return true
		}
		if windows.SetConsoleMode(h, want) != nil {
			return false
		}
		undo = append(undo, func() { _ = windows.SetConsoleMode(h, mode) })
		return true
	}
	outOK = enable(os.Stdout)
	errOK = enable(os.Stderr)
	return func() {
		for _, f := range slices.Backward(undo) {
			f()
		}
	}, outOK, errOK
}
