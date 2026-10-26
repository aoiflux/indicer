package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Env names the subprocess harness uses.
const (
	// EnvChildScenario names the scenario a re-executed test binary runs.
	EnvChildScenario = "DUES_CHILD_SCENARIO"
	// EnvFaultsNotify is the file a child appends "<point>\n" to whenever it
	// reaches a fault point; internal/faultinject writes it.
	EnvFaultsNotify = "DUES_FAULTS_NOTIFY"
)

// RunChildScenarios lets one test binary act as its own subprocess. Call it
// first in TestMain: when the binary was started by StartChild it runs the
// named scenario and exits with its return code; otherwise it runs the tests.
//
//	func TestMain(m *testing.M) {
//		os.Exit(testutil.RunChildScenarios(m, map[string]func() int{
//			"store": func() int { ... },
//		}))
//	}
func RunChildScenarios(m *testing.M, scenarios map[string]func() int) int {
	name := os.Getenv(EnvChildScenario)
	if name == "" {
		return m.Run()
	}
	fn, ok := scenarios[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "testutil: unknown child scenario %q\n", name)
		return 99
	}
	return fn()
}

// Child is a running scenario subprocess.
type Child struct {
	Cmd    *exec.Cmd
	Notify string // path of the fault-point notification file

	out  *syncBuffer
	done chan struct{}
	err  error
}

// StartChild re-executes the current test binary to run scenario, with the
// given extra environment ("K=V"). The child's output is captured, and a
// fresh notification file is passed in DUES_FAULTS_NOTIFY.
func StartChild(tb testing.TB, scenario string, env ...string) *Child {
	tb.Helper()
	exe, err := os.Executable()
	if err != nil {
		tb.Fatal(err)
	}
	notify := filepath.Join(tb.TempDir(), "points.log")
	if err := os.WriteFile(notify, nil, 0o644); err != nil {
		tb.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), EnvChildScenario+"="+scenario, EnvFaultsNotify+"="+notify)
	cmd.Env = append(cmd.Env, env...)
	return startChild(tb, cmd, notify)
}

// StartDues starts the dues binary bin in dir with args and the given extra
// environment ("K=V"), as StartChild starts a scenario: output captured, a
// fresh notification file in DUES_FAULTS_NOTIFY, killed at the end of the
// test if still running.
func StartDues(tb testing.TB, bin, dir string, env []string, args ...string) *Child {
	tb.Helper()
	notify := filepath.Join(tb.TempDir(), "points.log")
	if err := os.WriteFile(notify, nil, 0o644); err != nil {
		tb.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), EnvFaultsNotify+"="+notify)
	cmd.Env = append(cmd.Env, env...)
	return startChild(tb, cmd, notify)
}

func startChild(tb testing.TB, cmd *exec.Cmd, notify string) *Child {
	tb.Helper()
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	prepareInterruptible(cmd)
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	c := &Child{Cmd: cmd, Notify: notify, out: out, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	tb.Cleanup(func() {
		select {
		case <-c.done:
		default:
			_ = cmd.Process.Kill()
			<-c.done
		}
	})
	return c
}

// WaitForPoint blocks until the child reports reaching point, the child
// exits, or timeout passes; the last two fail the test.
func (c *Child) WaitForPoint(tb testing.TB, point string, timeout time.Duration) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(c.Notify)
		if err != nil {
			tb.Fatal(err)
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			if strings.TrimSpace(line) == point {
				return
			}
		}
		select {
		case <-c.done:
			tb.Fatalf("child exited before reaching %q: %v\n%s", point, c.err, c.Output())
		default:
		}
		if time.Now().After(deadline) {
			tb.Fatalf("child did not reach %q within %s\n%s", point, timeout, c.Output())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Kill terminates the child immediately, as a crash or power loss would.
func (c *Child) Kill(tb testing.TB) {
	tb.Helper()
	if err := c.Cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		tb.Fatal(err)
	}
	<-c.done
}

// Interrupt sends the platform's interactive interrupt: SIGINT on Unix,
// CTRL_BREAK_EVENT on Windows.
func (c *Child) Interrupt(tb testing.TB) {
	tb.Helper()
	if err := interrupt(c.Cmd); err != nil {
		tb.Fatal(err)
	}
}

// Wait waits for the child to exit and returns its exit code. It fails the
// test if the child is still running after timeout.
func (c *Child) Wait(tb testing.TB, timeout time.Duration) int {
	tb.Helper()
	select {
	case <-c.done:
	case <-time.After(timeout):
		_ = c.Cmd.Process.Kill()
		<-c.done
		tb.Fatalf("child still running after %s\n%s", timeout, c.Output())
	}
	if c.err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(c.err, &ee) {
		return ee.ExitCode()
	}
	tb.Fatalf("child wait: %v", c.err)
	return -1
}

// Output returns everything the child wrote to stdout and stderr so far.
func (c *Child) Output() string { return c.out.String() }

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
