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
	t.Parallel()
	for _, c := range []struct {
		args  []string
		want  string
		given bool
	}{
		{[]string{"--log-dir", "/a", "list"}, "/a", true},
		{[]string{"list", "--log-dir=/b"}, "/b", true},
		{[]string{"list", "--log-dir="}, "", true},
		{[]string{"list", "--", "--log-dir", "/c"}, "", false},
		{[]string{"list"}, "", false},
	} {
		if got, given := flagFromArgs(c.args, flagLogDir); got != c.want || given != c.given {
			t.Errorf("flagFromArgs(%q, log-dir) = %q, %v; want %q, %v", c.args, got, given, c.want, c.given)
		}
	}
	if got, _ := flagFromArgs([]string{"--log-dir", "/a", "--temp-dir", "/t"}, flagTempDir); got != "/t" {
		t.Errorf("flagFromArgs(temp-dir) = %q, want /t", got)
	}
	file := "/f"
	for args, want := range map[string]string{"--log-dir /a": "/a", "--log-dir=": "", "list": "/f"} {
		if got := flagOr(strings.Fields(args), flagLogDir, &file); got != want {
			t.Errorf("flagOr(%q, log-dir, %q) = %q, want %q", args, file, got, want)
		}
	}
	if got := flagOr([]string{"list"}, flagLogDir, nil); got != "" {
		t.Errorf("flagOr without a file value = %q, want \"\"", got)
	}
}

func TestBoolFlagFromArgs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args            []string
		want, wantGiven bool
	}{
		{[]string{"--debug", "list"}, true, true},
		{[]string{"list", "--debug"}, true, true},
		{[]string{"--no-debug", "--debug"}, true, true},
		{[]string{"--debug", "--no-debug"}, false, true},
		{[]string{"list"}, false, false},
		{[]string{"list", "--", "--debug"}, false, false},
		{[]string{"--debugger"}, false, false},
	} {
		if got, given := boolFlagFromArgs(c.args, flagDebug); got != c.want || given != c.wantGiven {
			t.Errorf("boolFlagFromArgs(%q, debug) = %v, %v; want %v, %v", c.args, got, given, c.want, c.wantGiven)
		}
	}
}
