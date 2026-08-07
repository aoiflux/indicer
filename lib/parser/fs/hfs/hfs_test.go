package hfs

import (
	"bytes"
	"testing"
)

// TestOpenRejectsNonHFS verifies the adapter fails cleanly (no panic) on data
// that is not an HFS+ volume. Full functional coverage runs against real HFS+
// images in the differential corpus (P4).
func TestOpenRejectsNonHFS(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(bytes.NewReader(buf), 0, int64(len(buf))); err == nil {
		t.Fatal("Open on non-HFS data: expected error, got nil")
	}
}
