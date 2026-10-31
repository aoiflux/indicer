//go:build !windows

package console

import "os"

// sameTerminal is true when stdout and stderr are the same terminal device.
func sameTerminal() bool {
	a, errA := os.Stdout.Stat()
	b, errB := os.Stderr.Stat()
	return errA == nil && errB == nil && os.SameFile(a, b)
}

// EnableVT is a no-op outside Windows: terminals interpret escapes as they
// are.
func EnableVT() (restore func(), outOK, errOK bool) {
	return func() {}, true, true
}
