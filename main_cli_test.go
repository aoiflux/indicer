package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestPrintHelpForPath_NearInRoutesToNearInHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"near", "in"})
	})
	if !strings.Contains(out, "Command: near in") {
		t.Fatalf("expected near in help header, got: %s", out)
	}
}

func TestPrintHelpForPath_MicroExtractRoutesToMicroExtractHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"micro", "extract"})
	})
	if !strings.Contains(out, "Command: micro extract") {
		t.Fatalf("expected micro extract help header, got: %s", out)
	}
}

func TestPrintHelpForPath_MicroListRoutesToMicroListHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"micro", "list"})
	})
	if !strings.Contains(out, "Command: micro list") {
		t.Fatalf("expected micro list help header, got: %s", out)
	}
}

func TestPrintHelpForPath_UnknownFallsBackToRootHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"does-not-exist"})
	})
	if !strings.Contains(out, "DUES - Deduplicated Unified Evidence Store") {
		t.Fatalf("expected root help fallback, got: %s", out)
	}
}

// TestEveryCommandHasHelp checks that `dues help` knows every command the
// parser defines. The help text is written by hand, apart from the parser,
// and `dues help repair` and `dues help reindex` printed "Unknown command".
func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range newCLIDefinition().app.Model().Commands {
		if c.Hidden {
			continue
		}
		paths := [][]string{{c.Name}}
		for _, sub := range c.Commands {
			if !sub.Hidden {
				paths = append(paths, []string{c.Name, sub.Name})
			}
		}
		for _, path := range paths {
			name := strings.Join(path, " ")
			out := captureStdout(t, func() { printHelpForPath(path) })
			if !strings.Contains(out, "Command: "+name+"\n") {
				t.Errorf("dues help %s does not print its own help", name)
			}
		}
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe create failed: %v", err)
	}
	os.Stdout = w

	// Read while fn writes: output larger than the pipe's buffer would
	// otherwise block fn for ever.
	type result struct {
		b   []byte
		err error
	}
	read := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		read <- result{b, err}
	}()

	fn()

	_ = w.Close()
	os.Stdout = oldStdout

	res := <-read
	if res.err != nil {
		t.Fatalf("read stdout failed: %v", res.err)
	}
	_ = r.Close()
	return string(res.b)
}
