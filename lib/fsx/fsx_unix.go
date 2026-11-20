//go:build !windows

package fsx

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

func setPerm(f *os.File, perm fs.FileMode) error { return f.Chmod(perm) }

// rename moves src to dst. Without overwrite it links dst first, which fails
// atomically when dst exists, then drops src. Filesystems that refuse hard
// links fall back to a check followed by a rename, which leaves a small race.
func rename(src, dst string, overwrite bool) error {
	if overwrite {
		return os.Rename(src, dst)
	}
	err := os.Link(src, dst)
	if err == nil {
		return os.Remove(src)
	}
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	if _, statErr := os.Lstat(dst); statErr == nil {
		return &fs.PathError{Op: "rename", Path: dst, Err: fs.ErrExist}
	}
	return os.Rename(src, dst)
}

// SyncDir makes a rename or creation inside dir durable. Filesystems that
// cannot sync a directory report EINVAL or ENOTSUP; that is not an error here.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// isBusy is always false off Windows: an open file never blocks a rename or a
// removal there.
func isBusy(error) bool { return false }

// PrivateDir reports whether info, of the directory at path, is a directory
// only this process's user can use: owned by the effective user, with
// exactly perm.
func PrivateDir(_ string, info fs.FileInfo, perm fs.FileMode) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && info.Mode().Perm() == perm && int(st.Uid) == os.Geteuid()
}
