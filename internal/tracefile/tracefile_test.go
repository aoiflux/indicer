package tracefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"testing"
)

// The tests here trace the test process itself, so none runs in parallel.

// traceHeader is how a Go execution trace file starts.
const traceHeader = "go 1."

// untraced skips a test when go test -trace traces the process already.
func untraced(t *testing.T) {
	t.Helper()
	if trace.IsEnabled() {
		t.Skip("the test process is traced already")
	}
}

// TestStartStop: a trace started and stopped leaves a trace file, and tracing
// off.
func TestStartStop(t *testing.T) {
	untraced(t)
	path := filepath.Join(t.TempDir(), "run.trace")
	stop, err := Start(path)
	if err != nil {
		t.Fatal(err)
	}
	if !trace.IsEnabled() {
		t.Fatal("tracing is off after Start")
	}
	trace.Log(t.Context(), "test", "a line in the trace")
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if trace.IsEnabled() {
		t.Fatal("tracing is on after stop")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte(traceHeader)) {
		t.Fatalf("trace starts %q, want %q", b[:min(len(b), 16)], traceHeader)
	}
}

// TestRefusesExisting: a file already at the path is kept as it was, and
// Start says so.
func TestRefusesExisting(t *testing.T) {
	untraced(t)
	path := filepath.Join(t.TempDir(), "kept")
	if err := os.WriteFile(path, []byte("evidence notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(path); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("Start over a file: %v, want an error saying it exists", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "evidence notes" {
		t.Fatalf("the file now holds %q", b)
	}
	if trace.IsEnabled() {
		t.Fatal("tracing is on after a refused Start")
	}
}

// TestStopTwice: a second stop does nothing and gives the first's result.
func TestStopTwice(t *testing.T) {
	untraced(t)
	stop, err := Start(filepath.Join(t.TempDir(), "twice.trace"))
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if err := stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// TestAlreadyTracing: a second trace while one runs is refused, and leaves
// no file behind.
func TestAlreadyTracing(t *testing.T) {
	untraced(t)
	dir := t.TempDir()
	stop, err := Start(filepath.Join(dir, "first.trace"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop() }()
	second := filepath.Join(dir, "second.trace")
	if _, err := Start(second); !errors.Is(err, ErrTracing) {
		t.Fatalf("second Start: %v, want ErrTracing", err)
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("second trace file left: %v", err)
	}
}
