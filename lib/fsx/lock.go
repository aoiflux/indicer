package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ErrLocked is returned, wrapped, when another process holds a lock.
var ErrLocked = errors.New("fsx: locked by another process")

// Lock is an advisory whole-file lock. It stops a second DUES process, not
// other programs.
//
// The lock belongs to the open file description, not to the process, so a
// second TryLock on the same path contends even within one process.
type Lock struct {
	f      *os.File
	path   string
	shared bool
}

// TryLock takes a lock on path without blocking, creating the file if needed.
// A shared lock admits other shared holders; an exclusive lock admits nobody.
// A held lock fails at once with an error wrapping ErrLocked.
//
// A shared lock on read-only media still works: when the file cannot be
// opened for writing, it is opened read-only, which is enough for a shared
// lock on every supported platform. The file must then already exist.
func TryLock(path string, shared bool) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil && shared && isReadOnlyErr(err) {
		f, err = os.Open(path)
	}
	if err != nil {
		return nil, fmt.Errorf("fsx: open lock %s: %w", path, err)
	}
	if err := lockFile(f, shared); err != nil {
		f.Close()
		if isLockContended(err) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("fsx: lock %s: %w", path, err)
	}
	return &Lock{f: f, path: path, shared: shared}, nil
}

// Path returns the locked file's path.
func (l *Lock) Path() string { return l.path }

// Shared reports whether the lock is shared.
func (l *Lock) Shared() bool { return l.shared }

// Unlock releases the lock. Calling it again does nothing.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	unlockErr := unlockFile(f)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("fsx: unlock %s: %w", l.path, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("fsx: close lock %s: %w", l.path, closeErr)
	}
	return nil
}

func isReadOnlyErr(err error) bool {
	return errors.Is(err, fs.ErrPermission) || isReadOnlyFS(err)
}

// stableLockTries bounds how often TryLockStable takes a lock again on a
// file that was replaced as it locked it.
const stableLockTries = 8

// TryLockStable is TryLock on a lock file that its holders remove when they
// finish. A lock taken on a file another process removed just before, and a
// third then made again, would let both hold "the" lock: the file locked
// must still be the one at path. A link at path is refused: whoever can
// write its folder could point it anywhere.
func TryLockStable(path string, shared bool) (*Lock, error) {
	for range stableLockTries {
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("fsx: lock %s: not a plain file", path)
		}
		l, err := TryLock(path, shared)
		if err != nil {
			return nil, err
		}
		held, herr := l.f.Stat()
		cur, cerr := os.Lstat(path)
		if herr == nil && cerr == nil && os.SameFile(held, cur) {
			return l, nil
		}
		_ = l.Unlock()
		switch {
		case herr != nil:
			return nil, fmt.Errorf("fsx: lock %s: %w", path, herr)
		case cerr != nil && !errors.Is(cerr, fs.ErrNotExist):
			return nil, fmt.Errorf("fsx: lock %s: %w", path, cerr)
		}
	}
	return nil, fmt.Errorf("fsx: lock %s: the file was replaced each time it was locked", path)
}
