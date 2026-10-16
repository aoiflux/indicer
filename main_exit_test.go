package main

import (
	"path/filepath"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/logging"
)

// isolateLogs keeps realMain's log and crash files in a temp directory.
func isolateLogs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(logging.EnvDir, dir)
	return dir
}

func TestRealMainUsageErrorExits2(t *testing.T) {
	isolateLogs(t)
	if code := realMain([]string{"no-such-command"}); code != lifecycle.ExitUsage {
		t.Fatalf("unknown command exited %d, want %d", code, lifecycle.ExitUsage)
	}
	if code := realMain([]string{"list", "--no-such-flag"}); code != lifecycle.ExitUsage {
		t.Fatalf("unknown flag exited %d, want %d", code, lifecycle.ExitUsage)
	}
}

func TestRealMainVersionExits0AndCleansUp(t *testing.T) {
	dir := isolateLogs(t)
	if code := realMain([]string{"--version"}); code != lifecycle.ExitOK {
		t.Fatalf("--version exited %d, want 0", code)
	}
	crashes, _ := filepath.Glob(filepath.Join(dir, "crash-*.txt"))
	if len(crashes) != 0 {
		t.Fatalf("empty crash files left after a clean exit: %v", crashes)
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "dues-*.ndjson"))
	if len(logs) == 0 {
		t.Fatal("no log file written")
	}
}

func TestLogDirFromArgs(t *testing.T) {
	cases := map[string][]string{
		"/a": {"--log-dir", "/a", "list"},
		"/b": {"list", "--log-dir=/b"},
		"":   {"list", "--", "--log-dir", "/c"},
	}
	for want, args := range cases {
		if got := logDirFromArgs(args); got != want {
			t.Errorf("logDirFromArgs(%q) = %q, want %q", args, got, want)
		}
	}
}
