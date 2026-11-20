package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
)

// TestSearchCommand: the search command refuses a bad query or an existing
// report as usage errors, and exits 2 when an object cannot be read, naming
// it in the report.
func TestSearchCommand(t *testing.T) {
	_, image := fixtureNamed(t, "fat/fat12")
	dbPath := filepath.Join(t.TempDir(), "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	if err := StoreData(t.Context(), opts, image, false, false, false, false); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "report.json")
	var usage *lifecycle.UsageError
	if err := SearchCmd(t.Context(), opts, SearchOptions{Query: "a", Out: out}); !errors.As(err, &usage) {
		t.Fatalf("a one-letter query: %v, want a usage error", err)
	}
	if err := SearchCmd(t.Context(), opts, SearchOptions{Query: strings.Repeat("x", 2000), Out: out}); !errors.As(err, &usage) {
		t.Fatalf("a 2000-byte query: %v, want a usage error", err)
	}
	if err := SearchCmd(t.Context(), opts, SearchOptions{Query: "FAT", Out: out}); err != nil {
		t.Fatal(err)
	}
	if err := SearchCmd(t.Context(), opts, SearchOptions{Query: "FAT", Out: out}); !errors.As(err, &usage) {
		t.Fatalf("a search over an existing report: %v, want a usage error", err)
	}

	// One stored chunk gone: what reads it cannot be searched.
	blobs, err := filepath.Glob(filepath.Join(dbPath, "blob", "*"))
	if err != nil || len(blobs) == 0 {
		t.Fatalf("no blobs: %v", err)
	}
	for _, b := range blobs {
		if info, err := os.Stat(b); err == nil && info.Mode().IsRegular() {
			if err := os.Remove(b); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	err = SearchCmd(t.Context(), opts, SearchOptions{Query: "FAT", Out: out, Force: true})
	var warn *lifecycle.WarningError
	if !errors.As(err, &warn) || lifecycle.ExitCode(err) != 2 {
		t.Fatalf("a search with a chunk missing: %v (exit %d), want exit 2", err, lifecycle.ExitCode(err))
	}
	body, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(body), `"unreadable"`) || !strings.Contains(string(body), `"problems"`) {
		t.Fatalf("the report does not name what could not be read: %v\n%s", err, body)
	}
}
