//go:build !windows

package graph

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes a whole-file advisory lock without blocking.
//
// flock is used rather than fcntl because its ownership is the open file
// description, not the process: a stray open/close of the same path elsewhere
// in the process cannot silently drop a lock this package still believes it
// holds. The tradeoff is that flock does not work on some network filesystems,
// which is acceptable — a graph store on NFS has worse problems.
func lockFile(file *os.File, shared bool) error {
	how := unix.LOCK_EX
	if shared {
		how = unix.LOCK_SH
	}
	return unix.Flock(int(file.Fd()), how|unix.LOCK_NB)
}

func unlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

// isLockContended distinguishes "someone else holds this" from a real failure.
// LOCK_NB reports contention as EWOULDBLOCK, which is EAGAIN on Linux; both are
// compared so the check holds wherever they differ.
func isLockContended(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

// isReadOnlyMedia reports whether err says the file cannot be opened for
// writing: a read-only mount, or file modes that forbid it.
func isReadOnlyMedia(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, unix.EROFS)
}
