package dbio

import (
	"encoding/base64"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
)

func openDBIOTestDB(t testing.TB) *badger.DB {
	t.Helper()

	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			t.Fatalf("zstd.NewReader: %v", err)
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
		if err != nil {
			t.Fatalf("zstd.NewWriter: %v", err)
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
	if err := SetReverseRelationSegmentMembers([]byte("segment-a"), []ReverseRelationAppendMember{{Chash: chash, Index: idx}}, db, batchA); err != nil {
		batchA.Cancel()
		t.Fatalf("SetReverseRelationSegmentMembers segment-a: %v", err)
	}
	if err := batchA.Flush(); err != nil {
		t.Fatalf("batchA.Flush: %v", err)
	}

	batchB := db.NewWriteBatch()
	if err := SetReverseRelationSegmentMembers([]byte("segment-b"), []ReverseRelationAppendMember{{Chash: chash, Index: idx}}, db, batchB); err != nil {
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

func TestGetReverseRelationAppendPrefixMembersIncludesSegments(t *testing.T) {
	db := openDBIOTestDB(t)
	chash := []byte("chunk-segment-prefix")
	seedMembers := []ReverseRelationAppendMember{
		{Chash: chash, Index: 4},
		{Chash: chash, Index: 20},
	}

	batch := db.NewWriteBatch()
	if err := SetReverseRelationSegmentMembers([]byte("segment-prefix"), seedMembers, db, batch); err != nil {
		batch.Cancel()
		t.Fatalf("SetReverseRelationSegmentMembers seed: %v", err)
	}
	if err := batch.Flush(); err != nil {
		t.Fatalf("batch.Flush seed: %v", err)
	}

	revPrefixKey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, int64(0))
	prefixMap, err := GetReverseRelationAppendPrefixMembers(revPrefixKey, db)
	if err != nil {
		t.Fatalf("GetReverseRelationAppendPrefixMembers segment read: %v", err)
	}

	for _, idx := range []int64{4, 20} {
		values := prefixMap[idx]
		if len(values) != 1 || values[0] != "segment-prefix" {
			t.Fatalf("unexpected prefix values at %d: %#v", idx, values)
		}
	}
}
