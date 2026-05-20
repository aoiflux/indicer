package near

import (
	"hash/fnv"
	"math/bits"
)

// fileSimHashAccumulator computes a streaming SimHash over a logical file's byte content,
// correctly spanning chunk boundaries via a carry buffer so boundary 4-gram windows
// are not missed or double-counted.
//
// Phase 2 only — not involved in Phase 1 chunk-level matching.
type fileSimHashAccumulator struct {
	weights [64]int
	carry   []byte
	hasher  interface {
		Write([]byte) (int, error)
		Sum64() uint64
		Reset()
	}
}

const fileSimHashWindow = 4

func newFileSimHashAccumulator() *fileSimHashAccumulator {
	return &fileSimHashAccumulator{hasher: fnv.New64a()}
}

// write feeds the next contiguous slice of file bytes into the accumulator.
// May be called multiple times (e.g. once per chunk); correctly handles
// 4-gram windows that straddle two successive write calls via the carry buffer.
func (a *fileSimHashAccumulator) write(data []byte) {
	if len(data) == 0 {
		return
	}
	combined := append(a.carry, data...) //nolint:gocritic // intentional: carry is always small (3 bytes)
	for i := 0; i+fileSimHashWindow <= len(combined); i++ {
		a.hasher.Reset()
		_, _ = a.hasher.Write(combined[i : i+fileSimHashWindow])
		h := a.hasher.Sum64()
		for bit := 0; bit < 64; bit++ {
			if h&(uint64(1)<<bit) != 0 {
				a.weights[bit]++
			} else {
				a.weights[bit]--
			}
		}
	}
	// Keep the last (window-1) bytes so windows straddling the next call are not lost.
	carryLen := fileSimHashWindow - 1
	if len(combined) >= carryLen {
		a.carry = make([]byte, carryLen)
		copy(a.carry, combined[len(combined)-carryLen:])
	} else {
		a.carry = make([]byte, len(combined))
		copy(a.carry, combined)
	}
}

// finalize returns the 64-bit SimHash signature after all bytes have been written.
func (a *fileSimHashAccumulator) finalize() uint64 {
	var sig uint64
	for bit := 0; bit < 64; bit++ {
		if a.weights[bit] >= 0 {
			sig |= uint64(1) << bit
		}
	}
	return sig
}

// hammingSimilarity64 returns the fraction of matching bits between two 64-bit signatures.
// Duplicated here (from util) so simhash.go is self-contained within the near package.
func hammingSimilarity64(a, b uint64) float64 {
	distance := bits.OnesCount64(a ^ b)
	return float64(64-distance) / 64
}
