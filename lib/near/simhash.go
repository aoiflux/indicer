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
	// The windows that start in the carry need its bytes and the first of
	// data; the rest are read from data itself, so data is never copied
	// (SRC-4: a whole file used to be copied onto the heap).
	head := append(append([]byte(nil), a.carry...), data[:min(len(data), fileSimHashWindow-1)]...)
	for i := 0; i < len(a.carry) && i+fileSimHashWindow <= len(head); i++ {
		a.add(head[i : i+fileSimHashWindow])
	}
	for i := 0; i+fileSimHashWindow <= len(data); i++ {
		a.add(data[i : i+fileSimHashWindow])
	}
	// Keep the last (window-1) bytes so windows straddling the next call are not lost.
	tail := head
	if len(data) >= fileSimHashWindow-1 {
		tail = data
	}
	a.carry = append(a.carry[:0], tail[max(0, len(tail)-(fileSimHashWindow-1)):]...)
}

func (a *fileSimHashAccumulator) add(window []byte) {
	a.hasher.Reset()
	_, _ = a.hasher.Write(window)
	h := a.hasher.Sum64()
	for bit := 0; bit < 64; bit++ {
		if h&(uint64(1)<<bit) != 0 {
			a.weights[bit]++
		} else {
			a.weights[bit]--
		}
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
