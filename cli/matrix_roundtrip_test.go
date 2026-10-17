package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/util"
)

// matrixChunkKB is small so the boundary sizes stay small.
const matrixChunkKB = 4

// TestMatrixStoreRestoreRoundTrip stores raw objects whose sizes sit on and
// around chunk boundaries into one database per storage mode, then restores
// each and compares bytes. Two sessions write into the same database, which
// is where container naming (STO-1) goes wrong.
func TestMatrixStoreRestoreRoundTrip(t *testing.T) {
	cs := matrixChunkKB * 1024
	sizes := []int{1, cs - 1, cs, cs + 1, 3*cs + 17}
	testdb.ForEachMode(t, func(t *testing.T, m testdb.Mode) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "db")
		util.SetChonkSize(matrixChunkKB)

		type stored struct {
			path, hash string
			size       int
		}
		var objs []stored
		for i, n := range sizes {
			p := testutil.WriteRandomFile(t, dir, "in/obj"+string(rune('a'+i))+".bin", uint64(100+i), n)
			// Each object is a separate store session, as separate CLI runs are.
			if err := StoreData(t.Context(), optsForMode(dbPath, matrixChunkKB, m), p, false, true, false, false); err != nil {
				t.Fatalf("store %d bytes: %v", n, err)
			}
			objs = append(objs, stored{path: p, hash: plainHashB64(t, p), size: n})
		}

		for _, o := range objs {
			t.Run(filepath.Base(o.path), func(t *testing.T) {
				out := filepath.Join(dir, "out", filepath.Base(o.path))
				if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := RestoreData(t.Context(), optsForMode(dbPath, matrixChunkKB, m), o.hash, out, RestoreOptions{}); err != nil {
					t.Fatalf("restore %d-byte object: %v", o.size, err)
				}
				if testutil.FileSHA256(t, out) != testutil.FileSHA256(t, o.path) {
					t.Fatalf("restore %d-byte object: content differs", o.size)
				}
			})
		}
	})
}

func plainHashB64(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h, err := util.GetFileHash(f, cnst.GetHashAlgo(true))
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(h)
}
