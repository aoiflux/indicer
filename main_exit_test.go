package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/console"
	"indicer/internal/faultinject"
	"indicer/internal/lifecycle"
	"indicer/internal/ui"
)

// logArgs keeps realMain's log and crash files in a temp directory: it
// returns the directory and the --log-dir arguments that select it.
func logArgs(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	return dir, []string{"--" + flagLogDir, dir}
}

func TestRealMainUsageErrorExits2(t *testing.T) {
	_, logs := logArgs(t)
	if code := realMain(append(logs, "no-such-command")); code != lifecycle.ExitUsage {
		t.Fatalf("unknown command exited %d, want %d", code, lifecycle.ExitUsage)
	}
	if code := realMain(append(logs, "list", "--no-such-flag")); code != lifecycle.ExitUsage {
		t.Fatalf("unknown flag exited %d, want %d", code, lifecycle.ExitUsage)
	}
}

// TestFaultsFlag: a release build has no --faults flag, and a faultinject
// build refuses a bad spec, so either way a bad one is a usage error.
func TestFaultsFlag(t *testing.T) {
	_, logs := logArgs(t)
	args := [][]string{{"--faults", "no.such.point=error", "version"}}
	if !faultinject.Enabled {
		args = append(args, []string{"--faults", faultinject.ResetMid + "=error", "version"}, []string{"--faults-notify", "f", "version"})
	}
	for _, a := range args {
		if code := realMain(append(logs, a...)); code != lifecycle.ExitUsage {
			t.Errorf("%v exited %d, want %d", a, code, lifecycle.ExitUsage)
		}
	}
}

// TestTempDirMustExist: a --temp-dir that is missing or not a directory is
// a usage error before any command runs.
func TestTempDirMustExist(t *testing.T) {
	_, logs := logArgs(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	console.Configure(io.Discard, &stderr)
	t.Cleanup(console.Reset)
	for _, dir := range []string{filepath.Join(t.TempDir(), "missing"), file} {
		db := filepath.Join(t.TempDir(), "db")
		stderr.Reset()
		code := realMain(append(logs, "--"+flagTempDir, dir, "list", "--dbpath", db))
		if code != lifecycle.ExitUsage || !strings.Contains(stderr.String(), "--"+flagTempDir) {
			t.Errorf("--temp-dir %s: exit %d, stderr %q; want a usage error naming the flag", dir, code, stderr.String())
		}
		if _, err := os.Stat(db); !os.IsNotExist(err) {
			t.Errorf("--temp-dir %s: the command ran (database created)", dir)
		}
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
			dir, logArg := logArgs(t)
			code := -1
			stdout, stderr := captureOutput(t, func() { code = realMain(append(logArg, args...)) })
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

func TestFlagFromArgs(t *testing.T) {
	cases := map[string][]string{
		"/a": {"--log-dir", "/a", "list"},
		"/b": {"list", "--log-dir=/b"},
		"":   {"list", "--", "--log-dir", "/c"},
	}
	for want, args := range cases {
		if got := flagFromArgs(args, flagLogDir); got != want {
			t.Errorf("flagFromArgs(%q, log-dir) = %q, want %q", args, got, want)
		}
	}
	if got := flagFromArgs([]string{"--log-dir", "/a", "--temp-dir", "/t"}, flagTempDir); got != "/t" {
		t.Errorf("flagFromArgs(temp-dir) = %q, want /t", got)
	}
}

func TestBoolFlagFromArgs(t *testing.T) {
	cases := map[bool][][]string{
		true:  {{"--debug", "list"}, {"list", "--debug"}, {"--no-debug", "--debug"}},
		false: {{"list"}, {"--debug", "--no-debug"}, {"list", "--", "--debug"}, {"--debugger"}},
	}
	for want, all := range cases {
		for _, args := range all {
			if got := boolFlagFromArgs(args, flagDebug); got != want {
				t.Errorf("boolFlagFromArgs(%q, debug) = %v, want %v", args, got, want)
			}
		}
	}
}
