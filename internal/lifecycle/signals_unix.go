//go:build !windows

package lifecycle

import (
	"os"
	"syscall"
)

var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// CodeForSignal returns the conventional 128+n exit code for a signal.
func CodeForSignal(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return ExitInterrupt
}
