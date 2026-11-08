package dbio

import (
	"bytes"
	"context"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// TestCountSharedChunksReadsEveryForm (STO-24): a chunk counts as shared when
// its reverse relations name more than one object, in whatever form they were
// kept: one key per occurrence (binary, and the text form), legacy per-offset
// maps and segments, or a mix of them. A chunk hash holding the separator's
// bytes is read by its length.
func TestCountSharedChunksReadsEveryForm(t *testing.T) {
	t.Parallel()
	db := openDBIOTestDB(t)
	const hashLen = 32
	chunk := func(name string) []byte {
		h := bytes.Repeat([]byte{0}, hashLen)
		copy(h, name)
		return h
	}
	withSep := chunk("c6")
	copy(withSep[10:], cnst.DataSeperator)

	batch := db.NewWriteBatch()
	set := func(key, value []byte) {
		t.Helper()
		if err := SetBatchNode(db, key, value, batch); err != nil {
			t.Fatal(err)
		}
	}
	member := func(chash []byte, index int64, owner string) {
		set(ReverseRelationAppendMemberKey(chash, index, []byte(owner)), []byte(owner))
	}
	textMember := func(chash []byte, index int64, owner string) {
		set(util.AppendToBytesSlice(cnst.ReverseRelationAppendNamespace, chash, cnst.DataSeperator, index,
			cnst.DataSeperator, reverseRelationAppendShard([]byte(owner)), cnst.DataSeperator, "encoded"), []byte(owner))
	}
	legacyMap := func(chash []byte, index int64, owners ...string) {
		m := map[string]struct{}{}
		for _, o := range owners {
			m[o] = struct{}{}
		}
		data, err := msgpack.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		set(util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, index), data)
	}
	segment := func(chash []byte, owner string, indexes ...int64) {
		var members []ReverseRelationAppendMember
		for _, i := range indexes {
			members = append(members, ReverseRelationAppendMember{Chash: chash, Index: i})
		}
		if err := seedLegacySegmentMembers([]byte(owner), members, db, batch); err != nil {
			t.Fatal(err)
		}
	}

	member(chunk("c1"), 0, "a") // shared: two objects
	member(chunk("c1"), 4096, "b")
	member(chunk("c2"), 0, "a") // not: one object, twice
	member(chunk("c2"), 8192, "a")
	member(chunk("c3"), 0, "a") // shared: binary and text forms
	textMember(chunk("c3"), 0, "b")
	member(chunk("c4"), 0, "d") // shared: across a map and a key
	legacyMap(chunk("c4"), 0, "c")
	legacyMap(chunk("c5"), 0, "e", "f") // shared: one map
	member(withSep, 0, "a")             // shared: the separator in the hash
	member(withSep, 0, "b")
	segment(chunk("c7"), "g", 0) // shared: segments of two objects
	segment(chunk("c7"), "h", 4096)
	segment(chunk("c8"), "g", 0, 4096) // not: one object's segment
	textMember(chunk("c9"), 0, "a")    // not: text form, one object
	legacyMap(chunk("ca"), 0, "a")     // not: one map, one object
	if err := batch.Flush(); err != nil {
		t.Fatal(err)
	}

	var n int64
	err := db.View(func(txn *badger.Txn) error {
		var err error
		n, err = CountSharedChunks(t.Context(), txn, hashLen)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("shared chunks: %d, want 6 (c1, c3, c4, c5, c6, c7)", n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = db.View(func(txn *badger.Txn) error {
		_, err := CountSharedChunks(ctx, txn, hashLen)
		return err
	})
	if err == nil {
		t.Fatal("a cancelled count succeeded")
	}
}
