package testutil

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"indicer/internal/testutil/leakcheck"
)

// The flags StartChild starts a test binary with. Every test binary that
// imports testutil has them.
var (
	childScenario = flag.String("dues.child", "", "run this child scenario instead of the tests (set by StartChild)")
	childNotify   = flag.String("dues.notify", "", "the file a child scenario reports the points it reaches to (set by StartChild)")
)

// RunChildScenarios lets one test binary act as its own subprocess. Call it
// first in TestMain: when the binary was started by StartChild it runs the
// named scenario with StartChild's arguments and exits with its return
// code; otherwise it runs the tests through leakcheck.Run.
//
//	func TestMain(m *testing.M) {
//		os.Exit(testutil.RunChildScenarios(m, map[string]func(args []string) int{
//			"store": func(args []string) int { ... },
//		}))
//	}
func RunChildScenarios(m *testing.M, scenarios map[string]func(args []string) int) int {
	flag.Parse()
	if *childScenario == "" {
		return leakcheck.Run(m)
	}
	fn, ok := scenarios[*childScenario]
	if !ok {
		fmt.Fprintf(os.Stderr, "testutil: unknown child scenario %q\n", *childScenario)
		return 99
	}
	return fn(flag.Args())
}

// ChildNotifyFile is, in a child scenario, the file the parent's
// WaitForPoint watches. A scenario that injects faults passes it to
// faultinject.Configure.
func ChildNotifyFile() string { return *childNotify }

// NotifyPoint reports, from a child scenario, that it reached point.
func NotifyPoint(point string) error {
	f, err := os.OpenFile(*childNotify, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(point + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Child is a running scenario subprocess.
type Child struct {
	Cmd    *exec.Cmd
	Notify string // path of the fault-point notification file

	out  *syncBuffer
	done chan struct{}
	err  error
}

// StartChild re-executes the current test binary to run scenario with args.
// The child's output is captured, and it reports the points it reaches to a
// fresh notification file.
func StartChild(tb testing.TB, scenario string, args ...string) *Child {
	tb.Helper()
	exe, err := os.Executable()
	if err != nil {
		tb.Fatal(err)
	}
	notify := newNotifyFile(tb)
	flags := []string{"-test.run=^$", "-dues.child=" + scenario, "-dues.notify=" + notify, "--"}
	return startChild(tb, exec.Command(exe, append(flags, args...)...), notify)
}

// StartDues starts bin, a faultinject build of dues or compression, in dir
// with the fault spec faults ("" for none) and args, kept apart from the
// account as IsolatedArgs says, as StartChild starts a scenario: output
// captured, the first hit of each point reported to a fresh notification
// file, killed at the end of the test if still running.
func StartDues(tb testing.TB, bin, dir, faults string, args ...string) *Child {
	tb.Helper()
	notify := newNotifyFile(tb)
	flags := append(FaultArgs(faults), "--faults-notify="+notify)
	cmd := exec.Command(bin, IsolatedArgs(tb, append(flags, args...))...)
	cmd.Dir = dir
	return startChild(tb, cmd, notify)
}

// FaultArgs is the hidden flag that gives a faultinject build of dues or
// compression its fault spec, or nothing for an empty spec. Put it before
// the command, where compression takes its global options.
func FaultArgs(spec string) []string {
	if spec == "" {
		return nil
	}
	return []string{"--faults=" + spec}
}

func newNotifyFile(tb testing.TB) string {
	tb.Helper()
	notify := filepath.Join(tb.TempDir(), "points.log")
	if err := os.WriteFile(notify, nil, 0o644); err != nil {
		tb.Fatal(err)
	}
	return notify
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
