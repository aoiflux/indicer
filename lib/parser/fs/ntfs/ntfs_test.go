package ntfs

import (
	"bytes"
	"os"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fsfixture.Cleanup()
	os.Exit(code)
}

// TestOpenRejectsNonNTFS verifies the adapter fails cleanly (no panic) on data
// that is not an NTFS volume. Full functional coverage runs against real NTFS
// images in the differential corpus (P4).
func TestOpenRejectsNonNTFS(t *testing.T) {
	buf := make([]byte, 64*1024)
	if _, err := Open(bytes.NewReader(buf), 0, int64(len(buf))); err == nil {
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
	fsys, err := Open(trap, 0, int64(len(raw)))
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
	if err := fsys.Walk(func(n core.Node) error {
		if n.Kind == core.KindFile {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if files == 0 || trap.writes != 0 {
		t.Fatalf("walk saw %d files and caused %d writes", files, trap.writes)
	}
}
