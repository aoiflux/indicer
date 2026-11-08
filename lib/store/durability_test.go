package store

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbmeta"
	"indicer/lib/fio"

	"github.com/dgraph-io/badger/v4"
)

// TestChunkMetaVisibleOnlyAfterSync is the STO-5 regression. A chunk's C|||
// metadata went into the write batch, which commits on its own as it fills,
// as soon as the chunk was written. Container bytes were synced only when the
// container was rotated or closed, and the names of new blobs never, so a
// power loss could leave metadata that a later ingest deduplicates against
// pointing at bytes that never reached the disk.
//
// Every chunk materialised before a sync of the chunk material started is
// covered by it; metadata for any other chunk must not reach the batch.
func TestChunkMetaVisibleOnlyAfterSync(t *testing.T) {
	for name, pass := range chunkPasses {
		for _, m := range []testdb.Mode{testdb.Default, testdb.Container} {
			t.Run(name+"/"+m.Name, func(t *testing.T) {
				db := testdb.Open(t, m, testdb.Options{ChunkKiB: 4})
				content := testutil.RandomBytes(9, 40*int(dbmeta.ChunkSize(db.DB)))

				var (
					mu       sync.Mutex
					produced = map[string]int64{}
					seq      atomic.Int64
					synced   atomic.Int64
					syncs    atomic.Int64
					sets     atomic.Int64
				)
				prevMaterialize, prevSync, prevSet := materializeChunk, syncChunkMaterial, setChunkMeta
				t.Cleanup(func() {
					materializeChunk, syncChunkMaterial, setChunkMeta = prevMaterialize, prevSync, prevSet
				})
				materializeChunk = func(key, data []byte, db *badger.DB, c *fio.ContainerManager) ([]byte, int64, error) {
					meta, written, err := prevMaterialize(key, data, db, c)
					if err == nil {
						mu.Lock()
						produced[string(key)] = seq.Add(1)
						mu.Unlock()
					}
					return meta, written, err
				}
				syncChunkMaterial = func(dir string, c *fio.ContainerManager) error {
					syncs.Add(1)
					covered := seq.Load()
					if err := prevSync(dir, c); err != nil {
						return err
					}
					synced.Store(covered)
					return nil
				}
				setChunkMeta = func(db *badger.DB, key, val []byte, batch *badger.WriteBatch) error {
					sets.Add(1)
					mu.Lock()
					n, ok := produced[string(key)]
					mu.Unlock()
					if !ok {
						t.Errorf("metadata set for a chunk that was never materialised")
					} else if n > synced.Load() {
						t.Errorf("metadata for chunk %d reached the batch before a sync covered it (synced through %d)", n, synced.Load())
					}
					return prevSet(db, key, val, batch)
				}

				runChunkPass(t, pass, db.DB, "evidence", content)
				if sets.Load() != 40 {
					t.Fatalf("%d chunk metadata entries set, want 40", sets.Load())
				}
				if syncs.Load() == 0 {
					t.Fatal("chunk material was never synced")
				}
			})
		}
	}
}

// TestChunkMetaBarrierThresholds checks when the barrier releases pending
// metadata: on the chunk count, on the bytes those chunks hold, after the
// delay since the oldest pending entry, and on an explicit fire. Nothing is
// released when the sync fails.
func TestChunkMetaBarrierThresholds(t *testing.T) {
	newBarrier := func(syncErr error) (*chunkMetaBarrier, *testutil.FakeClock, *int, *[]string) {
		clock := testutil.NewFakeClock(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
		syncs, set := 0, []string{}
		b := &chunkMetaBarrier{
			now:  clock.Now,
			sync: func() error { syncs++; return syncErr },
			set:  func(key, _ []byte) error { set = append(set, string(key)); return nil },
		}
		return b, clock, &syncs, &set
	}
	key := func(i int) []byte { return []byte{byte(i >> 8), byte(i)} }

	t.Run("chunks", func(t *testing.T) {
		b, _, syncs, set := newBarrier(nil)
		for i := 0; i < barrierMaxChunks-1; i++ {
			if err := b.add(key(i), []byte("m"), 1); err != nil {
				t.Fatal(err)
			}
		}
		if *syncs != 0 || len(*set) != 0 {
			t.Fatalf("released early: %d syncs, %d set", *syncs, len(*set))
		}
		if err := b.add(key(barrierMaxChunks), []byte("m"), 1); err != nil {
			t.Fatal(err)
		}
		if *syncs != 1 || len(*set) != barrierMaxChunks {
			t.Fatalf("at %d chunks: %d syncs, %d set", barrierMaxChunks, *syncs, len(*set))
		}
	})

	t.Run("bytes", func(t *testing.T) {
		b, _, syncs, set := newBarrier(nil)
		if err := b.add(key(1), []byte("m"), barrierMaxBytes-1); err != nil {
			t.Fatal(err)
		}
		if *syncs != 0 {
			t.Fatal("released below the byte threshold")
		}
		if err := b.add(key(2), []byte("m"), 1); err != nil {
			t.Fatal(err)
		}
		if *syncs != 1 || len(*set) != 2 {
			t.Fatalf("at the byte threshold: %d syncs, %d set", *syncs, len(*set))
		}
	})

	t.Run("delay", func(t *testing.T) {
		b, clock, syncs, set := newBarrier(nil)
		if err := b.tick(); err != nil || *syncs != 0 {
			t.Fatalf("empty barrier synced: %v, %d syncs", err, *syncs)
		}
		if err := b.add(key(1), []byte("m"), 1); err != nil {
			t.Fatal(err)
		}
		clock.Advance(barrierMaxDelay - 1)
		if err := b.tick(); err != nil || *syncs != 0 {
			t.Fatalf("released before the delay: %v, %d syncs", err, *syncs)
		}
		clock.Advance(1)
		if err := b.tick(); err != nil {
			t.Fatal(err)
		}
		if *syncs != 1 || len(*set) != 1 {
			t.Fatalf("after the delay: %d syncs, %d set", *syncs, len(*set))
		}
	})

	t.Run("fire", func(t *testing.T) {
		b, _, syncs, set := newBarrier(nil)
		if err := b.fire(); err != nil || *syncs != 0 {
			t.Fatalf("empty fire synced: %v, %d syncs", err, *syncs)
		}
		if err := b.add(key(1), []byte("m"), 1); err != nil {
			t.Fatal(err)
		}
		if err := b.fire(); err != nil {
			t.Fatal(err)
		}
		if *syncs != 1 || len(*set) != 1 {
			t.Fatalf("explicit fire: %d syncs, %d set", *syncs, len(*set))
		}
	})

	t.Run("sync error", func(t *testing.T) {
		boom := errors.New("disk gone")
		b, _, _, set := newBarrier(boom)
		if err := b.add(key(1), []byte("m"), 1); err != nil {
			t.Fatal(err)
		}
		if err := b.fire(); !errors.Is(err, boom) {
			t.Fatalf("fire returned %v, want the sync error", err)
		}
		if len(*set) != 0 {
			t.Fatalf("%d entries released after a failed sync", len(*set))
		}
	})
}
