package dbopen

import (
	"context"
	"errors"
	"testing"
)

// TestSnapshotKeepsWritersOut: while a snapshot holds the root, a writable
// open is refused and a read-only one is not; after Release a writer opens
// again. A snapshot is refused while a writer holds the root.
func TestSnapshotKeepsWritersOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	h, err := Open(ctx, Request{Path: root, Mode: ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TakeSnapshot(ctx, root); !errors.Is(err, ErrInUse) {
		t.Errorf("snapshot beside a writer: %v, want %v", err, ErrInUse)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := TakeSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Manifest.Format != "dues-db" {
		t.Errorf("manifest = %+v", s.Manifest)
	}
	if _, err := Open(ctx, Request{Path: root, Mode: ReadWrite}); !errors.Is(err, ErrInUse) {
		t.Errorf("writer beside a snapshot: %v, want %v", err, ErrInUse)
	}
	r, err := Open(ctx, Request{Path: root, Mode: ReadOnly})
	if err != nil {
		t.Fatalf("reader beside a snapshot: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	w, err := Open(ctx, Request{Path: root, Mode: ReadWrite})
	if err != nil {
		t.Fatalf("writer after release: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
