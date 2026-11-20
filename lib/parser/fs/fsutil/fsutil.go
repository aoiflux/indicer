// Package fsutil holds small shared helpers for the filesystem adapters.
package fsutil

import (
	"time"

	"indicer/lib/parser/core"
)

// TimePtr returns a pointer to t, or nil if t is the zero time — the convention
// core.MACB uses to mean "this filesystem does not carry that timestamp".
func TimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// TrimToSize trims a list of contiguous, in-order data ranges so their total
// length does not exceed size, dropping cluster/block slack past end-of-file.
// It returns nil for a non-positive size.
func TrimToSize(ranges []core.AbsRange, size int64) []core.AbsRange {
	if size <= 0 {
		return nil
	}
	var acc int64
	for i := range ranges {
		rlen := ranges[i].End - ranges[i].Start + 1
		if acc+rlen >= size {
			ranges[i].End = ranges[i].Start + (size - acc) - 1
			return ranges[:i+1]
		}
		acc += rlen
	}
	return ranges
}
