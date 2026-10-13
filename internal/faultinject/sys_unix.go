//go:build faultinject && !windows

package faultinject

import (
	"os"
	"syscall"
)

var (
	errNoSpace error = syscall.ENOSPC
	errIO      error = syscall.EIO
	errAccess  error = syscall.EACCES
)

func signalSelf(name string) error {
	sig := syscall.SIGINT
	if name == "TERM" {
		sig = syscall.SIGTERM
	}
	return syscall.Kill(os.Getpid(), sig)
}
