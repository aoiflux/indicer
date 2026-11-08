package store

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/fio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// TestPassesClaimTheChunksTheyStore: a pass that needs a chunk another pass
// of its database is storing, or has stored and not yet ended, records what
// that pass stored it under and writes nothing; once that pass ends the
// database answers, and a claim that failed lets the next pass write.
func TestPassesClaimTheChunksTheyStore(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	key := []byte("C|||:shared")
	first, second := joinChunkWriter(db.DB), joinChunkWriter(db.DB)

	started, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		meta, written, fresh, err := first.store(key, func() ([]byte, int64, error) {
			close(started)
			<-release
			return []byte("meta"), 10, nil
		})
		if err == nil && (string(meta) != "meta" || written != 10 || !fresh) {
			err = fmt.Errorf("the writer got %q, %d, fresh %v", meta, written, fresh)
		}
		firstDone <- err
	}()
	<-started
	// Whether second finds the write running or done, it must not write.
	secondDone := make(chan error, 1)
	go func() {
		meta, written, fresh, err := second.store(key, func() ([]byte, int64, error) {
			return nil, 0, errors.New("wrote a chunk another pass was storing")
		})
		if err == nil && (string(meta) != "meta" || written != 0 || fresh) {
			err = fmt.Errorf("the second pass got %q, %d, fresh %v; want the first's metadata, 0, not fresh", meta, written, fresh)
		}
		secondDone <- err
	}()
	close(release)
	if err := errors.Join(<-firstDone, <-secondDone); err != nil {
		t.Fatal(err)
	}

	var err error
	first.leave(&err)
	wrote := false
	if _, _, fresh, _ := second.store(key, func() ([]byte, int64, error) { wrote = true; return nil, 0, nil }); !wrote || fresh {
		t.Fatalf("after the first pass ended: asked the database %v, fresh %v; want asked, not fresh", wrote, fresh)
	}
	failed := errors.New("disk gone")
	if _, _, _, err := second.store(key, func() ([]byte, int64, error) { return nil, 0, failed }); !errors.Is(err, failed) {
		t.Fatalf("a failed write gave %v", err)
	}
	if _, _, fresh, err := second.store(key, func() ([]byte, int64, error) { return []byte("again"), 3, nil }); err != nil || !fresh {
		t.Fatalf("after a failed claim: fresh %v, %v; want this pass to write", fresh, err)
	}

	// A pass that finds the claim of a write that then fails writes itself.
	third := joinChunkWriter(db.DB)
	other := []byte("C|||:other")
	found := make(chan struct{})
	claimFound = func() { close(found) }
	t.Cleanup(func() { claimFound = func() {} })
	started = make(chan struct{})
	go func() {
		_, _, _, err := third.store(other, func() ([]byte, int64, error) {
			close(started)
			<-found
			return nil, 0, failed
		})
		firstDone <- err
	}()
	<-started
	go func() {
		meta, _, fresh, err := second.store(other, func() ([]byte, int64, error) { return []byte("mine"), 4, nil })
		if err == nil && (string(meta) != "mine" || !fresh) {
			err = fmt.Errorf("after the claim it found failed, the second pass got %q, fresh %v; want its own write", meta, fresh)
		}
		secondDone <- err
	}()
	if err := <-firstDone; !errors.Is(err, failed) {
		t.Fatalf("the failing write gave %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	third.leave(&err)
	second.leave(&err)
	if err != nil || len(chunkWriters) != 0 {
		t.Fatalf("after both passes left: %v, %d writers open", err, len(chunkWriters))
	}
}

// TestPassesAtOnceShareTheirContainers: in container mode passes of one
// database running at once write through one container manager, so one
// pass's barrier syncs the frame another wrote for a chunk they share. The
// last to leave closes it; the next pass starts another.
func TestPassesAtOnceShareTheirContainers(t *testing.T) {
	db := testdb.Open(t, testdb.Container)
	first, second := joinChunkWriter(db.DB), joinChunkWriter(db.DB)
	if first.containers() == nil || first.containers() != second.containers() {
		t.Fatalf("passes at once write through %p and %p, want one manager", first.containers(), second.containers())
	}
	shared := first.containers()
	var err error
	first.leave(&err)
	second.leave(&err)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := shared.WriteChunkToContainer(chunkKeyOf("late"), func() ([]byte, error) { return []byte("x"), nil }); err == nil {
		t.Fatal("the last pass left the manager open")
	}
	next := joinChunkWriter(db.DB)
	defer next.leave(&err)
	if next.containers() == nil || next.containers() == shared {
		t.Fatal("a pass after the others left reuses their closed manager")
	}
}

func chunkKeyOf(s string) []byte { return append([]byte(cnst.ChonkNamespace), s...) }

// TestPassesAtOnceStoreASharedChunkOnce: passes of one database that run at
// once store a chunk they share once, and only the pass that stored it counts
// it new and written. Each looked the chunk up in the database, where it
// shows only once a pass ends, and stored it: in container mode a frame for
// every pass, which nothing reclaims.
func TestPassesAtOnceStoreASharedChunkOnce(t *testing.T) {
	for name, pass := range chunkPasses {
		for _, m := range []testdb.Mode{testdb.Default, testdb.Container} {
			t.Run(name+"/"+m.Name, func(t *testing.T) {
				db := testdb.Open(t, m, testdb.Options{ChunkKiB: 4})
				if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
					t.Fatal(err)
				}
				cs := int(dbmeta.ChunkSize(db.DB))
				shared := testutil.RandomBytes(51, 6*cs)
				const passes = 6
				results := make([]ChunkResult, passes)
				errs := make(chan error, passes)
				var wg sync.WaitGroup
				for i := range passes {
					// The six shared chunks, then one of the pass's own.
					content := append(append([]byte{}, shared...), testutil.RandomBytes(uint64(60+i), cs)...)
					p := ChunkPass{DB: db.DB, ID: []byte(fmt.Sprintf("e%d", i)), Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true}
					wg.Go(func() {
						res, err := pass(t.Context(), p)
						results[i] = res
						if err != nil {
							errs <- err
						}
					})
				}
				wg.Wait()
				close(errs)
				for err := range errs {
					t.Fatal(err)
				}

				var newBytes, written int64
				for _, r := range results {
					newBytes += r.NewBytes
					written += r.WrittenBytes
				}
				chunks, onDisk := storedChunks(t, db.DB)
				if want := 6 + passes; chunks != want {
					t.Errorf("%d chunks on disk, want %d: each once", chunks, want)
				}
				if want := int64((6 + passes) * cs); newBytes != want || written != onDisk {
					t.Errorf("the passes counted %d new and %d written; want %d new and %d written, what is on disk", newBytes, written, want, onDisk)
				}
			})
		}
	}
}

// storedChunks counts the chunks in the database's blob directory, and the
// bytes they take: blob files, or the frames of containers.
func storedChunks(t *testing.T, db *badger.DB) (n int, bytes int64) {
	t.Helper()
	for _, p := range blobFiles(t, db) {
		if !engine.For(db).Container() {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			n, bytes = n+1, bytes+fi.Size()
			continue
		}
		if err := fio.ScanContainer(p, func(f fio.ContainerFrame) error {
			n, bytes = n+1, bytes+f.Size
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return n, bytes
}
