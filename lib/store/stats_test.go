package store

import (
	"context"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/util"
)

// TestStatsWithReverseRelations (STO-24): statistics of a database stored
// with reverse relations count the chunks two evidence share. They failed
// with "invalid reverse-relation append key": the keys written since W3 have
// three parts, not four.
func TestStatsWithReverseRelations(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	util.SetChonkSize(4)
	cnst.ENABLEREVREL = true
	cs := int(cnst.ChonkSize)
	a := testutil.RandomBytes(51, 6*cs)
	b := append(append([]byte{}, a[:3*cs]...), testutil.RandomBytes(52, 3*cs)...)
	runChunkPass(t, ingestChunksBatchOwner, db.DB, "a", a)
	runChunkPass(t, ingestChunksBatchOwner, db.DB, "b", b)

	s, err := GatherStats(t.Context(), db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if s.SharedChunks != 3 || s.UniqueChunks != 9 || s.TotalChunkRefs != 12 || s.ChunkSize != int64(cs) {
		t.Fatalf("stats: shared %d (want 3), unique %d (want 9), references %d (want 12), chunk size %d", s.SharedChunks, s.UniqueChunks, s.TotalChunkRefs, s.ChunkSize)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := GatherStats(ctx, db.DB); err == nil {
		t.Fatal("cancelled statistics succeeded")
	}
}
