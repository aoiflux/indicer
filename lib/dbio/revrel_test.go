package dbio

import (
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
