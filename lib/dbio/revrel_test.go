package dbio

import (
	"encoding/base64"
	"runtime"
	"strconv"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/util"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

func openDBIOTestDB(t testing.TB) *badger.DB {
	t.Helper()

	if cnst.ENCODER == nil {
		encoder, err := zstdx.Acquire("best", runtime.GOMAXPROCS(0))
		if err != nil {
			t.Fatalf("zstdx.Acquire: %v", err)
		}
		cnst.ENCODER = encoder
	}

	prevQuick := cnst.QUICKOPT
	cnst.QUICKOPT = true
	t.Cleanup(func() {
		cnst.QUICKOPT = prevQuick
	})

	db, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("db.Close: %v", err)
		}
	})

	return db
}

func TestGetReverseRelationNodeReadsAppendPrimary(t *testing.T) {
	db := openDBIOTestDB(t)
	revKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, []byte("chunk-a"), cnst.DataSeperator, int64(2))

	batch := db.NewWriteBatch()
	if err := SetReverseRelationAppendMember([]byte("chunk-a"), int64(2), []byte("append-a"), batch); err != nil {
		batch.Cancel()
		t.Fatalf("SetReverseRelationAppendMember append-a: %v", err)
	}
	if err := SetReverseRelationAppendMember([]byte("chunk-a"), int64(2), []byte("append-b"), batch); err != nil {
		batch.Cancel()
		t.Fatalf("SetReverseRelationAppendMember append-b: %v", err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatalf("batch.Flush append primary: %v", err)
	}

	revMap, err := GetReverseRelationNode(revKey, db)
	if err != nil {
		t.Fatalf("GetReverseRelationNode append primary: %v", err)
	}
	if len(revMap) != 2 {
		t.Fatalf("expected 2 append-primary members, got %d", len(revMap))
	}
	for _, expected := range []string{"append-a", "append-b"} {
		if _, ok := revMap[expected]; !ok {
			t.Fatalf("missing %s from append-primary reverse relation", expected)
		}
	}
}

func TestGetReverseRelationNodeAppendPrimaryDedupesDuplicateMembers(t *testing.T) {
	db := openDBIOTestDB(t)
	revKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, []byte("chunk-b"), cnst.DataSeperator, int64(3))

	batch := db.NewWriteBatch()
	if err := SetReverseRelationAppendMember([]byte("chunk-b"), int64(3), []byte("shared"), batch); err != nil {
		batch.Cancel()
		t.Fatalf("SetReverseRelationAppendMember shared first: %v", err)
	}
	if err := SetReverseRelationAppendMember([]byte("chunk-b"), int64(3), []byte("shared"), batch); err != nil {
		batch.Cancel()
		t.Fatalf("SetReverseRelationAppendMember shared second: %v", err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatalf("batch.Flush append dedupe seed: %v", err)
	}

	revMap, err := GetReverseRelationNode(revKey, db)
	if err != nil {
		t.Fatalf("GetReverseRelationNode append dedupe read: %v", err)
	}
	if len(revMap) != 1 {
		t.Fatalf("expected 1 deduped member, got %d", len(revMap))
	}
	if _, ok := revMap["shared"]; !ok {
		t.Fatal("missing shared member after append dedupe")
	}
}

func TestGetReverseRelationNodeReturnsNotFoundWithoutAppendMembers(t *testing.T) {
	db := openDBIOTestDB(t)
	revKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, []byte("chunk-empty"), cnst.DataSeperator, int64(4))

	_, err := GetReverseRelationNode(revKey, db)
	if err != badger.ErrKeyNotFound {
		t.Fatalf("expected badger.ErrKeyNotFound, got %v", err)
	}
}

func TestGetReverseRelationNodeReadsLegacyAppendPrimaryKeys(t *testing.T) {
	db := openDBIOTestDB(t)
	chash := []byte("chunk-legacy")
	fhash := []byte("legacy-member")
	index := int64(7)
	revKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, index)

	encodedFHash := base64.RawURLEncoding.EncodeToString(fhash)
	shard := reverseRelationAppendShard(fhash)
	legacyKey := util.AppendToBytesSlice(
		cnst.ReverseRelationAppendNamespace,
		chash,
		cnst.DataSeperator,
		index,
		cnst.DataSeperator,
		shard,
		cnst.DataSeperator,
		encodedFHash,
	)

	batch := db.NewWriteBatch()
	if err := SetBatchNode(legacyKey, fhash, batch); err != nil {
		batch.Cancel()
		t.Fatalf("SetBatchNode legacy append key: %v", err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatalf("batch.Flush legacy append key: %v", err)
	}

	revMap, err := GetReverseRelationNode(revKey, db)
	if err != nil {
		t.Fatalf("GetReverseRelationNode legacy append read: %v", err)
	}
	if len(revMap) != 1 {
		t.Fatalf("expected 1 legacy member, got %d", len(revMap))
	}
	if _, ok := revMap[string(fhash)]; !ok {
		t.Fatalf("missing legacy member %q", string(fhash))
	}
}

