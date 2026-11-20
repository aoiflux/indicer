//go:build windows

package fsx

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile locks the largest representable byte range, the documented idiom
// for a whole file that may be empty.
func lockFile(f *os.File, shared bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, ^uint32(0), ^uint32(0), new(windows.Overlapped))
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, ^uint32(0), ^uint32(0), new(windows.Overlapped))
}

// isLockContended: LOCKFILE_FAIL_IMMEDIATELY reports a held range as
// ERROR_LOCK_VIOLATION; ERROR_SHARING_VIOLATION means another process opened
// the file with an incompatible sharing mode. Both mean busy.
func isLockContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func isReadOnlyFS(err error) bool { return errors.Is(err, windows.ERROR_WRITE_PROTECT) }
