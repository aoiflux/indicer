//go:build !windows

package lifecycle

import (
	"os"
	"syscall"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// signalName says what a signal did to the process, with its usual name.
func signalName(sig os.Signal) string {
	switch sig {
	case os.Interrupt:
		return "interrupted (SIGINT)"
	case syscall.SIGTERM:
		return "terminated (SIGTERM)"
	case syscall.SIGHUP:
		return "stopped: the terminal closed (SIGHUP)"
	}
	return "stopped by signal " + sig.String()
}

// CodeForSignal returns the conventional 128+n exit code for a signal.
func CodeForSignal(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return ExitInterrupt
}