func TestGetReverseRelationNodeReadsSegmentMembers(t *testing.T) {
	db := openDBIOTestDB(t)
	chash := []byte("chunk-segment")
	idx := int64(11)
	revKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, idx)

	batchA := db.NewWriteBatch()
	if err := seedLegacySegmentMembers([]byte("segment-a"), []ReverseRelationAppendMember{{Chash: chash, Index: idx}}, db, batchA); err != nil {
		batchA.Cancel()
		t.Fatalf("SetReverseRelationSegmentMembers segment-a: %v", err)
	}
	if err := batchA.Flush(); err != nil {
		t.Fatalf("batchA.Flush: %v", err)
	}

	batchB := db.NewWriteBatch()
	if err := seedLegacySegmentMembers([]byte("segment-b"), []ReverseRelationAppendMember{{Chash: chash, Index: idx}}, db, batchB); err != nil {
		batchB.Cancel()
		t.Fatalf("SetReverseRelationSegmentMembers segment-b: %v", err)
	}
	if err := batchB.Flush(); err != nil {
		t.Fatalf("batchB.Flush: %v", err)
	}

	revMap, err := GetReverseRelationNode(revKey, db)
	if err != nil {
		t.Fatalf("GetReverseRelationNode segment read: %v", err)
	}
	if len(revMap) != 2 {
		t.Fatalf("expected 2 segment members, got %d", len(revMap))
	}
	for _, expected := range []string{"segment-a", "segment-b"} {
		if _, ok := revMap[expected]; !ok {
			t.Fatalf("missing %s from segment reverse relation", expected)
		}
	}
}

func TestReverseRelationLookupPartsHandlesSeparatorLikeChunkHash(t *testing.T) {
	chash := []byte("chunk-with-|||:sep")
	idx := int64(8650752)
	key := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, idx)

	gotChash, gotIdx, err := reverseRelationLookupParts(key)
	if err != nil {
		t.Fatalf("reverseRelationLookupParts: %v", err)
	}
	if string(gotChash) != string(chash) {
		t.Fatalf("unexpected chash: got %q want %q", string(gotChash), string(chash))
	}
	if gotIdx != idx {
		t.Fatalf("unexpected idx: got %d want %d", gotIdx, idx)
	}
}

func TestReverseRelationAppendMemberIndexParsesLegacyPayloadRobustly(t *testing.T) {
	chash := []byte("chunk-legacy-|||:sep")
	index := int64(12345)
	fhash := []byte("file-hash")

	legacyKey := util.AppendToBytesSlice(
		cnst.ReverseRelationAppendNamespace,
		chash,
		cnst.DataSeperator,
		index,
		cnst.DataSeperator,
		reverseRelationAppendShard(fhash),
		cnst.DataSeperator,
		"encoded",
	)

	gotIdx, ok := reverseRelationAppendMemberIndex(chash, legacyKey)
	if !ok {
		t.Fatal("expected legacy payload index parse to succeed")
	}
	if gotIdx != index {
		t.Fatalf("unexpected parsed index: got %d want %d", gotIdx, index)
	}
}

func TestForEachChunkOccurrenceReadsEveryForm(t *testing.T) {
	db := openDBIOTestDB(t)
	chash := []byte("chunk-everywhere")

	batch := db.NewWriteBatch()
	// One key per occurrence, as ingests since W3 write them.
	for _, m := range []struct {
		owner string
		index int64
	}{{"evidence-a", 0}, {"evidence-a", 8192}, {"evidence-b", 4096}} {
		if err := SetBatchNode(ReverseRelationAppendMemberKey(chash, m.index, []byte(m.owner)), []byte(m.owner), batch); err != nil {
			t.Fatal(err)
		}
	}
	// The text form of the same key.
	legacyKey := util.AppendToBytesSlice(cnst.ReverseRelationAppendNamespace, chash, cnst.DataSeperator, int64(12288),
		cnst.DataSeperator, reverseRelationAppendShard([]byte("evidence-c")), cnst.DataSeperator, "encoded")
	if err := SetBatchNode(legacyKey, []byte("evidence-c"), batch); err != nil {
		t.Fatal(err)
	}
	// A legacy per-offset map, repeating one occurrence.
	legacyMap, err := msgpack.Marshal(map[string]struct{}{"evidence-a": {}, "evidence-d": {}})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetBatchNode(util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, int64(0)), legacyMap, batch); err != nil {
		t.Fatal(err)
	}
	// Segments.
	if err := seedLegacySegmentMembers([]byte("evidence-e"), []ReverseRelationAppendMember{{Chash: chash, Index: 4}, {Chash: chash, Index: 20}}, db, batch); err != nil {
		t.Fatal(err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatal(err)
	}

	got := map[string]int{}
	truncated, err := ForEachChunkOccurrence(db, chash, 0, func(owner []byte, off int64) error {
		got[string(owner)+"@"+strconv.FormatInt(off, 10)]++
		return nil
	})
	if err != nil || truncated {
		t.Fatalf("truncated %v, %v", truncated, err)
	}
	want := []string{"evidence-a@0", "evidence-a@8192", "evidence-b@4096", "evidence-c@12288", "evidence-d@0", "evidence-e@4", "evidence-e@20"}
	if len(got) != len(want) {
		t.Fatalf("occurrences %v, want %v", got, want)
	}
	for _, w := range want {
		if got[w] != 1 {
			t.Fatalf("occurrence %s reported %d times: %v", w, got[w], got)
		}
	}

	n := 0
	truncated, err = ForEachChunkOccurrence(db, chash, 3, func([]byte, int64) error { n++; return nil })
	if err != nil || !truncated || n != 3 {
		t.Fatalf("with a limit of 3: %d reported, truncated %v, %v", n, truncated, err)
	}
	if ok, err := ChunkOccursAt(db, chash, 4096, []byte("evidence-b")); err != nil || !ok {
		t.Fatalf("ChunkOccursAt: %v, %v", ok, err)
	}
	if ok, err := ChunkOccursAt(db, chash, 4096, []byte("evidence-a")); err != nil || ok {
		t.Fatalf("ChunkOccursAt of an absent occurrence: %v, %v", ok, err)
	}
}
