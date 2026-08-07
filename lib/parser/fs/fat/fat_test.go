package fat

import (
	"bytes"
	"testing"
)

// TestOpenRejectsNonFAT verifies the adapter fails cleanly (no panic) on data
// that is not a FAT volume. Full functional coverage runs against real FAT
// images in the differential corpus (P4).
func TestOpenRejectsNonFAT(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(bytes.NewReader(buf), 0, int64(len(buf))); err == nil {
		t.Fatal("Open on non-FAT data: expected error, got nil")
	}
}
