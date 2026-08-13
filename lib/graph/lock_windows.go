//go:build windows

package graph

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes a whole-file advisory lock without blocking.
//
// LockFileEx locks a byte range, and a zero-length range locks nothing, so the
// range passed here is the maximum representable one — the documented idiom for
// "the whole file" when the file may be empty, which this one always is.
func lockFile(file *os.File, shared bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if !shared {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	overlapped := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		flags,
		0,          // reserved
		^uint32(0), // low-order length
		^uint32(0), // high-order length
		overlapped,
	)
}

func unlockFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		^uint32(0),
		^uint32(0),
		overlapped,
	)
}

// isLockContended distinguishes "someone else holds this" from a real failure.
//
// ERROR_LOCK_VIOLATION is what LOCKFILE_FAIL_IMMEDIATELY returns for a
// contended range; ERROR_SHARING_VIOLATION shows up when another process has
// the file open with an incompatible sharing mode. Both mean busy, and neither
// should be reported as a broken store.
func isLockContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
