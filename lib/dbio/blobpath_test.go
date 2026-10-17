package dbio

import (
	"path/filepath"
	"testing"
)

func TestStoredBlobPathRoundTrip(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "kvdb")
	for _, name := range []string{"Zm9v.blob", "ctr-0192.blob"} {
		abs := filepath.Join(root, "blob", name)
		stored := StoredBlobPath(dbDir, abs)
		if stored != "blob/"+name {
			t.Fatalf("StoredBlobPath(%s) = %q, want %q", abs, stored, "blob/"+name)
		}
		if got := ResolveBlobPath(dbDir, stored); got != abs {
			t.Fatalf("ResolveBlobPath(%q) = %s, want %s", stored, got, abs)
		}
		// The root itself, not its kvdb directory, resolves the same.
		if got := ResolveBlobPath(root, stored); got != abs {
			t.Fatalf("ResolveBlobPath from the root = %s, want %s", got, abs)
		}
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.blob")
	if got := StoredBlobPath(dbDir, outside); got != outside {
		t.Fatalf("a path outside the blob directory became %q", got)
	}
}

// TestResolveLegacyBlobPaths is the STO-10 rule for chunk paths written
// before this pass: absolute, from whichever machine and data root wrote
// them. Each resolves to the file of that name in this database's blob
// directory, never to the recorded location, which may be another case.
func TestResolveLegacyBlobPaths(t *testing.T) {
	root := t.TempDir()
	for _, legacy := range []string{
		`C:\Users\analyst\.dues\blob\Zm9v.blob`,
		`\\server\cases\case1\blob\Zm9v.blob`,
		"/home/analyst/.dues/blob/Zm9v.blob",
		"relative/old/blob/Zm9v.blob",
	} {
		want := filepath.Join(root, "blob", "Zm9v.blob")
		if got := ResolveBlobPath(root, legacy); got != want {
			t.Errorf("ResolveBlobPath(%q) = %s, want %s", legacy, got, want)
		}
	}
}
