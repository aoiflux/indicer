package crash

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunChildScenarios(m, map[string]func() int{
		// unguardedPanic dies the way a stray goroutine panic kills DUES: the
		// runtime prints the trace and exits with code 2.
		"unguardedPanic": func() int {
			if _, err := Setup(os.Getenv("CRASH_DIR"), "child"); err != nil {
				return 3
			}
			go func() {
				panic("unguarded goroutine panic")
			}()
			// The panic ends the process. Waiting on a channel the goroutine
			// closed in a defer let this return 0 first: deferred calls run
			// before the runtime prints the trace and exits.
			time.Sleep(time.Minute)
			return 0
		},
	}))
}

func TestSetupFinishRemovesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p, err := Setup(dir, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("crash file not created: %v", err)
	}
	Finish()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("empty crash file kept after Finish: %v", err)
	}
	if Path() != "" {
		t.Fatal("Path still set after Finish")
	}
}

func TestWriteKeepsReport(t *testing.T) {
	dir := t.TempDir()
	p, err := Setup(dir, "run2")
	if err != nil {
		t.Fatal(err)
	}
	if got := Write("boom", debug.Stack()); got != p {
		t.Fatalf("Write returned %q, want %q", got, p)
	}
	Finish()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("crash file with a report was removed: %v", err)
	}
	if !bytes.Contains(raw, []byte("panic: boom")) || !bytes.Contains(raw, []byte("TestWriteKeepsReport")) {
		t.Fatalf("report lacks the value or stack: %s", raw)
	}

	var msg strings.Builder
	Message(&msg, "boom", "run2", p)
	if !strings.Contains(msg.String(), p) || !strings.Contains(msg.String(), "run2") {
		t.Fatalf("message does not point at the report: %s", msg.String())
	}
}

// TestUnguardedPanicIsCaptured checks the case recover cannot handle: the
// runtime's own trace must land in the crash file.
func TestUnguardedPanicIsCaptured(t *testing.T) {
	dir := t.TempDir()
	c := testutil.StartChild(t, "unguardedPanic", "CRASH_DIR="+dir)
	if code := c.Wait(t, time.Minute); code == 0 || code == 3 {
		t.Fatalf("child exited %d; want a runtime crash\n%s", code, c.Output())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "crash-child.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("unguarded goroutine panic")) || !bytes.Contains(raw, []byte("goroutine")) {
		t.Fatalf("crash file lacks the runtime trace:\n%s", raw)
	}
}
