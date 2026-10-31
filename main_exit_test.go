package main

import (
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/ui"
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

// TestVersionFlag: --version and -v print the identity line on stdout and
// nothing else, wherever they stand, and exit 0 with the log written and no
// crash file left. kingpin's own flag printed a block on stderr and, with no
// command, its whole usage after it.
func TestVersionFlag(t *testing.T) {
	want := ui.Identity(buildInfo()) + "\n"
	for _, args := range [][]string{{"--version"}, {"-v"}, {"--plain", "--version"}, {"list", "--version"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := isolateLogs(t)
			code := -1
			stdout, stderr := captureOutput(t, func() { code = realMain(args) })
			if code != lifecycle.ExitOK {
				t.Fatalf("exited %d, want 0", code)
			}
			if stdout != want || stderr != "" {
				t.Errorf("stdout %q, stderr %q; want stdout %q alone", stdout, stderr, want)
			}
			crashes, _ := filepath.Glob(filepath.Join(dir, "crash-*.txt"))
			if len(crashes) != 0 {
				t.Errorf("empty crash files left after a clean exit: %v", crashes)
			}
			logs, _ := filepath.Glob(filepath.Join(dir, "dues-*.ndjson"))
			if len(logs) == 0 {
				t.Error("no log file written")
			}
		})
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
