package store

import (
	"strconv"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbmeta"
	"indicer/lib/util"
)

// adjusted checks what each item's evidence pass is to add, item: {new,
// written}.
func adjusted(t *testing.T, l *ChunkCredits, want map[int][2]int64) {
	t.Helper()
	for item, w := range want {
		if n, written := l.Adjust(item); n != w[0] || written != w[1] {
			t.Fatalf("item %d is to add %d new and %d written, want %d and %d", item, n, written, w[0], w[1])
		}
	}
}

// TestChunkCreditsGoToTheFirstInOrder: a stored chunk goes, with its bytes,
// to each pass before its owner that needs it. Within an item the evidence's
// own pass comes before those of the objects its index stores, and the pass
// of an object counts no bytes of its own.
func TestChunkCreditsGoToTheFirstInOrder(t *testing.T) {
	l := NewChunkCredits()
	key := []byte("C|||:shared")
	l.stored(key, Credit{Item: 2}, 10, 4)

	l.found(key, Credit{Item: 3})
	adjusted(t, l, map[int][2]int64{2: {0, 0}, 3: {0, 0}})
	l.found(key, Credit{Item: 1, Object: true})
	adjusted(t, l, map[int][2]int64{1: {0, 0}, 2: {-10, -4}})
	l.found(key, Credit{Item: 1})
	adjusted(t, l, map[int][2]int64{1: {10, 4}, 2: {-10, -4}})
	l.found(key, Credit{Item: 1, Object: true})
	adjusted(t, l, map[int][2]int64{1: {10, 4}})
	l.found(key, Credit{Item: 0})
	adjusted(t, l, map[int][2]int64{0: {10, 4}, 1: {0, 0}, 2: {-10, -4}})
}

// TestChunkCreditsCountTheirLocks: Stats counts the passes that took the
// ledger's lock: none for the first unsettled item while nothing is held,
// one for each store ahead of it and each find while chunks are held.
func TestChunkCreditsCountTheirLocks(t *testing.T) {
	if (*ChunkCredits)(nil).Stats() != (CreditStats{}) {
		t.Fatal("no ledger has stats")
	}
	l := NewChunkCredits()
	l.stored([]byte("C|||:first"), Credit{Item: 0}, 10, 4)
	l.found([]byte("C|||:first"), Credit{Item: 0})
	if st := l.Stats(); st.Locked != 0 || st.Held != 0 {
		t.Fatalf("first item alone: %+v, want no locks and nothing held", st)
	}
	l.stored([]byte("C|||:ahead"), Credit{Item: 2}, 10, 4)
	l.found([]byte("C|||:ahead"), Credit{Item: 1})
	if st := l.Stats(); st.Locked != 2 || st.Held != 1 {
		t.Fatalf("after a store ahead and a find: %+v, want 2 locks and 1 held", st)
	}
	l.Settle(2)
	if st := l.Stats(); st.Held != 0 {
		t.Fatalf("after settling: %+v, want nothing held", st)
	}
}

// TestChunkCreditsStoredAgainIsTheNewOwners: a chunk stored again was not
// kept by the pass that stored it first, so it is the new pass's to give.
func TestChunkCreditsStoredAgainIsTheNewOwners(t *testing.T) {
	l := NewChunkCredits()
	key := []byte("C|||:again")
	l.stored(key, Credit{Item: 3}, 10, 4)
	l.stored(key, Credit{Item: 2}, 10, 5)
	l.found(key, Credit{Item: 1})
	adjusted(t, l, map[int][2]int64{1: {10, 5}, 2: {-10, -5}, 3: {0, 0}})
}

// TestChunkCreditsHoldOnlyWhatAPassBeforeMayNeed: the ledger does not hold a
// chunk no pass before its owner can still need, and lets go of the rest as
// items settle, and of an object's chunks once its evidence's pass ended.
func TestChunkCreditsHoldOnlyWhatAPassBeforeMayNeed(t *testing.T) {
	l := NewChunkCredits()
	held := func(want int64) {
		t.Helper()
		if got := l.held.Load(); got != want {
			t.Fatalf("holds %d chunks, want %d", got, want)
		}
	}
	l.stored([]byte("a"), Credit{Item: 0}, 1, 1)
	held(0) // nothing comes before item 0's evidence
	l.stored([]byte("b"), Credit{Item: 0, Object: true}, 1, 1)
	held(1) // item 0's evidence pass may still need it
	l.stored([]byte("c"), Credit{Item: 1}, 1, 1)
	held(2)
	l.evidenceEnded(0)
	held(1)
	l.stored([]byte("d"), Credit{Item: 0, Object: true}, 1, 1)
	held(1)
	l.found([]byte("c"), Credit{Item: 0, Object: true})
	held(0) // item 0's object took it, and nothing comes before that now
	adjusted(t, l, map[int][2]int64{0: {0, 0}, 1: {-1, -1}})
	l.stored([]byte("e"), Credit{Item: 2}, 1, 1)
	l.Settle(0)
	held(1)
	l.Settle(1)
	held(1) // item 2's, until item 2 settles
	adjusted(t, l, map[int][2]int64{1: {0, 0}})
	l.stored([]byte("f"), Credit{Item: 2}, 1, 1)
	held(1) // no pass before item 2 runs now
	l.Settle(2)
	held(0)
}

