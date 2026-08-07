package ntfs

import (
	"bytes"
	"testing"
)

// TestOpenRejectsNonNTFS verifies the adapter fails cleanly (no panic) on data
// that is not an NTFS volume. Full functional coverage runs against real NTFS
// images in the differential corpus (P4).
func TestOpenRejectsNonNTFS(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(bytes.NewReader(buf), 0, int64(len(buf))); err == nil {
		t.Fatal("Open on non-NTFS data: expected error, got nil")
	}
}
