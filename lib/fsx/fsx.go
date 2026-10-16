// Package fsx holds the filesystem operations DUES relies on for crash safety:
// atomic file replacement, directory sync, advisory locks and cleanup of
// abandoned temporary files.
//
// Every durable file DUES writes outside badger goes through this package, so
// a crash or power loss leaves either the old content or the new content,
// never a truncated mix. On Windows, renames and removals are retried while
// another process (typically antivirus or an indexer) briefly holds the file.
package fsx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tempMarker appears in the name of every temporary file this package
// creates. SweepTemps deletes only names carrying it, so it can never remove a
// file DUES did not create.
const tempMarker = ".dues-tmp-"

// Options controls WriteAtomic.
type Options struct {
	// NoOverwrite makes the write fail with an error wrapping fs.ErrExist when
	// the target already exists, instead of replacing it. The check and the
	// rename are one atomic step where the platform allows it.
	NoOverwrite bool
}

// WriteFileAtomic writes data to path so that a crash leaves either the old
// file or the complete new one. An existing file is replaced.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return WriteAtomic(path, perm, Options{}, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// WriteAtomic creates a temporary file next to path, lets write fill it, then
// syncs it and renames it over path, and syncs the directory.
//
// If write or any later step fails, the temporary file is removed and path is
// left exactly as it was.
func WriteAtomic(path string, perm fs.FileMode, opts Options, write func(f *os.File) error) (err error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+tempMarker+"*")
	if err != nil {
		return fmt.Errorf("fsx: create temporary file for %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = Remove(tmp)
		}
	}()

	if err = write(f); err != nil {
		return fmt.Errorf("fsx: write %s: %w", path, err)
	}
	if err = setPerm(f, perm); err != nil {
		return fmt.Errorf("fsx: set mode of %s: %w", path, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("fsx: sync %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("fsx: close %s: %w", path, err)
	}
	if err = rename(tmp, path, !opts.NoOverwrite); err != nil {
		return fmt.Errorf("fsx: replace %s: %w", path, err)
	}
	// The new file is in place and complete; a failed directory sync means
	// only that its entry may not be durable yet. Reporting that as a failed
	// write would make the caller retry a write that already happened.
	_ = SyncDir(dir)
	return nil
}

// ReplaceFile atomically renames src to dst, which must be in the same
// directory or at least on the same volume. With overwrite false it fails with
// an error wrapping fs.ErrExist when dst exists. The destination directory is
// synced afterwards.
func ReplaceFile(src, dst string, overwrite bool) error {
	if err := rename(src, dst, overwrite); err != nil {
		return fmt.Errorf("fsx: rename %s to %s: %w", src, dst, err)
	}
	_ = SyncDir(filepath.Dir(dst))
	return nil
}

// Remove deletes path, retrying briefly on Windows while another process holds
// it open. A path that does not exist is not an error.
func Remove(path string) error {
	err := retryBusy(func() error { return os.Remove(path) })
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// IsTempName reports whether name (a base name) is a temporary file created by
// this package.
func IsTempName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, tempMarker)
}

// SweepTemps removes temporary files this package left in dir, typically
// after a crash, that are older than olderThan. It does not recurse, removes
// only regular files whose names carry the package's marker, and leaves
// younger ones alone because another process may still be writing them.
func SweepTemps(dir string, olderThan time.Duration) (removed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || !IsTempName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := Remove(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// busyRetryBudget bounds how long a rename or remove keeps retrying while the
// file is held by another process.
const busyRetryBudget = 2 * time.Second

// retryBusy runs op until it succeeds, fails for a reason other than the file
// being busy, or the retry budget is spent.
func retryBusy(op func() error) error {
	delay := 5 * time.Millisecond
	deadline := time.Now().Add(busyRetryBudget)
	for {
		err := op()
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(delay)
		if delay < 200*time.Millisecond {
			delay *= 2
		}
	}
}
