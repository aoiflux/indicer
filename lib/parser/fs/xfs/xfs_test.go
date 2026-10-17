package xfs

import (
	"bytes"
	"testing"

	"indicer/lib/parser/core"
)

// TestOpenRejectsNonXFS verifies the adapter fails cleanly (no panic) on data
// that is not an XFS volume. Full functional coverage runs against real XFS
// images in the differential corpus (P4).
func TestOpenRejectsNonXFS(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(core.OpenParams{Reader: bytes.NewReader(buf), Size: int64(len(buf))}); err == nil {
		t.Fatal("Open on non-XFS data: expected error, got nil")
	}
}
