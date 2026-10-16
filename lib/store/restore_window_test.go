package store

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"indicer/internal/testutil/testdb"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// TestRestoreReorderWindowIsBounded is the RST-5 regression. Restore writes
// chunks in order but reads them in parallel, parking early arrivals until
// their turn. Nothing limited how far reads ran ahead, so one slow chunk let
// the parked set grow towards the whole file.
func TestRestoreReorderWindowIsBounded(t *testing.T) {
	testdb.Isolate(t)
	util.SetChonkSize(4)
	const chunks = 1500
	meta, db := setupRestoreBenchmarkDataset(t, chunks, 0)

	prevRead, prevNote := readRestoreChunk, notePendingChunks
	t.Cleanup(func() { readRestoreChunk, notePendingChunks = prevRead, prevNote })
	readRestoreChunk = func(m structs.FileMeta, off, end int64, db *badger.DB, key, md []byte) ([]byte, error) {
		if off == 0 {
			time.Sleep(300 * time.Millisecond)
		}
		return prevRead(m, off, end, db, key, md)
	}
	var peak atomic.Int64
	notePendingChunks = func(n int) {
		for {
			p := peak.Load()
			if int64(n) <= p || peak.CompareAndSwap(p, int64(n)) {
				return
			}
		}
	}

	dst, err := os.CreateTemp(t.TempDir(), "restore-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := restoreData(context.Background(), meta, dst, db); err != nil {
		t.Fatalf("restore: %v", err)
	}
	_, window := restoreWindow()
	if p := peak.Load(); p > int64(window) {
		t.Fatalf("%d chunks parked waiting for a slow one; the window is %d", p, window)
	}
	if fi, err := dst.Stat(); err != nil || fi.Size() != meta.Size {
		t.Fatalf("restored %v bytes, want %d (%v)", fi.Size(), meta.Size, err)
	}
}