// TestChunkCreditsFull: a ledger is full at creditsHeld chunks, and no longer
// once Settle lets go of them. No ledger is never full.
func TestChunkCreditsFull(t *testing.T) {
	l := NewChunkCredits()
	for i := range creditsHeld {
		if l.Full() {
			t.Fatalf("full at %d chunks", i)
		}
		l.stored([]byte(strconv.Itoa(i)), Credit{Item: 1}, 1, 1)
	}
	if !l.Full() {
		t.Fatalf("not full at %d chunks", creditsHeld)
	}
	l.Settle(1)
	if l.Full() {
		t.Fatal("full after its chunks settled")
	}
	var none *ChunkCredits
	if none.Full() {
		t.Fatal("no ledger is full")
	}
}

// TestPassesOfARunCountSharedChunksInOrder: when the later of two evidence
// that share chunks stores them first, the earlier, finding them stored,
// takes them over, and each counts what it would one at a time.
func TestPassesOfARunCountSharedChunksInOrder(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
		t.Fatal(err)
	}
	cs := int(dbmeta.ChunkSize(db.DB))
	shared := testutil.RandomBytes(1, 4*cs)
	first := append(append([]byte(nil), shared...), testutil.RandomBytes(2, 2*cs)...)
	second := append(append([]byte(nil), shared...), testutil.RandomBytes(3, 3*cs)...)
	l := NewChunkCredits()
	pass := func(item int, data []byte) ChunkResult {
		t.Helper()
		res, err := IngestChunks(t.Context(), ChunkPass{DB: db.DB, ID: []byte("evidence-" + strconv.Itoa(item)), Source: byteReaderAt(data),
			Size: int64(len(data)), Quiet: true, Credit: Credit{Ledger: l, Item: item}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	later := pass(1, second)
	earlier := pass(0, first)
	n0, w0 := l.Adjust(0)
	n1, w1 := l.Adjust(1)
	if got := earlier.NewBytes + n0; got != int64(len(first)) {
		t.Errorf("the earlier evidence counts %d new bytes, want all %d", got, len(first))
	}
	if got := later.NewBytes + n1; got != int64(3*cs) {
		t.Errorf("the later evidence counts %d new bytes, want its own %d", got, 3*cs)
	}
	if w0 <= 0 || w1 != -w0 {
		t.Errorf("the written bytes moved %d to the earlier and %d to the later; want what one took from the other", w0, w1)
	}
}

// TestObjectChunksAreHeldUntilTheirEvidencePassEnds: what the pass of an
// object stores is held while its item's evidence pass may still need it,
// and let go once that pass ended.
func TestObjectChunksAreHeldUntilTheirEvidencePassEnds(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
		t.Fatal(err)
	}
	cs := int(dbmeta.ChunkSize(db.DB))
	l := NewChunkCredits()
	pass := func(c Credit, id string, seed uint64) {
		t.Helper()
		data := testutil.RandomBytes(seed, 2*cs)
		if _, err := IngestChunks(t.Context(), ChunkPass{DB: db.DB, ID: []byte(id), Source: byteReaderAt(data), Size: int64(len(data)), Quiet: true, Credit: c}); err != nil {
			t.Fatal(err)
		}
	}
	pass(Credit{Ledger: l, Object: true}, "object", 21)
	if got := l.held.Load(); got != 2 {
		t.Fatalf("the object's pass left %d chunks held, want its 2: its evidence's pass may still need them", got)
	}
	pass(Credit{Ledger: l}, "evidence", 22)
	if got := l.held.Load(); got != 0 {
		t.Fatalf("%d chunks held after the evidence's pass ended, want none", got)
	}
}
