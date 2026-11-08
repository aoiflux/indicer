package store

import (
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
)

// TestPassMemory: a chunk pass holds its feed ring, which a small source
// sizes to its chunks (at least two), and a chunk for each worker that can
// have one; the staged pipeline also queues encoded chunks for its writer.
func TestPassMemory(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default)
	cs := dbmeta.ChunkSize(db.DB)
	e := engine.For(db.DB)
	workers, queue := e.Tuning().StorePipeline(e.Container())
	w := int64(min(workers, 2))
	ring := int64(feedSlots(int(w), cs))
	if ring <= 3 {
		t.Fatalf("a ring of %d slots leaves no small source to size", ring)
	}
	for _, c := range []struct {
		name string
		size int64
		want int64
	}{
		{"empty", 0, (2 + 1) * cs},
		{"one byte", 1, (2 + 1) * cs},
		{"three chunks", 3 * cs, (3 + min(w, 3)) * cs},
		{"larger than the ring", 1 << 40, (ring + w) * cs},
	} {
		if got := PassMemory(db.DB, c.size, 2); got != c.want {
			t.Errorf("%s: PassMemory = %d chunks, want %d", c.name, got/cs, c.want/cs)
		}
	}
	if full, capped := PassMemory(db.DB, 1<<40, 0), PassMemory(db.DB, 1<<40, 2); workers > 2 && full <= capped {
		t.Errorf("every worker holds %d chunks, two hold %d", full/cs, capped/cs)
	}

	// 64 MiB chunks: a ring of four slots, so no more than four workers are
	// busy, however many there are.
	db.Reconfigure(t, testdb.Options{ChunkKiB: 64 << 10})
	big := dbmeta.ChunkSize(db.DB)
	if got, want := PassMemory(db.DB, 1<<40, 0), (4+min(int64(workers), 4))*big; got != want {
		t.Errorf("64 MiB chunks: PassMemory = %d chunks, want %d", got/big, want/big)
	}

	staged := testdb.Tuning()
	staged.Pipeline = cnst.StorePipelineStaged
	db.Reconfigure(t, testdb.Options{ChunkKiB: int(cs >> 10), Tuning: &staged})
	if got, want := PassMemory(db.DB, 1<<40, 2), (ring+w+int64(queue))*cs; got != want {
		t.Errorf("staged: PassMemory = %d chunks, want %d", got/cs, want/cs)
	}
}
