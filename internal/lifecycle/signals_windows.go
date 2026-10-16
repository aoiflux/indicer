//go:build windows

package lifecycle

import (
	"os"
	"syscall"
)

// Go delivers Ctrl-C and Ctrl-Break as os.Interrupt, and closing the console,
// logoff and shutdown as SIGTERM.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// CodeForSignal returns the exit code Unix shells would report, so scripts
// see the same codes on every platform.
func CodeForSignal(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return ExitTerminated
	}
	return ExitInterrupt
}
