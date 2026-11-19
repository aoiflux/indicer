package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestTryLockStable: the lock holds the file at its path, and a second taker
// contends, also after the first holder removed the file and made it again.
func TestTryLockStable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "stage.lock")
	l, err := TryLockStable(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLockStable(path, false); !errors.Is(err, ErrLocked) {
		t.Errorf("a second lock: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	again, err := TryLockStable(path, false)
	if err != nil {
		t.Fatalf("after the file was removed: %v", err)
	}
	defer again.Unlock()
	held, err := again.f.Stat()
	cur, cerr := os.Stat(path)
	if err != nil || cerr != nil || !os.SameFile(held, cur) {
		t.Errorf("the lock holds another file than the one at its path: %v, %v", err, cerr)
	}
}

// TestPrivateDir: a folder made with the stage's mode is private, a file is
// not.
func TestPrivateDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !PrivateDir(dir, info, 0o700) {
		t.Error("a 0700 folder of ours is not private")
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(file); err != nil || PrivateDir(file, info, 0o700) {
		t.Errorf("a file is taken for a private folder: %v", err)
	}
}

// TestTryLockStableRefusesLink: a link at the lock's path is refused, and
// the file it points to is not made.
func TestTryLockStableRefusesLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	path := filepath.Join(dir, "lock")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if l, err := TryLockStable(path, false); err == nil {
		_ = l.Unlock()
		t.Fatal("a link was locked")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link's target was made: %v", err)
	}
}
