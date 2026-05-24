package store

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
)

func diffReuseStats(after, before LogicalHochoReuseStats) LogicalHochoReuseStats {
	return LogicalHochoReuseStats{
		Attempts:                after.Attempts - before.Attempts,
		Reused:                  after.Reused - before.Reused,
		FallbackUnaligned:       after.FallbackUnaligned - before.FallbackUnaligned,
		FallbackMissingRelation: after.FallbackMissingRelation - before.FallbackMissingRelation,
		Errors:                  after.Errors - before.Errors,
	}
}

func ensureZstdReadyForStoreTests(t *testing.T) {
	t.Helper()
	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			t.Fatalf("init zstd decoder: %v", err)
		}
		cnst.DECODER = decoder
	}
	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedDefault)))
		if err != nil {
			t.Fatalf("init zstd encoder: %v", err)
		}
		cnst.ENCODER = encoder
	}
}

func TestTryComputeAlignedLogicalHochoReuseSuccess(t *testing.T) {
	ensureZstdReadyForStoreTests(t)

	oldChunkSize := cnst.ChonkSize
	cnst.ChonkSize = 8
	t.Cleanup(func() {
		cnst.ChonkSize = oldChunkSize
	})

	dbPath := filepath.Join(t.TempDir(), "db")
	key := []byte("01234567890123456789012345678901")
	db, err := dbio.ConnectDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer db.Close()

	fileID := []byte("evidence-id-123456")
	chunkHashes := [][]byte{
		[]byte("chunk-hash-000000000000000000000001"),
		[]byte("chunk-hash-000000000000000000000002"),
	}

	err = db.Update(func(txn *badger.Txn) error {
		for i, ch := range chunkHashes {
			idx := int64(i) * cnst.ChonkSize
			relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, idx)
			if setErr := txn.Set(relKey, ch); setErr != nil {
				return setErr
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set relation keys: %v", err)
	}

	before := SnapshotLogicalHochoReuseStats()
	got, reused, err := TryComputeAlignedLogicalHocho(fileID, 0, 2*cnst.ChonkSize, db)
	if err != nil {
		t.Fatalf("TryComputeAlignedLogicalHocho: %v", err)
	}
	if !reused {
		t.Fatal("expected reuse to succeed for fully aligned range")
	}

	h := cnst.GetHashAlgo(true)
	for _, ch := range chunkHashes {
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(ch)))
		if _, err := h.Write(lp[:]); err != nil {
			t.Fatalf("write length prefix: %v", err)
		}
		if _, err := h.Write(ch); err != nil {
			t.Fatalf("write chunk hash: %v", err)
		}
	}
	want := h.Sum(nil)

	if !bytes.Equal(got, want) {
		t.Fatal("unexpected hocho reuse hash output")
	}

	after := SnapshotLogicalHochoReuseStats()
	delta := diffReuseStats(after, before)
	if delta.Attempts != 1 || delta.Reused != 1 || delta.FallbackUnaligned != 0 || delta.FallbackMissingRelation != 0 || delta.Errors != 0 {
		t.Fatalf("unexpected stats delta for reuse success: %+v", delta)
	}
}

func TestTryComputeAlignedLogicalHochoFallbackOnMissingRelation(t *testing.T) {
	ensureZstdReadyForStoreTests(t)

	oldChunkSize := cnst.ChonkSize
	cnst.ChonkSize = 8
	t.Cleanup(func() {
		cnst.ChonkSize = oldChunkSize
	})

	dbPath := filepath.Join(t.TempDir(), "db")
	key := []byte("01234567890123456789012345678901")
	db, err := dbio.ConnectDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer db.Close()

	fileID := []byte("evidence-id-123456")

	before := SnapshotLogicalHochoReuseStats()
	got, reused, err := TryComputeAlignedLogicalHocho(fileID, 0, cnst.ChonkSize, db)
	if err != nil {
		t.Fatalf("TryComputeAlignedLogicalHocho: %v", err)
	}
	if reused {
		t.Fatal("expected fallback when relation entries are missing")
	}
	if got != nil {
		t.Fatal("expected nil hash when reuse is unavailable")
	}

	after := SnapshotLogicalHochoReuseStats()
	delta := diffReuseStats(after, before)
	if delta.Attempts != 1 || delta.Reused != 0 || delta.FallbackUnaligned != 0 || delta.FallbackMissingRelation != 1 || delta.Errors != 0 {
		t.Fatalf("unexpected stats delta for missing relation fallback: %+v", delta)
	}
}

