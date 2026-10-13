//go:build faultinject && windows

package faultinject

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

var (
	errNoSpace error = windows.ERROR_DISK_FULL
	errIO      error = windows.ERROR_IO_DEVICE
	errAccess  error = windows.ERROR_ACCESS_DENIED
)

// signalSelf raises a Ctrl-Break in the process's own console group. The
// process must have been started with CREATE_NEW_PROCESS_GROUP (testutil's
// StartChild does this), or the event would also reach the parent.
func signalSelf(name string) error {
	if name == "TERM" {
		return errors.New("SIGTERM has no Windows equivalent; use signal:INT")
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(os.Getpid()))
}
