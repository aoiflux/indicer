package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/fio"
)

// legacyHierarchicalDB stores two objects into a container database and
// reshapes it into what the retired hierarchical mode left: chunk locations
// in block index files and a container-bidx manifest. It returns the database
// path and the stored files.
func legacyHierarchicalDB(t *testing.T) (string, []string) {
	t.Helper()
	testdb.Apply(t, testdb.Container)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")
	opts := optsForMode(dbPath, matrixChunkKB, testdb.Container)
	var files []string
	for i, n := range []int{3*matrixChunkKB*1024 + 5, matrixChunkKB * 1024} {
		p := testutil.WriteRandomFile(t, dir, "in/obj"+string(rune('a'+i))+".bin", uint64(300+i), n)
		if err := StoreData(t.Context(), opts, p, false, true, false, false); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}

	h, err := openTestDB(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	testdb.MoveChunkMetaToBlockIndex(t, h.DB)
	m := h.Manifest
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	m.Layout.Storage = dbmeta.StorageContainerBIDX
	if err := dbmeta.Save(dbPath, m); err != nil {
		t.Fatal(err)
	}
	return dbPath, files
}

func assertRestores(t *testing.T, dbPath string, files []string) {
	t.Helper()
	for _, f := range files {
		out := filepath.Join(t.TempDir(), filepath.Base(f))
		if err := RestoreData(t.Context(), optsFor(dbPath, 0, nil), plainHashB64(t, f), out, false); err != nil {
			t.Fatalf("restore %s: %v", filepath.Base(f), err)
		}
		if testutil.FileSHA256(t, out) != testutil.FileSHA256(t, f) {
			t.Fatalf("restore %s: content differs", filepath.Base(f))
		}
	}
}

func blockFiles(t *testing.T, dbPath string) []string {
	t.Helper()
	files, err := fio.BlockIndexFiles(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestRepairUpgradeMovesBlockIndex covers `dues repair --upgrade` (D9) on a
// legacy hierarchical database: chunk locations move into the database, the
// manifest becomes container, the block files go only when confirmed, and
// every object restores throughout.
func TestRepairUpgradeMovesBlockIndex(t *testing.T) {
	cases := []struct {
		name        string
		yes         bool
		interactive bool
		answer      string
		wantDeleted bool
	}{
		{"yes", true, false, "", true},
		{"confirmed", false, true, "y\n", true},
		{"declined", false, true, "n\n", false},
		{"not a terminal", false, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dbPath, files := legacyHierarchicalDB(t)
			assertRestores(t, dbPath, files)

			err := repairData(t.Context(), optsFor(dbPath, 0, nil), RepairOptions{Upgrade: true, Yes: c.yes},
				strings.NewReader(c.answer), c.interactive)
			if err != nil {
				t.Fatal(err)
			}
			m, err := dbmeta.Load(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if m.Layout.Storage != dbmeta.StorageContainer {
				t.Fatalf("manifest storage %s after the upgrade, want %s", m.Layout.Storage, dbmeta.StorageContainer)
			}
			if left := blockFiles(t, dbPath); (len(left) == 0) != c.wantDeleted {
				t.Fatalf("%d block files left, want deleted %v", len(left), c.wantDeleted)
			}
			if c.wantDeleted {
				if _, err := os.Stat(filepath.Join(dbPath, cnst.BLOBSDIR, "blocks")); !os.IsNotExist(err) {
					t.Fatalf("empty blocks directory kept: %v", err)
				}
			}
			assertRestores(t, dbPath, files)
		})
	}
}

// TestRepairUpgradeWithoutBlockIndex: on a database with no block index the
// upgrade has nothing to do and changes nothing.
func TestRepairUpgradeWithoutBlockIndex(t *testing.T) {
	testdb.Apply(t, testdb.Container)
	dbPath := filepath.Join(t.TempDir(), "db")
	p := testutil.WriteRandomFile(t, t.TempDir(), "obj.bin", 9, 10000)
	if err := StoreData(t.Context(), optsForMode(dbPath, matrixChunkKB, testdb.Container), p, false, true, false, false); err != nil {
		t.Fatal(err)
	}
	before, err := dbmeta.Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := repairData(t.Context(), optsFor(dbPath, 0, nil), RepairOptions{Upgrade: true}, strings.NewReader(""), false); err != nil {
		t.Fatal(err)
	}
	after, err := dbmeta.Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Layout != before.Layout {
		t.Fatalf("layout changed from %+v to %+v", before.Layout, after.Layout)
	}
	assertRestores(t, dbPath, []string{p})
}