func TestTryComputeAlignedLogicalHochoFallbackOnUnalignedRange(t *testing.T) {
	ensureZstdReadyForStoreTests(t)

	oldChunkSize := cnst.ChonkSize
	cnst.ChonkSize = 8
	t.Cleanup(func() {
		cnst.ChonkSize = oldChunkSize
	})

	dbPath := filepath.Join(t.TempDir(), "db")
	key := []byte("01234567890123456789012345678901")
	db, err := dbio.ConnectDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer db.Close()

	fileID := []byte("evidence-id-123456")

	before := SnapshotLogicalHochoReuseStats()
	got, reused, err := TryComputeAlignedLogicalHocho(fileID, 1, cnst.ChonkSize, db)
	if err != nil {
		t.Fatalf("TryComputeAlignedLogicalHocho: %v", err)
	}
	if reused {
		t.Fatal("expected fallback for unaligned range")
	}
	if got != nil {
		t.Fatal("expected nil hash for unaligned range")
	}

	after := SnapshotLogicalHochoReuseStats()
	delta := diffReuseStats(after, before)
	if delta.Attempts != 1 || delta.Reused != 0 || delta.FallbackUnaligned != 1 || delta.FallbackMissingRelation != 0 || delta.Errors != 0 {
		t.Fatalf("unexpected stats delta for unaligned fallback: %+v", delta)
	}
}

func TestTryComputeLogicalHochoWithEdgesReuseSuccess(t *testing.T) {
	ensureZstdReadyForStoreTests(t)

	oldChunkSize := cnst.ChonkSize
	cnst.ChonkSize = 8
	t.Cleanup(func() {
		cnst.ChonkSize = oldChunkSize
	})

	dbPath := filepath.Join(t.TempDir(), "db")
	key := []byte("01234567890123456789012345678901")
	db, err := dbio.ConnectDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer db.Close()

	fileID := []byte("evidence-id-123456")
	mapped := []byte("abcdefghijklmnopqrstuvwxyz")
	interiorChunkHash := []byte("chunk-hash-interior-0000000000001")

	err = db.Update(func(txn *badger.Txn) error {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, int64(8))
		return txn.Set(relKey, interiorChunkHash)
	})
	if err != nil {
		t.Fatalf("set relation key: %v", err)
	}

	start := int64(3)
	size := int64(14) // [3,17): partial chunk0, full chunk1, partial chunk2

	before := SnapshotLogicalHochoReuseStats()
	got, reused, err := TryComputeLogicalHochoWithEdges(fileID, start, size, mapped, db)
	if err != nil {
		t.Fatalf("TryComputeLogicalHochoWithEdges: %v", err)
	}
	if !reused {
		t.Fatal("expected edge-aware reuse to succeed")
	}

	leadingHash, err := util.GetChonkHash(mapped[3:8], cnst.GetHashAlgo(true))
	if err != nil {
		t.Fatalf("leading edge hash: %v", err)
	}
	trailingHash, err := util.GetChonkHash(mapped[16:17], cnst.GetHashAlgo(true))
	if err != nil {
		t.Fatalf("trailing edge hash: %v", err)
	}

	h := cnst.GetHashAlgo(true)
	for _, ch := range [][]byte{leadingHash, interiorChunkHash, trailingHash} {
		var lp [4]byte
		binary.BigEndian.PutUint32(lp[:], uint32(len(ch)))
		if _, err := h.Write(lp[:]); err != nil {
			t.Fatalf("write length prefix: %v", err)
		}
		if _, err := h.Write(ch); err != nil {
			t.Fatalf("write chunk hash: %v", err)
		}
	}
	want := h.Sum(nil)

	if !bytes.Equal(got, want) {
		t.Fatal("unexpected edge-aware hocho reuse hash output")
	}

	after := SnapshotLogicalHochoReuseStats()
	delta := diffReuseStats(after, before)
	if delta.Attempts != 1 || delta.Reused != 1 || delta.FallbackUnaligned != 0 || delta.FallbackMissingRelation != 0 || delta.Errors != 0 {
		t.Fatalf("unexpected stats delta for edge-aware reuse success: %+v", delta)
	}
}

func TestTryComputeLogicalHochoWithEdgesFallbackOnMissingInteriorRelation(t *testing.T) {
	ensureZstdReadyForStoreTests(t)

	oldChunkSize := cnst.ChonkSize
	cnst.ChonkSize = 8
	t.Cleanup(func() {
		cnst.ChonkSize = oldChunkSize
	})

	dbPath := filepath.Join(t.TempDir(), "db")
	key := []byte("01234567890123456789012345678901")
	db, err := dbio.ConnectDB(dbPath, key)
	if err != nil {
		t.Fatalf("ConnectDB: %v", err)
	}
	defer db.Close()

	fileID := []byte("evidence-id-123456")
	mapped := []byte("abcdefghijklmnopqrstuvwxyz")

	before := SnapshotLogicalHochoReuseStats()
	got, reused, err := TryComputeLogicalHochoWithEdges(fileID, 3, 14, mapped, db)
	if err != nil {
		t.Fatalf("TryComputeLogicalHochoWithEdges: %v", err)
	}
	if reused {
		t.Fatal("expected fallback when interior relation hash is missing")
	}
	if got != nil {
		t.Fatal("expected nil hash on fallback")
	}

	after := SnapshotLogicalHochoReuseStats()
	delta := diffReuseStats(after, before)
	if delta.Attempts != 1 || delta.Reused != 0 || delta.FallbackUnaligned != 0 || delta.FallbackMissingRelation != 1 || delta.Errors != 0 {
		t.Fatalf("unexpected stats delta for edge-aware missing relation fallback: %+v", delta)
	}
}
