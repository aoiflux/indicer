package dbio

import (
	"bytes"
	"testing"

	"indicer/lib/cnst"
)

func TestTrimLogicalChunkData(t *testing.T) {
	chunk := make([]byte, cnst.ChonkSize)
	for i := range chunk {
		chunk[i] = byte(i % 251)
	}

	t.Run("full_chunk_no_trim", func(t *testing.T) {
		start := int64(0)
		size := int64(cnst.ChonkSize * 3)
		dbstart := int64(0)
		end := start + size
		restoreIndex := int64(cnst.ChonkSize)

		out := trimLogicalChunkData(append([]byte(nil), chunk...), restoreIndex, start, size, dbstart, end)
		if len(out) != len(chunk) {
			t.Fatalf("expected len %d, got %d", len(chunk), len(out))
		}
		if !bytes.Equal(out, chunk) {
			t.Fatalf("expected full chunk bytes unchanged")
		}
	})

	t.Run("start_boundary_trim", func(t *testing.T) {
		start := int64(100)
		size := int64(cnst.ChonkSize * 2)
		dbstart := int64(0)
		end := start + size
		restoreIndex := dbstart

		out := trimLogicalChunkData(append([]byte(nil), chunk...), restoreIndex, start, size, dbstart, end)
		expected := chunk[100:]
		if len(out) != len(expected) {
			t.Fatalf("expected len %d, got %d", len(expected), len(out))
		}
		if !bytes.Equal(out, expected) {
			t.Fatalf("unexpected start-trimmed bytes")
		}
	})

	t.Run("end_boundary_trim", func(t *testing.T) {
		start := int64(0)
		size := int64(cnst.ChonkSize + 123)
		dbstart := int64(0)
		end := start + size
		restoreIndex := int64(cnst.ChonkSize)

		out := trimLogicalChunkData(append([]byte(nil), chunk...), restoreIndex, start, size, dbstart, end)
		expected := chunk[:123]
		if len(out) != len(expected) {
			t.Fatalf("expected len %d, got %d", len(expected), len(out))
		}
		if !bytes.Equal(out, expected) {
			t.Fatalf("unexpected end-trimmed bytes")
		}
	})

	t.Run("single_chunk_start_and_end_trim", func(t *testing.T) {
		start := int64(50)
		size := int64(200)
		dbstart := int64(0)
		end := start + size
		restoreIndex := dbstart

		out := trimLogicalChunkData(append([]byte(nil), chunk...), restoreIndex, start, size, dbstart, end)
		expected := chunk[50:250]
		if len(out) != len(expected) {
			t.Fatalf("expected len %d, got %d", len(expected), len(out))
		}
		if !bytes.Equal(out, expected) {
			t.Fatalf("unexpected single-chunk trimmed bytes")
		}
	})
}
