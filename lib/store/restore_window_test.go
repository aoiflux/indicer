package store

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/util"
)

// TestRestoreReorderWindowIsBounded is the RST-5 regression. Restore writes
// chunks in order but reads them in parallel, parking early arrivals until
// their turn. Nothing limited how far reads ran ahead, so one slow chunk let
// the parked set grow towards the whole file.
func TestRestoreReorderWindowIsBounded(t *testing.T) {
	testdb.Isolate(t)
	util.SetChonkSize(4)
	const chunks = 1500
	l := setupRestoreBenchmarkDataset(t, chunks, 0)

	prevRead, prevNote := readRestoreSpan, notePendingChunks
	t.Cleanup(func() { readRestoreSpan, notePendingChunks = prevRead, prevNote })
	readRestoreSpan = func(l *dbio.Logical, s dbio.ChunkSpan) ([]byte, error) {
		if s.Chunk == 0 {
			time.Sleep(300 * time.Millisecond)
		}
		return prevRead(l, s)
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

	var out bytes.Buffer
	report, err := restoreLogical(context.Background(), l, &out, RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	_, window := restoreWindow(l.Evidence().ChunkSize())
	if p := peak.Load(); p > int64(window) {
		t.Fatalf("%d chunks parked waiting for a slow one; the window is %d", p, window)
	}
	if int64(out.Len()) != l.Size() || report.Chunks != chunks {
		t.Fatalf("restored %d bytes in %d chunks, want %d in %d", out.Len(), report.Chunks, l.Size(), chunks)
	}
}
