package testdb

import (
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// MoveChunkMetaToBlockIndex turns a container-mode database into one shaped
// like a legacy hierarchical database: every chunk location moves from its
// C||| key into the block index files.
func MoveChunkMetaToBlockIndex(t testing.TB, db *badger.DB) {
	t.Helper()
	var keys [][]byte
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.ChonkNamespace)
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			keys = append(keys, it.Item().KeyCopy(nil))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var recs []testutil.LegacyBlockRecord
	for _, k := range keys {
		raw, err := dbio.GetNode(k, db)
		if err != nil {
			t.Fatal(err)
		}
		var meta structs.ChonkMetadata
		if err := msgpack.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, testutil.LegacyBlockRecord{
			Hash: k[len(cnst.ChonkNamespace):], Path: meta.Path, Offset: meta.Offset, Size: meta.EncodedSize,
		})
	}
	if len(recs) == 0 {
		t.Fatal("no chunk metadata to move")
	}
	if err := testutil.WriteLegacyBlockIndex(util.BlobPath(db.Opts().Dir), recs); err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(txn *badger.Txn) error {
		for _, k := range keys {
			if err := txn.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
