package ext

import (
	"bytes"
	"testing"
)

// TestOpenRejectsNonExt verifies the adapter fails cleanly (no panic) on data
// that is not an ext volume. Full functional coverage runs against real ext
// images in the differential corpus (P4).
func TestOpenRejectsNonExt(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(bytes.NewReader(buf), 0, int64(len(buf))); err == nil {
		t.Fatal("Open on non-ext data: expected error, got nil")
	}
}
