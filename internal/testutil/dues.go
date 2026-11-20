package testutil

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"indicer/internal/osdirs"
)

var (
	buildMu sync.Mutex
	builds  = map[string]*build{}
)

type build struct {
	once sync.Once
	path string
	err  string
}

// BuildDues builds the dues binary from the current module once per test
// process and set of build tags, and returns its path. Black-box tests use it
// so they keep working across internal rewrites; fault-injection tests pass
// "faultinject".
func BuildDues(tb testing.TB, tags ...string) string {
	tb.Helper()
	return BuildBinary(tb, ".", tags...)
}

// BuildBinary builds the main package pkg (a path relative to the module
// root, such as "./cmd/compression") as BuildDues builds dues.
func BuildBinary(tb testing.TB, pkg string, tags ...string) string {
	tb.Helper()
	key := pkg + "|" + strings.Join(tags, ",")
	buildMu.Lock()
	b := builds[key]
	if b == nil {
		b = &build{}
		builds[key] = b
	}
	buildMu.Unlock()
	b.once.Do(func() {
		temp, err := osdirs.Temp()
		if err != nil {
			b.err = err.Error()
			return
		}
		b.path, b.err = buildBinary(temp, pkg, tags)
	})
	if b.err != "" {
		tb.Fatal(b.err)
	}
	return b.path
}

// RemoveBuilds removes the binaries BuildDues and BuildBinary built in this
// test process, tens of megabytes each. Call it from TestMain after the
// tests ran.
func RemoveBuilds() error {
	buildMu.Lock()
	defer buildMu.Unlock()
	var errs []error
	for key, b := range builds {
		b.once.Do(func() {}) // waits out a build still running
		if b.path != "" {
			errs = append(errs, os.RemoveAll(filepath.Dir(b.path)))
		}
		delete(builds, key)
	}
	return errors.Join(errs...)
}

func buildBinary(temp, pkg string, tags []string) (string, string) {
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", "go env GOMOD: " + err.Error()
	}
	root := filepath.Dir(strings.TrimSpace(string(gomod)))
	dir, err := os.MkdirTemp(temp, "dues-bin-")
	if err != nil {
		return "", err.Error()
	}
	name := "dues"
	if pkg != "." {
		name = filepath.Base(pkg)
	}
	bin := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	args := []string{"build", "-o", bin}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	cmd := exec.Command("go", append(args, pkg)...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := "build dues: " + err.Error() + "\n" + string(out)
		if err := os.RemoveAll(dir); err != nil {
			msg += "\n" + err.Error()
		}
		return "", msg
	}
	return bin, ""
}

// Result is the outcome of one CLI run.
type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Err            error // non-nil when the process could not run or timed out
}

// Combined returns stdout followed by stderr.
func (r Result) Combined() string { return r.Stdout + r.Stderr }

// LastLine returns the last non-empty line of the combined output, with
// progress-bar redraws (\r) collapsed.
func (r Result) LastLine() string {
	var last string
	for l := range strings.SplitSeq(r.Combined(), "\n") {
		l = strings.TrimSpace(l[strings.LastIndex(l, "\r")+1:])
		if l != "" {
			last = l
		}
	}
	return last
}

// RunDues runs the binary in dir with args, kept apart from the account as
// IsolatedArgs says, killing it after timeout. Its output is never a
// terminal, so it must hold no escape code unless the arguments ask for
// colour; a run that breaks this fails the test.
func RunDues(tb testing.TB, bin, dir string, timeout time.Duration, args ...string) Result {
	tb.Helper()
	cmd := exec.Command(bin, IsolatedArgs(tb, args)...)
	cmd.Dir = dir
	defer func() {
		if !slices.Contains(args, "--color=always") {
			if out := stdoutOf(cmd) + stderrOf(cmd); strings.Contains(out, "\x1b") {
				tb.Errorf("dues %s wrote an escape code to a pipe:\n%q", strings.Join(args, " "), out)
			}
		}
	}()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Result{Err: err, ExitCode: -1}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1, Err: errTimeout{timeout}}
	}
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Err = err
		}
	}
	return res
}

// IsolatedArgs keeps a run of dues or compression apart from the account.
// It puts --no-config before args unless they choose a config file, and
// --log-dir with a directory of the test's own unless they choose a log
// directory or a config file, which may give one. Home comes from the
// account, not from anything a test can set, so a run left to the defaults
// would read the account's own config file and write its log, and any
// crash file, among the account's logs, whose rotation keeps only the
// newest ten. Every runner of a binary passes its arguments through it.
func IsolatedArgs(tb testing.TB, args []string) []string {
	tb.Helper()
	if !gives(args, "--config", "--log-dir") {
		args = append([]string{"--log-dir", tb.TempDir()}, args...)
	}
	if !gives(args, "--config", "--no-config") {
		args = append([]string{"--no-config"}, args...)
	}
	return args
}

// gives reports whether args, before any "--", give one of flags, as
// "--flag value" or "--flag=value".
func gives(args []string, flags ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

func stdoutOf(cmd *exec.Cmd) string { return cmd.Stdout.(*bytes.Buffer).String() }
func stderrOf(cmd *exec.Cmd) string { return cmd.Stderr.(*bytes.Buffer).String() }

type errTimeout struct{ d time.Duration }

func (e errTimeout) Error() string { return "timed out after " + e.d.String() }
