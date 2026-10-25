package compression_product

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"indicer/lib/fsx"
)

const (
	tempWorkDirPattern = "duesarc-work-*"
	tempExtractPattern = "duesarc-extract-*"
	tempDirPrefix      = "duesarc-"
	// workLockName is the lock a work directory holds while in use, so the
	// sweep never removes one another process is working in.
	workLockName = ".qwikpak.lock"
	// staleWorkAge is how old an unlocked work directory must be before the
	// sweep removes it.
	staleWorkAge = 24 * time.Hour
)

// workDir is a directory an archive operation builds or reads a database in.
type workDir struct {
	path    string
	created bool
	lock    *fsx.Lock
}

// newTempWorkDir creates a locked work directory under the system's
// temporary directory. A stale one from a process that died is swept first.
func newTempWorkDir(pattern string) (*workDir, error) {
	sweepStaleWorkDirsOnce()
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return nil, err
	}
	lock, err := lockWorkDir(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &workDir{path: dir, created: true, lock: lock}, nil
}

// openWorkDir uses dir, given by the caller, creating it if needed. It must
// not already hold an archive's database or manifest: what is in it would
// be archived too.
func openWorkDir(dir string) (*workDir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, defaultDirPerm); err != nil {
		return nil, err
	}
	for _, name := range []string{manifestDbRelativePath, manifestFileName} {
		if _, err := os.Lstat(filepath.Join(abs, name)); err == nil {
			return nil, fmt.Errorf("work_dir %s already holds %s; use an empty directory", abs, name)
		}
	}
	lock, err := lockWorkDir(abs)
	if err != nil {
		return nil, err
	}
	return &workDir{path: abs, lock: lock}, nil
}

// lockWorkDir takes a work directory's lock.
func lockWorkDir(dir string) (*fsx.Lock, error) {
	return fsx.TryLock(filepath.Join(dir, workLockName), false)
}

// release unlocks the directory and, with remove, deletes it.
func (w *workDir) release(remove bool) {
	if w == nil {
		return
	}
	_ = w.lock.Unlock()
	if remove {
		_ = fsx.RetryBusy(func() error { return os.RemoveAll(w.path) })
		return
	}
	_ = fsx.Remove(filepath.Join(w.path, workLockName))
}

var sweepOnce sync.Once

func sweepStaleWorkDirsOnce() {
	sweepOnce.Do(func() { _, _ = SweepStaleWorkDirs(os.TempDir(), staleWorkAge) })
}

// SweepStaleWorkDirs removes work directories (duesarc-*) under dir that are
// older than olderThan and that no process holds: left by a process that
// was killed. It returns how many it removed.
func SweepStaleWorkDirs(dir string, olderThan time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), tempDirPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < olderThan {
			continue
		}
		p := filepath.Join(dir, e.Name())
		lockPath := filepath.Join(p, workLockName)
		if _, err := os.Lstat(lockPath); err == nil {
			lock, err := fsx.TryLock(lockPath, false)
			if err != nil {
				continue // in use
			}
			_ = lock.Unlock()
		} else if !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if os.RemoveAll(p) == nil {
			removed++
		}
	}
	return removed, nil
}
