package dbopen

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"

	"github.com/dgraph-io/badger/v4"
)

// newRoot creates a closed database at a fresh data root, plus a user file
// and directory that are not DUES's.
func newRoot(t *testing.T) string {
	t.Helper()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{cnst.GRAPHDIR, cnst.FTSDIR, "mine"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestResetDeletesOnlyDUESEntries is the ING-16 regression: reset removed the
// whole directory it was given, whatever else it held.
func TestResetDeletesOnlyDUESEntries(t *testing.T) {
	root := newRoot(t)
	_, removed, err := Reset(root)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, name := range []string{cnst.KVDBDIR, cnst.BLOBSDIR, cnst.GRAPHDIR, cnst.FTSDIR, dbmeta.ManifestName, LockName} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset: %v", name, err)
		}
		if !slices.Contains(removed, name) {
			t.Errorf("%s not reported removed: %v", name, removed)
		}
	}
	for _, name := range []string{"notes.txt", "mine"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("reset deleted %s, which DUES does not own: %v", name, err)
		}
	}
}

func TestResetRemovesAnEmptiedRoot(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reset(filepath.Join(root, cnst.KVDBDIR)); err != nil {
		t.Fatalf("Reset by kvdb path: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("emptied root kept: %v", err)
	}
}

func TestResetRefusals(t *testing.T) {
	t.Run("not a database", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "precious.doc"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Reset(dir); !errors.Is(err, ErrResetRefused) {
			t.Fatalf("Reset of a non-DUES directory = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "precious.doc")); err != nil {
			t.Fatal("refused reset deleted a file")
		}
	})
	t.Run("filesystem root", func(t *testing.T) {
		vol := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
		if err := CheckResettable(vol); !errors.Is(err, ErrResetRefused) {
			t.Fatalf("CheckResettable(%s) = %v", vol, err)
		}
	})
	// The home and working directories are refused even when they hold a
	// database, and so is any directory containing them.
	t.Run("working directory", func(t *testing.T) {
		root := newRoot(t)
		t.Chdir(filepath.Join(root, "mine"))
		if _, _, err := Reset(root); !errors.Is(err, ErrResetRefused) {
			t.Fatalf("Reset of the directory holding the CWD = %v", err)
		}
		if !dbmeta.HasDatabase(root) {
			t.Fatal("refused reset deleted the database")
		}
	})
	t.Run("home directory", func(t *testing.T) {
		root := newRoot(t)
		t.Setenv("HOME", root)
		t.Setenv("USERPROFILE", root)
		if home, _ := os.UserHomeDir(); home != root {
			t.Skipf("cannot point the home directory at a test root (%s)", home)
		}
		if _, _, err := Reset(root); !errors.Is(err, ErrResetRefused) {
			t.Fatalf("Reset of the home directory = %v", err)
		}
	})
}

func TestResetRefusedWhileOpen(t *testing.T) {
	root := newRoot(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	if _, _, err := Reset(root); !errors.Is(err, ErrInUse) {
		t.Fatalf("Reset of an open database = %v, want ErrInUse", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !dbmeta.HasDatabase(root) {
		t.Fatal("refused reset deleted the database")
	}
}

// TestHandleResetEmptiesAndStaysUsable covers the TUI's reset: the database
// stays open and keeps its layout, but its contents, blobs, graph and
// full-text index are gone. The TUI used to drop badger and blobs only,
// leaving the graph and full-text index describing deleted evidence.
func TestHandleResetEmptiesAndStaysUsable(t *testing.T) {
	root := newRoot(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	storeChunk(t, h.DB, []byte("some chunk content to drop"))
	if err := os.WriteFile(filepath.Join(root, cnst.GRAPHDIR, "graphene.wal"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := h.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	var keys []string
	if err := h.DB.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{})
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			keys = append(keys, string(it.Item().Key()))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{dbmeta.MarkerKey}) {
		t.Fatalf("database after reset holds %q, want only the marker", keys)
	}
	if _, err := os.Stat(filepath.Join(root, cnst.GRAPHDIR)); !os.IsNotExist(err) {
		t.Fatalf("graph survived the reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err != nil {
		t.Fatal("reset deleted a file DUES does not own")
	}
	storeChunk(t, h.DB, []byte("written after the reset"))
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	h2, err := Open(context.Background(), Request{Path: root, Mode: ReadOnly, KDF: &fastKDF})
	if err != nil {
		t.Fatalf("reopen after reset: %v", err)
	}
	h2.Close()
}
