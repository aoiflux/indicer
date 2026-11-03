// Package leakcheck fails a test binary whose tests leave goroutines
// running: nothing an operation starts may outlive it (invariant I6). Every
// test package's TestMain runs its tests through Run, directly or through
// testutil.RunChildScenarios; a guard test checks that.
//
// It imports only the standard library, so any package's tests can use it.
package leakcheck

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// grace is how long the goroutines the tests started may take to end once
// the tests have. A variable only so this package's own test can shorten it.
var grace = 5 * time.Second

// Run runs the tests and returns their exit code. When they pass, it waits
// up to grace for every goroutine they started to end, and for any still
// running it prints the stacks and returns 1. Goroutines already running
// before the tests, those of package init among them, are not counted.
//
// Fuzzing is not checked: the fuzzing engine's own goroutines outlive the
// fuzz target, and a fuzz run checks for crashes, not leaks.
func Run(m *testing.M) int {
	before := ids()
	code := m.Run()
	if code != 0 || fuzzing() {
		return code
	}
	if leaked := wait(before, grace); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "leakcheck: %d goroutine(s) still running %v after the tests passed:\n\n%s\n",
			len(leaked), grace, strings.Join(leaked, "\n\n"))
		return 1
	}
	return code
}

func fuzzing() bool {
	f := flag.Lookup("test.fuzz")
	return f != nil && f.Value.String() != ""
}

// wait returns the stacks of the goroutines not in before that are still
// running after d, sorted; nil as soon as there are none.
func wait(before map[string]bool, d time.Duration) []string {
	deadline := time.Now().Add(d)
	for pause := time.Millisecond; ; pause = min(2*pause, 100*time.Millisecond) {
		leaked := running(before)
		if len(leaked) == 0 || time.Now().After(deadline) {
			slices.Sort(leaked)
			return leaked
		}
		time.Sleep(pause)
	}
}

// running returns the stacks of the goroutines not in before, less the
// process-wide ones.
func running(before map[string]bool) []string {
	var out []string
	for id, stack := range stacks() {
		if !before[id] && !processWide(stack) {
			out = append(out, stack)
		}
	}
	return out
}

// processWideTops are the top frames of goroutines the standard library
// starts once and keeps for the life of the process.
var processWideTops = []string{
	// signal.Notify's delivery loop.
	"os/signal.signal_recv(",
	"os/signal.loop(",
	// The signal mask goroutine, started with the first signal.Notify.
	"runtime.ensureSigM.",
}

func processWide(stack string) bool {
	_, frames, _ := strings.Cut(stack, "\n")
	for _, top := range processWideTops {
		if strings.HasPrefix(frames, top) {
			return true
		}
	}
	return false
}

func ids() map[string]bool {
	out := map[string]bool{}
	for id := range stacks() {
		out[id] = true
	}
	return out
}

// stacks returns each goroutine's stack but the caller's, keyed by its
// "goroutine N" header.
func stacks() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[string]string{}
	for i, g := range strings.Split(string(buf), "\n\n") {
		header, _, _ := strings.Cut(g, " [")
		// The first stack is the calling goroutine's.
		if i > 0 && strings.HasPrefix(header, "goroutine ") {
			out[header] = g
		}
	}
	return out
}
