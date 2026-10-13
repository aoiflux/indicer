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
	buildOnce sync.Once
	buildPath string
	buildErr  string
)

// BuildDues builds the dues binary from the current module once per test
// process and returns its path. Black-box tests use it so they keep working
// across internal rewrites.
func BuildDues(tb testing.TB) string {
	tb.Helper()
	buildOnce.Do(func() {
		gomod, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			buildErr = "go env GOMOD: " + err.Error()
			return
		}
		root := filepath.Dir(strings.TrimSpace(string(gomod)))
		dir, err := os.MkdirTemp("", "dues-bin-")
		if err != nil {
			buildErr = err.Error()
			return
		}
		bin := filepath.Join(dir, "dues")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = "build dues: " + err.Error() + "\n" + string(out)
			return
		}
		buildPath = bin
	})
	if buildErr != "" {
		tb.Fatal(buildErr)
	}
	return buildPath
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
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
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
