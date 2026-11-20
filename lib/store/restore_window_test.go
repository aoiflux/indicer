package store

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
)

// TestRestoreReorderWindowIsBounded is the RST-5 regression. Restore writes
// chunks in order but reads them in parallel, parking early arrivals until
// their turn. Nothing limited how far reads ran ahead, so one slow chunk let
// the parked set grow towards the whole file.
func TestRestoreReorderWindowIsBounded(t *testing.T) {
	const chunks = 1500
	l := setupRestoreBenchmarkDataset(t, chunks, 0, 4)

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
	report, err := restoreLogical(context.Background(), l, &out, RestoreOptions{}, nil, cnst.DefaultTuning())
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

// TestRestoreBufferFitsTheObject: restore's write buffer is allocated whole,
// so it is no larger than the object restored, and never under
// minRestoreBuffer.
func TestRestoreBufferFitsTheObject(t *testing.T) {
	t.Parallel()
	def := cnst.DefaultTuning()
	for _, c := range []struct {
		t    cnst.Tuning
		size int64
		want int
	}{
		{def, 0, minRestoreBuffer},
		{def, 10, minRestoreBuffer},
		{def, 5 * cnst.MB, int(5 * cnst.MB)},
		{def, 10 * cnst.GB, int(256 * cnst.MB)},
		{cnst.Tuning{RestoreBufferMB: 8}, 10 * cnst.GB, int(8 * cnst.MB)},
	} {
		if got := restoreBuffer(c.t, c.size); got != c.want {
			t.Errorf("restoreBuffer(%+v, %d) = %d, want %d", c.t, c.size, got, c.want)
		}
	}
}
