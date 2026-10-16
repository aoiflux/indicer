//go:build !windows

package fsx

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile uses flock rather than fcntl: flock belongs to the open file
// description, so an unrelated open and close of the same path elsewhere in
// the process cannot silently drop the lock.
func lockFile(f *os.File, shared bool) error {
	how := unix.LOCK_EX
	if shared {
		how = unix.LOCK_SH
	}
	return unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
}

func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }

// isLockContended separates "someone else holds it" from real failures.
// LOCK_NB reports contention as EWOULDBLOCK, which is EAGAIN on Linux.
func isLockContended(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

func isReadOnlyFS(err error) bool { return errors.Is(err, unix.EROFS) }
