package ntfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
	"indicer/lib/parser/core"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fsfixture.Cleanup()
	os.Exit(code)
}

// TestOpenRejectsNonNTFS verifies the adapter fails cleanly (no panic) on data
// that is not an NTFS volume. Content is checked against ground truth by the
// fixture invariant tests in lib/parser/scan.
func TestOpenRejectsNonNTFS(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 64*1024)
	if _, err := Open(core.OpenParams{Reader: bytes.NewReader(buf), Size: int64(len(buf))}); err == nil {
		t.Fatal("Open on non-NTFS data: expected error, got nil")
	}
}

// writeTrap is an in-memory image that also implements io.WriterAt, as an
// *os.File opened read-write does. It counts the writes it receives.
type writeTrap struct {
	*bytes.Reader
	writes int
}

func (w *writeTrap) WriteAt(p []byte, off int64) (int, error) {
	w.writes++
	return len(p), nil
}

// TestOpenIsReadOnly checks that the adapter opens evidence read-only even
// when the reader can write, and that walking and reading never writes.
func TestOpenIsReadOnly(t *testing.T) {
	t.Parallel()
	var path string
	for _, m := range fsfixture.All(t) {
		if m.Name == "ntfs/ntfs_fsagen" {
			path = fsfixture.Open(t, m)
		}
	}
	if path == "" {
		t.Fatal("fixture ntfs/ntfs_fsagen not found")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trap := &writeTrap{Reader: bytes.NewReader(raw)}
	fsys, err := Open(core.OpenParams{Reader: trap, Size: int64(len(raw))})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	vol := fsys.(*ntfsFS).vol
	if vol.IsWritable() {
		t.Fatal("volume opened writable over a reader that implements io.WriterAt")
	}
	if off := vol.BaseOffset(); off != 0 {
		t.Fatalf("library base offset %d; the adapter adds the partition base itself", off)
	}
	files := 0
	if err := fsys.Walk(context.Background(), func(n core.Node) error {
		if n.Kind == core.KindFile {
			files++
			if rc, err := fsys.Open(n); err == nil {
				_, _ = io.Copy(io.Discard, rc)
				rc.Close()
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 || trap.writes != 0 {
		t.Fatalf("walk saw %d files and caused %d writes", files, trap.writes)
	}
}
