package hfs

import (
	"bytes"
	"testing"

	"indicer/lib/parser/core"
)

// TestOpenRejectsNonHFS verifies the adapter fails cleanly (no panic) on data
// that is not an HFS volume. Content is checked against ground truth by the
// fixture invariant tests in lib/parser/scan.
func TestOpenRejectsNonHFS(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(core.OpenParams{Reader: bytes.NewReader(buf), Size: int64(len(buf))}); err == nil {
		t.Fatal("Open on non-HFS data: expected error, got nil")
	}
}
