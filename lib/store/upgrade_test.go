package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// legacyBlockIndexDB stores content into a container database and moves its
// chunk locations into a block index, as a database written in the retired
// hierarchical mode holds them. It returns the database and the chunk keys.
func legacyBlockIndexDB(t *testing.T, chunks int) (*testdb.DB, []byte, [][]byte) {
	t.Helper()
	db := testdb.Open(t, testdb.Container, testdb.Options{ChunkKiB: 4})
	cs := int(dbmeta.ChunkSize(db.DB))
	content := testutil.RandomBytes(21, chunks*cs)
	runChunkPass(t, ingestChunksBatchOwner, db.DB, "legacy", content)
	testdb.MoveChunkMetaToBlockIndex(t, db.DB)
	engine.For(db.DB).SetBlockIndex(true)

	var keys [][]byte
	for i := range chunks {
		chash, err := util.GetChonkHash(content[i*cs:(i+1)*cs], dbmeta.ChunkHasher(db.DB)())
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, util.AppendToBytesSlice(cnst.ChonkNamespace, chash))
	}
	return db, content, keys
}

// assertChunksInBadger checks that every chunk has C||| metadata and reads
// back without the block index.
func assertChunksInBadger(t *testing.T, db *badger.DB, content []byte, keys [][]byte) {
	t.Helper()
	engine.For(db).SetBlockIndex(false)
	defer engine.For(db).SetBlockIndex(true)
	cs := int(dbmeta.ChunkSize(db))
	for i, k := range keys {
		got, err := dbio.GetChonkNode(k, db)
		if err != nil {
			t.Fatalf("chunk %d without the block index: %v", i, err)
		}
		if !bytes.Equal(got, content[i*cs:(i+1)*cs]) {
			t.Fatalf("chunk %d reads back other bytes", i)
		}
	}
}

// TestUpgradeBlockIndexMovesRecords covers D9's `repair --upgrade`: every
// block index record becomes C||| metadata, checked against its chunk, and a
// second run finds nothing left to do.
func TestUpgradeBlockIndexMovesRecords(t *testing.T) {
	t.Parallel()
	db, content, keys := legacyBlockIndexDB(t, 8)

	report, err := UpgradeBlockIndex(t.Context(), db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete() || report.Migrated != len(keys) || report.Records != len(keys) || report.Files == 0 {
		t.Fatalf("upgrade report %+v, want %d records migrated", report, len(keys))
	}
	assertChunksInBadger(t, db.DB, content, keys)

	again, err := UpgradeBlockIndex(t.Context(), db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if again.Migrated != 0 || again.AlreadyStored != len(keys) || !again.Complete() {
		t.Fatalf("second upgrade %+v, want every record already stored", again)
	}
}

// TestUpgradeBlockIndexKeepsDamagedRecords checks that a record whose chunk
// does not verify is reported and not written to C|||, while the others are.
func TestUpgradeBlockIndexKeepsDamagedRecords(t *testing.T) {
	t.Parallel()
	db, content, keys := legacyBlockIndexDB(t, 6)
	loc, err := dbio.LocateChunk(keys[2], db.DB)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(dbio.ResolveBlobPath(db.Opts().Dir, loc.Path), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xA5}, 16), loc.Offset+8); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := UpgradeBlockIndex(t.Context(), db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete() || report.DamagedCount != 1 || report.Migrated != len(keys)-1 {
		t.Fatalf("upgrade report %+v, want 1 damaged and %d migrated", report, len(keys)-1)
	}
	if _, err := dbio.GetNode(keys[2], db.DB); !errors.Is(err, badger.ErrKeyNotFound) {
		t.Fatalf("damaged chunk got C||| metadata: %v", err)
	}
	healthy := append(append([][]byte{}, keys[:2]...), keys[3:]...)
	cs := int(dbmeta.ChunkSize(db.DB))
	healthyContent := append(append([]byte{}, content[:2*cs]...), content[3*cs:]...)
	assertChunksInBadger(t, db.DB, healthyContent, healthy)
}

// TestUpgradeBlockIndexCancelled checks that a cancelled upgrade stops and
// says so.
func TestUpgradeBlockIndexCancelled(t *testing.T) {
	t.Parallel()
	db, _, _ := legacyBlockIndexDB(t, 4)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := UpgradeBlockIndex(ctx, db.DB); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled upgrade returned %v", err)
	}
}
