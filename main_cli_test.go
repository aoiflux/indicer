package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/fatih/color"
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	oldNoColor := color.NoColor
	color.NoColor = true

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe create failed: %v", err)
	}
	os.Stdout = w

	fn()

	_ = w.Close()
	os.Stdout = oldStdout
	color.NoColor = oldNoColor

	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	_ = r.Close()
	return string(b)
}
