package cli

import (
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
)

// TestStoreRestoreAfterRelocation is the STO-10 regression. Chunk paths were
// stored absolute, so a data root that was moved, copied to another machine
// or opened through another drive letter could not restore anything, and a
// copy could read another case's blobs where the old path still existed.
func TestStoreRestoreAfterRelocation(t *testing.T) {
	testdb.ForEachMode(t, func(t *testing.T, m testdb.Mode) {
		dir := t.TempDir()
		oldRoot := filepath.Join(dir, "case-a", "db")
		src := testutil.WriteRandomFile(t, dir, "in/obj.bin", 77, 3*matrixChunkKB*1024+11)
		if err := StoreData(t.Context(), optsForMode(oldRoot, matrixChunkKB, m), src, false, true, false, false); err != nil {
			t.Fatal(err)
		}

		if err := os.Rename(filepath.Join(dir, "case-a"), filepath.Join(dir, "case-b")); err != nil {
			t.Fatal(err)
		}
		newRoot := filepath.Join(dir, "case-b", "db")
		opts := optsFor(newRoot, 0, []byte(m.Password))
		out := filepath.Join(dir, "out.bin")
		if err := RestoreData(t.Context(), opts, plainHashB64(t, src), out, false); err != nil {
			t.Fatalf("restore after moving the data root: %v", err)
		}
		if testutil.FileSHA256(t, out) != testutil.FileSHA256(t, src) {
			t.Fatal("restore after moving the data root: content differs")
		}

		// Storing the same content again finds every chunk in place.
		blobsBefore := countFiles(t, filepath.Join(newRoot, "blob"))
		copyPath := filepath.Join(dir, "in", "copy.bin")
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := StoreData(t.Context(), opts, copyPath, false, true, false, false); err != nil {
			t.Fatal(err)
		}
		if after := countFiles(t, filepath.Join(newRoot, "blob")); after != blobsBefore {
			t.Fatalf("re-ingest after the move wrote %d new blob files", after-blobsBefore)
		}
	})
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
