//go:build windows

package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// holdExclusively opens path with no sharing, as an antivirus scan does, and
// returns a function that closes it.
func holdExclusively(t *testing.T, path string) func() {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { windows.CloseHandle(h) }
}

func TestReplaceFileRetriesWhileTargetBusy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for p, s := range map[string]string{src: "new", dst: "old"} {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	release := holdExclusively(t, dst)
	go func() {
		time.Sleep(150 * time.Millisecond)
		release()
	}()

	if err := ReplaceFile(src, dst, true); err != nil {
		t.Fatalf("ReplaceFile did not wait for the brief hold to end: %v", err)
	}
	if got := readFile(t, dst); got != "new" {
		t.Fatalf("dst %q, want %q", got, "new")
	}
}

func TestReplaceFileReportsPersistentHolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for p, s := range map[string]string{src: "new", dst: "old"} {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	release := holdExclusively(t, dst)
	defer release()

	err := ReplaceFile(src, dst, true)
	if err == nil {
		t.Fatal("ReplaceFile succeeded over a file held open without sharing")
	}
	if !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("error does not say the file is in use: %v", err)
	}
}
