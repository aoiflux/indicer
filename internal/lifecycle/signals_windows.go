//go:build windows

package lifecycle

import (
	"os"
	"syscall"
)

// Go delivers Ctrl-C and Ctrl-Break as os.Interrupt, and closing the console,
// logoff and shutdown as SIGTERM.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// signalName says what a signal did to the process, in the terms of the
// console event Go delivered it for.
func signalName(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "interrupted (Ctrl-C or Ctrl-Break)"
	case syscall.SIGTERM:
		return "stopped (SIGTERM): the console closed, or Windows is logging off or shutting down"
	}
	return "stopped by signal " + sig.String()
}

// CodeForSignal returns the exit code Unix shells would report, so scripts
// see the same codes on every platform.
func CodeForSignal(sig os.Signal) int {
	if sig == syscall.SIGTERM {
		return ExitTerminated
	}
	return ExitInterrupt
}
