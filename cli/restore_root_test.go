package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// TestRestoreByContentRoot: an evidence is known by its content root as
// well as its plain hash, and dues list shows both. A sync evidence's file
// hash is the plain one, and restoring it by its root reported not found.
func TestRestoreByContentRoot(t *testing.T) {
	dir := t.TempDir()
	opts := optsFor(filepath.Join(dir, "db"), matrixChunkKB, nil)
	src := testutil.WriteRandomFile(t, dir, "evidence.bin", 23, 3*matrixChunkKB*1024+5)
	if err := StoreData(t.Context(), opts, src, false, true, false, false); err != nil {
		t.Fatalf("store: %v", err)
	}

	h, err := Common(t.Context(), opts, dbopen.ReadOnly, false)
	if err != nil {
		t.Fatal(err)
	}
	var rec structs.EvidenceFile
	err = h.DB.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{Prefix: []byte(cnst.EviFileNamespace)})
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			data, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			if err := dbio.DecodeRecord(data, &rec); err != nil {
				return err
			}
		}
		return nil
	})
	closeDB(h, &err)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ContentRoot == "" || rec.ContentRoot == rec.FileHash {
		t.Fatalf("want a sync evidence whose file hash is not its root: file hash %q, root %q", rec.FileHash, rec.ContentRoot)
	}

	out := filepath.Join(t.TempDir(), "restored.bin")
	if err := RestoreData(t.Context(), opts, rec.ContentRoot, out, RestoreOptions{}); err != nil {
		t.Fatalf("restore by the content root: %v", err)
	}
	if got, want := testutil.FileSHA256(t, out), testutil.FileSHA256(t, src); got != want {
		t.Fatalf("restored %s, want %s", got, want)
	}

	// A hash that names nothing says so, rather than "Key not found".
	unknown := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	err = RestoreData(t.Context(), opts, unknown, filepath.Join(t.TempDir(), "x.bin"), RestoreOptions{})
	if !errors.Is(err, badger.ErrKeyNotFound) || !strings.Contains(err.Error(), "no stored object has hash "+unknown) {
		t.Fatalf("restore of an unknown hash: %v", err)
	}
}
