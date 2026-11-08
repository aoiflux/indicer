package leakcheck

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) { os.Exit(Run(m)) }

var leaky = flag.Bool("leakcheck.leaky", false, "run TestLeaky, which leaks a goroutine")

// TestLeaky is run by TestRunFailsOnALeak in a child process.
func TestLeaky(t *testing.T) {
	if !*leaky {
		t.Skip("run by TestRunFailsOnALeak")
	}
	grace = 100 * time.Millisecond
	go leakForever()
}

func leakForever() { select {} }

// TestRunFailsOnALeak: the tests passed, but one left a goroutine running,
// so the binary fails and names it.
func TestRunFailsOnALeak(t *testing.T) {
	out, err := exec.Command(os.Args[0], "-test.run=^TestLeaky$", "-leakcheck.leaky").CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("want exit 1, got %v:\n%s", err, out)
	}
	for _, want := range []string{"leakcheck: 1 goroutine(s) still running", "leakcheck.leakForever()"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestWaitLetsGoroutinesEnd(t *testing.T) {
	before := ids()
	done := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(done)
	}()
	if leaked := wait(before, time.Second); leaked != nil {
		t.Fatalf("a goroutine that ended within the grace period was reported:\n%s", leaked)
	}
	<-done
}

func TestWaitReportsWhatIsStillRunning(t *testing.T) {
	before := ids()
	started, stop := make(chan struct{}), make(chan struct{})
	defer close(stop)
	go blockUntil(started, stop)
	// Under load the goroutine could still be waiting to run, its stack
	// not yet in blockUntil, when wait looked.
	<-started
	leaked := wait(before, 20*time.Millisecond)
	if len(leaked) != 1 || !strings.Contains(leaked[0], "leakcheck.blockUntil(") {
		t.Fatalf("want blockUntil reported, got:\n%s", leaked)
	}
}

func blockUntil(started, stop chan struct{}) {
	close(started)
	<-stop
}

// TestSignalLoopIsProcessWide: signal.Notify starts goroutines that live as
// long as the process, so they are not leaks.
func TestSignalLoopIsProcessWide(t *testing.T) {
	before := ids()
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Stop(c)
	if leaked := wait(before, 20*time.Millisecond); leaked != nil {
		t.Fatalf("signal.Notify's goroutines were reported:\n%s", leaked)
	}
}
