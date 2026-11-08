package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSummary: with --summary on, a file or archive command also says on
// stderr what it did, while stdout stays the one JSON envelope, now with
// the operation's duration and the size ratio. auto stays quiet when stderr
// is not a terminal, as here; off is always quiet.
func TestSummary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "case")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a.txt": strings.Repeat("alpha ", 4096), "sub/b.txt": "beta"} {
		if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	archive := filepath.Join(dir, "case.duesarc")
	code, env, stderr := runCLI(t, ctx, "", "--summary", "on", cmdCreateArchive, "--input", src, "--output", archive)
	if code != 0 {
		t.Fatalf("create-archive: exit %d %v %s", code, env, stderr)
	}
	if _, ok := env["duration_ms"].(float64); !ok {
		t.Errorf("envelope %v has no duration_ms", env)
	}
	result, _ := env["result"].(map[string]any)
	if r, _ := result["ratio"].(float64); r <= 1 {
		t.Errorf("ratio %v, want above 1 for repetitive text", result["ratio"])
	}
	if _, ok := result["saved_percent"].(float64); !ok {
		t.Errorf("result %v has no saved_percent", result)
	}
	for _, want := range []string{"archived 2 files and 1 folder in ", "smaller, ", "% saved", "written to " + archive} {
		if !strings.Contains(stderr, want) {
			t.Errorf("summary %q lacks %q", stderr, want)
		}
	}

	out := filepath.Join(dir, "out")
	if _, _, stderr := runCLI(t, ctx, "", "--summary", "on", cmdExtractArchive, "--input", archive, "--output", out); !strings.Contains(stderr, "extracted 2 files and 1 folder in ") {
		t.Errorf("extract summary %q", stderr)
	}
	for _, mode := range []string{"auto", "off"} {
		if _, _, stderr := runCLI(t, ctx, "", "--summary", mode, cmdCreateArchive, "--input", src, "--output", archive); strings.Contains(stderr, "archived") {
			t.Errorf("--summary %s: stderr %q has a summary", mode, stderr)
		}
	}
	if code, _, stderr := runCLI(t, ctx, "", "--summary", "loud", cmdCapabilities); code != 2 || !strings.Contains(stderr, "use auto, on or off") {
		t.Errorf("--summary loud: exit %d, %q", code, stderr)
	}
}
