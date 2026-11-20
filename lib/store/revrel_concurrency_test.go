package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// TestRevrelConcurrentIngestsKeepEveryRelation is the STO-14 regression.
// Reverse relations were buffered for the whole evidence and then merged
// into per-chunk segments read in one transaction and written in another, so
// concurrent ingests of evidence sharing chunks overwrote each other's
// relations, and the buffer grew with the evidence. Every ingest must leave
// its relation for every shared chunk.
func TestRevrelConcurrentIngestsKeepEveryRelation(t *testing.T) {
	t.Parallel()
	for name, pass := range chunkPasses {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
			if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			cs := int(dbmeta.ChunkSize(db.DB))
			content := testutil.RandomBytes(41, 6*cs)

			const ingests = 6
			ids := make([][]byte, ingests)
			var wg sync.WaitGroup
			errs := make(chan error, ingests)
			for i := range ingests {
				ids[i] = []byte(fmt.Sprintf("e%d", i))
				p := ChunkPass{DB: db.DB, ID: ids[i], Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true, RevRel: true}
				wg.Go(func() {
					if _, err := pass(t.Context(), p); err != nil {
						errs <- err
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}

			for c := range 6 {
				chash, err := util.GetChonkHash(content[c*cs:(c+1)*cs], dbmeta.ChunkHasher(db.DB)())
				if err != nil {
					t.Fatal(err)
				}
				key := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, int64(c*cs))
				got, err := dbio.GetReverseRelationNode(key, db.DB)
				if err != nil {
					t.Fatalf("chunk %d: %v", c, err)
				}
				for i, id := range ids {
					if _, ok := got[string(id)]; !ok {
						t.Fatalf("chunk %d lost the relation of ingest %d (%d of %d present)", c, i, len(got), ingests)
					}
				}
			}

			// Relations are one key each; no merged segment is written.
			err := db.DB.View(func(txn *badger.Txn) error {
				opts := badger.DefaultIteratorOptions
				opts.Prefix = []byte(cnst.ReverseRelationSegmentNamespace)
				it := txn.NewIterator(opts)
				defer it.Close()
				for it.Rewind(); it.Valid(); it.Next() {
					return fmt.Errorf("segment key %q written", it.Item().Key())
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestChunkPassOptionsArePerPass: reverse relations and signatures are
// options of one pass. Two passes run at once, one with both and one with
// neither, and each writes only what it asked for. They were process
// globals, which TUI stores running side by side could not set apart.
func TestChunkPassOptionsArePerPass(t *testing.T) {
	t.Parallel()
	for name, pass := range chunkPasses {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
			if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			cs := int(dbmeta.ChunkSize(db.DB))
			with, without := testutil.RandomBytes(61, 6*cs), testutil.RandomBytes(62, 6*cs)
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			for _, p := range []ChunkPass{
				{DB: db.DB, ID: []byte("with"), Source: byteReaderAt(with), Size: int64(len(with)), Quiet: true, RevRel: true, Simhash: true},
				{DB: db.DB, ID: []byte("without"), Source: byteReaderAt(without), Size: int64(len(without)), Quiet: true},
			} {
				wg.Go(func() {
					if _, err := pass(t.Context(), p); err != nil {
						errs <- err
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatal(err)
			}

			for _, c := range []struct {
				id      string
				content []byte
				want    bool
			}{{"with", with, true}, {"without", without, false}} {
				for i := range 6 {
					chash, err := util.GetChonkHash(c.content[i*cs:(i+1)*cs], dbmeta.ChunkHasher(db.DB)())
					if err != nil {
						t.Fatal(err)
					}
					related, err := dbio.ChunkOccursAt(db.DB, chash, int64(i*cs), []byte(c.id))
					if err != nil {
						t.Fatal(err)
					}
					_, err = dbio.GetChonkSignature(chash, db.DB)
					if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
						t.Fatal(err)
					}
					if signed := err == nil; related != c.want || signed != c.want {
						t.Errorf("pass %q, chunk %d: reverse relation %t, signature %t; want both %t", c.id, i, related, signed, c.want)
					}
				}
			}
		})
	}
}
