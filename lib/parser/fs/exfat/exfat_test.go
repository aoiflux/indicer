package exfat

import (
	"bytes"
	"os"
	"regexp"
	"testing"

	"indicer/lib/parser/core"
)

// TestOpenRejectsNonExFAT verifies the adapter fails cleanly (no panic) on data
// that is not an exFAT volume. Content is checked against ground truth by the
// fixture invariant tests in lib/parser/scan.
func TestOpenRejectsNonExFAT(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 64*1024)
	if _, err := Open(core.OpenParams{Reader: bytes.NewReader(buf), Size: int64(len(buf))}); err == nil {
		t.Fatal("Open on non-exFAT data: expected error, got nil")
	}
}

// TestNoUnboundedClusterLists: ClusterList and allocations sized from an
// entry's claimed size let a crafted volume exhaust memory (PAR-7). The
// adapter goes through FragmentOffsetsWithOptions, which bounds both.
func TestNoUnboundedClusterLists(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("exfat.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{`\.ClusterList\(`, `make\(\[\]byte, *(size|e\.Size\(\))`, `ContiguousFilePaths`, `AllEntries\(`} {
		if regexp.MustCompile(re).Match(src) {
			t.Errorf("exfat.go matches %s", re)
		}
	}
}
