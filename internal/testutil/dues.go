package testutil

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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
	b.once.Do(func() { b.path, b.err = buildBinary(pkg, tags) })
	if b.err != "" {
		tb.Fatal(b.err)
	}
	return b.path
}

func buildBinary(pkg string, tags []string) (string, string) {
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", "go env GOMOD: " + err.Error()
	}
	root := filepath.Dir(strings.TrimSpace(string(gomod)))
	dir, err := os.MkdirTemp("", "dues-bin-")
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
		return "", "build dues: " + err.Error() + "\n" + string(out)
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

// RunDues runs the binary in dir with args, killing it after timeout.
func RunDues(tb testing.TB, bin, dir string, timeout time.Duration, args ...string) Result {
	tb.Helper()
	return RunDuesEnv(tb, bin, dir, timeout, nil, args...)
}

// RunDuesEnv is RunDues with extra environment ("K=V") for the child only.
func RunDuesEnv(tb testing.TB, bin, dir string, timeout time.Duration, env []string, args ...string) Result {
	tb.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
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

type errTimeout struct{ d time.Duration }

func (e errTimeout) Error() string { return "timed out after " + e.d.String() }
