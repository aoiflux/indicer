package e2e

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
)

// TestTraceFlag: --trace FILE writes an execution trace of the run, with a
// task per evidence and a region per step of its store, and never replaces
// a file already there.
func TestTraceFlag(t *testing.T) {
	t.Parallel()
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	dir := filepath.Join(work, "case")
	for i := range 3 {
		testutil.WriteRandomFile(t, dir, fmt.Sprintf("ev-%d.bin", i), uint64(300+i), 1<<16+i)
	}
	root := filepath.Join(work, "db")
	path := filepath.Join(work, "store.trace")
	duesMust(t, bin, work, "--trace", path, "store", dir, "--dbpath", root, "-c", "64", "--store-jobs", "2")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b, []byte("go 1.")) {
		t.Fatalf("the trace starts %q, not as a Go execution trace", b[:min(len(b), 16)])
	}
	for _, name := range []string{"store evidence", "open", "fingerprint", "identify", "begin", "chunks", "flush", "commit"} {
		if !traceHolds(b, name) {
			t.Errorf("the trace names no %q", name)
		}
	}

	r := duesRun(t, bin, work, "--trace", path, "list", "--dbpath", root)
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, "exists") {
		t.Fatalf("--trace over a file: exit %d\n%s", r.ExitCode, r.Combined())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, b) {
		t.Fatal("--trace changed the file already there")
	}
}

// traceHolds reports whether trace b's string table holds name. A string
// there follows its length, so a name inside another (a region "open" in a
// function "os.OpenFile") does not count.
func traceHolds(b []byte, name string) bool {
	return bytes.Contains(b, append([]byte{byte(len(name))}, name...))
}
