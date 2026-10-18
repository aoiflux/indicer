package dbio

import (
	"errors"
	"math"
	"testing"
)

func TestIntersect(t *testing.T) {
	const cs = 4096
	tests := []struct {
		name                           string
		chunkOff, chunkLen, start, end int64
		lo, hi                         int64
		wantErr                        bool
	}{
		{name: "chunk inside range", chunkOff: cs, chunkLen: cs, start: 0, end: 3 * cs, lo: 0, hi: cs},
		{name: "range starts inside chunk", chunkOff: 0, chunkLen: cs, start: 100, end: 2 * cs, lo: 100, hi: cs},
		{name: "range ends inside chunk", chunkOff: cs, chunkLen: cs, start: 0, end: cs + 123, lo: 0, hi: 123},
		{name: "range inside chunk", chunkOff: 0, chunkLen: cs, start: 50, end: 250, lo: 50, hi: 250},
		// RST-1: the second chunk of a small range across a boundary holds
		// only what is left of the range, not the range's whole size.
		{name: "straddle first chunk", chunkOff: 0, chunkLen: cs, start: cs - 100, end: cs + 100, lo: cs - 100, hi: cs},
		{name: "straddle second chunk", chunkOff: cs, chunkLen: cs, start: cs - 100, end: cs + 100, lo: 0, hi: 100},
		{name: "short last chunk", chunkOff: 3 * cs, chunkLen: 17, start: 2 * cs, end: 3*cs + 17, lo: 0, hi: 17},
		{name: "range past short chunk", chunkOff: 3 * cs, chunkLen: 17, start: 3*cs + 5, end: 3*cs + 100, lo: 5, hi: 17},
		{name: "chunk before range", chunkOff: 0, chunkLen: cs, start: cs, end: 2 * cs, wantErr: true},
		{name: "chunk after range", chunkOff: 2 * cs, chunkLen: cs, start: 0, end: cs, wantErr: true},
		{name: "range past short chunk end", chunkOff: 3 * cs, chunkLen: 17, start: 3*cs + 50, end: 3*cs + 60, wantErr: true},
		{name: "empty chunk", chunkOff: 0, chunkLen: 0, start: 0, end: 10, wantErr: true},
		{name: "empty range", chunkOff: 0, chunkLen: cs, start: 10, end: 10, wantErr: true},
		{name: "inverted range", chunkOff: 0, chunkLen: cs, start: 20, end: 10, wantErr: true},
		{name: "negative start", chunkOff: 0, chunkLen: cs, start: -1, end: 10, wantErr: true},
		{name: "negative offset", chunkOff: -cs, chunkLen: cs, start: 0, end: 10, wantErr: true},
		{name: "offset overflow", chunkOff: math.MaxInt64 - 10, chunkLen: cs, start: 0, end: math.MaxInt64, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lo, hi, err := Intersect(tt.chunkOff, tt.chunkLen, tt.start, tt.end)
			if tt.wantErr {
				if !errors.Is(err, ErrChunkRange) {
					t.Fatalf("Intersect = [%d, %d), %v; want ErrChunkRange", lo, hi, err)
				}
				return
			}
			if err != nil || lo != tt.lo || hi != tt.hi {
				t.Fatalf("Intersect = [%d, %d), %v; want [%d, %d)", lo, hi, err, tt.lo, tt.hi)
			}
		})
	}
}
