package store

import (
	"bytes"
	"sync/atomic"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/fio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// TestReingestFindsChunksInLegacyBlockIndex is the STO-21 regression.
// Hierarchical mode kept chunk locations only in its block index, and ingest
// looked for stored chunks only in C|||, so every session stored every chunk
// again. Ingest must find chunks a legacy block index holds, and reads must
// still reach them.
func TestReingestFindsChunksInLegacyBlockIndex(t *testing.T) {
	for name, pass := range chunkPasses {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t, testdb.Container, testdb.Options{ChunkKiB: 4})
			cs := int(dbmeta.ChunkSize(db.DB))
			content := testutil.RandomBytes(11, 6*cs)
			runChunkPass(t, pass, db.DB, "first", content)
			testdb.MoveChunkMetaToBlockIndex(t, db.DB)
			engine.For(db.DB).SetBlockIndex(true)

			prev := materializeChunk
			t.Cleanup(func() { materializeChunk = prev })
			var calls atomic.Int64
			materializeChunk = func(key, data []byte, db *badger.DB, c *fio.ContainerManager) ([]byte, int64, error) {
				calls.Add(1)
				return prev(key, data, db, c)
			}
			runChunkPass(t, pass, db.DB, "second", content)
			if n := calls.Load(); n != 0 {
				t.Fatalf("re-ingest stored %d chunks the block index already holds, want 0", n)
			}

			for i := range 6 {
				chunk := content[i*cs : (i+1)*cs]
				chash, err := util.GetChonkHash(chunk, dbmeta.ChunkHasher(db.DB)())
				if err != nil {
					t.Fatal(err)
				}
				got, err := dbio.GetChonkNode(util.AppendToBytesSlice(cnst.ChonkNamespace, chash), db.DB)
				if err != nil {
					t.Fatalf("chunk %d through the block index: %v", i, err)
				}
				if !bytes.Equal(got, chunk) {
					t.Fatalf("chunk %d through the block index reads back other bytes", i)
				}
			}
		})
	}
}
