package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"indicer/internal/testutil"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if IsTempName(e.Name()) {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileAtomicCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	if err := WriteFileAtomic(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "one" {
		t.Fatalf("content %q, want %q", got, "one")
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "two" {
		t.Fatalf("content after replace %q, want %q", got, "two")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode %v, want 0600", perm)
		}
	}
	assertNoTemps(t, dir)
}

func TestWriteAtomicFailureLeavesTargetIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	if err := WriteFileAtomic(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := WriteAtomic(path, 0o644, Options{}, func(f *os.File) error {
		w := testutil.NewENOSPCWriter(f, 3)
		_, err := w.Write([]byte("partial content"))
		return err
	})
	if !errors.Is(err, testutil.ErrNoSpace()) {
		t.Fatalf("WriteAtomic error %v, want the writer's disk-full error", err)
	}
	if got := readFile(t, path); got != "original" {
		t.Fatalf("target changed to %q after a failed write", got)
	}
	assertNoTemps(t, dir)
}

func TestWriteAtomicNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restored.bin")
	write := func(s string) func(*os.File) error {
		return func(f *os.File) error { _, err := f.WriteString(s); return err }
	}

	if err := WriteAtomic(path, 0o644, Options{NoOverwrite: true}, write("first")); err != nil {
		t.Fatal(err)
	}
	err := WriteAtomic(path, 0o644, Options{NoOverwrite: true}, write("second"))
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second write returned %v, want fs.ErrExist", err)
	}
	if got := readFile(t, path); got != "first" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	assertNoTemps(t, dir)
}

func TestWriteNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chunk.blob")

	if err := WriteNewFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "first" {
		t.Fatalf("content %q, want %q", got, "first")
	}
	err := WriteNewFile(path, []byte("second"), 0o600)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second write returned %v, want fs.ErrExist", err)
	}
	if got := readFile(t, path); got != "first" {
		t.Fatalf("existing file overwritten: %q", got)
	}
	assertNoTemps(t, dir)
}

func TestWriteNewFailureRemovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chunk.blob")

	err := WriteNew(path, 0o600, func(f *os.File) error {
		w := testutil.NewENOSPCWriter(f, 3)
		_, err := w.Write([]byte("partial content"))
		return err
	})
	if !errors.Is(err, testutil.ErrNoSpace()) {
		t.Fatalf("WriteNew error %v, want the writer's disk-full error", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("partial file left after a failed write: %v", err)
	}
}

func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceFile(src, dst, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("ReplaceFile without overwrite returned %v, want fs.ErrExist", err)
	}
	if got := readFile(t, dst); got != "old" {
		t.Fatalf("dst changed to %q by a refused replace", got)
	}
	if err := ReplaceFile(src, dst, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dst); got != "new" {
		t.Fatalf("dst %q after replace, want %q", got, "new")
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("src still exists after replace: %v", err)
	}
}

func TestSweepTempsRemovesOnlyOldOwnTemps(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldTemp := mk(".blob"+tempMarker+"123", old)
	youngTemp := mk(".blob"+tempMarker+"456", time.Now())
	userFile := mk("notes.tmp", old)
	lookalike := mk("blob"+tempMarker+"789", old) // no leading dot: not ours

	removed, err := SweepTemps(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d files, want 1", removed)
	}
	if _, err := os.Stat(oldTemp); !errors.Is(err, fs.ErrNotExist) {
		t.Error("old temporary file was not removed")
	}
	for _, p := range []string{youngTemp, userFile, lookalike} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(p), err)
		}
	}

	if n, err := SweepTemps(filepath.Join(dir, "missing"), time.Hour); n != 0 || err != nil {
		t.Errorf("sweep of a missing directory = %d, %v; want 0, nil", n, err)
	}
}

func TestLockExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dues.lock")

	ex, err := TryLock(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("second exclusive lock returned %v, want ErrLocked", err)
	}
	if _, err := TryLock(path, true); !errors.Is(err, ErrLocked) {
		t.Fatalf("shared lock under an exclusive one returned %v, want ErrLocked", err)
	}
	if err := ex.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := ex.Unlock(); err != nil {
		t.Fatalf("second Unlock returned %v", err)
	}

	sh1, err := TryLock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer sh1.Unlock()
	sh2, err := TryLock(path, true)
	if err != nil {
		t.Fatalf("two shared locks must coexist: %v", err)
	}
	defer sh2.Unlock()
	if _, err := TryLock(path, false); !errors.Is(err, ErrLocked) {
		t.Fatalf("exclusive lock under shared ones returned %v, want ErrLocked", err)
	}
}

// TestSharedLockOnReadOnlyFile covers read-only media: the lock file cannot be
// opened for writing, yet readers must still be able to share it.
func TestSharedLockOnReadOnlyFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		testutil.RequireLinuxNonRoot(t)
	}
	path := filepath.Join(t.TempDir(), "dues.lock")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	l, err := TryLock(path, true)
	if err != nil {
		t.Fatalf("shared lock on a read-only file: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path, false); err == nil {
		t.Fatal("exclusive lock succeeded on a file that cannot be opened for writing")
	}
}

// TestWriteAtomicRemovesStaleTemps: with RemoveStale, a write first removes
// what killed writes to the same path left, and nothing else.
func TestWriteAtomicRemovesStaleTemps(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "report.json")
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := []string{mk(".report.json" + tempMarker + "1"), mk(".report.json" + tempMarker + "2")}
	other := mk(".report.json.bak" + tempMarker + "3") // another target's
	user := mk("report.json.old")

	if err := WriteAtomic(target, 0o644, Options{RemoveStale: true}, func(f *os.File) error {
		_, err := f.WriteString("{}")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range stale {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s was left", filepath.Base(p))
		}
	}
	for _, p := range []string{other, user, target} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", filepath.Base(p), err)
		}
	}
	if got := TempPattern(target); got != ".report.json"+tempMarker+"*" {
		t.Errorf("TempPattern = %q", got)
	}
}
