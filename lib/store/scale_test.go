package store

import (
	"runtime"
	"runtime/debug"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/zeebo/blake3"
)

// TestValidateFourMillionChunks: validating evidence of four million chunks
// (16 GiB in 4 KiB chunks; a 1 TiB image in 256 KiB chunks) streams its
// relations and keeps the heap flat. Validation used to hold every chunk
// hash of the evidence at once (ING-10).
func TestValidateFourMillionChunks(t *testing.T) {
	testutil.LongTest(t)
	// Low-resource mode caps badger's caches at 96 MiB, so what the heap
	// holds beyond them is validation's.
	testdb.Apply(t, testdb.Default)
	cnst.MEMOPT = true
	db := testdb.OpenAt(t, testdb.Default, testdb.NewDataRoot(t))
	util.SetChonkSize(4)
	if err := util.EnsureBlobPath(db.Root); err != nil {
		t.Fatal(err)
	}
	const n = 4 << 20
	cs := int64(cnst.ChonkSize)
	data := testutil.RandomBytes(7, int(cs))
	sum := blake3.Sum256(data)
	ckey := append([]byte(cnst.ChonkNamespace), sum[:]...)
	meta, err := dbio.MaterializeChonkNode(ckey, data, db.DB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := dbio.SetNode(ckey, meta, db.DB); err != nil {
		t.Fatal(err)
	}

	eid := []byte("evidence-4M-chnk")
	wb := db.DB.NewWriteBatch()
	for i := range int64(n) {
		key := util.AppendToBytesSlice(cnst.RelationNamespace, eid, cnst.DataSeperator, i*cs)
		if err := wb.Set(key, sum[:]); err != nil {
			t.Fatal(err)
		}
	}
	if err := wb.Flush(); err != nil {
		t.Fatal(err)
	}

	var ev structs.EvidenceFile
	ev.Size, ev.IngestState = n*cs, structs.IngestStateFlushed
	// Collect often, so the heap sampled is mostly live data, not garbage.
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	base := m.HeapAlloc
	peak := testutil.PeakHeap(func() {
		err = ValidateEvidence(t.Context(), db.DB, eid, ev, ValidateMaterial)
	})
	if err != nil {
		t.Fatal(err)
	}
	// badger's caches, capped at 96 MiB, and their bookkeeping take most of
	// this (about 176 MiB in all on Windows); four million chunk hashes held
	// at once would add some 400 MiB.
	if growth := int64(peak) - int64(base); growth > 256<<20 {
		t.Fatalf("validation grew the heap by %d MiB", growth>>20)
	}
}
