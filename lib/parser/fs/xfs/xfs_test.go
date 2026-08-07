package xfs

import (
	"bytes"
	"testing"
)

// TestOpenRejectsNonXFS verifies the adapter fails cleanly (no panic) on data
// that is not an XFS volume. Full functional coverage runs against real XFS
// images in the differential corpus (P4).
func TestOpenRejectsNonXFS(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(bytes.NewReader(buf), 0, int64(len(buf))); err == nil {
		t.Fatal("Open on non-XFS data: expected error, got nil")
	}
}
