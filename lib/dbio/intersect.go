package dbio

import (
	"errors"
	"fmt"
	"math"
)

// ErrChunkRange reports a chunk that cannot supply its part of a logical
// range: the range is malformed, the chunk lies outside it, or the chunk is
// shorter than the range needs (truncated, or read from the wrong place).
var ErrChunkRange = errors.New("chunk does not cover the requested range")

// Intersect returns the part of a chunk that falls inside the logical range
// [start, end), as bounds into the chunk: the bytes are chunk[lo:hi]. The
// chunk is chunkLen bytes long and starts at evidence offset chunkOff. A
// malformed range, or a chunk that does not overlap it, is an error, so the
// bounds are always inside the chunk.
func Intersect(chunkOff, chunkLen, start, end int64) (lo, hi int64, err error) {
	if chunkOff < 0 || chunkLen < 0 || start < 0 || end < start || chunkLen > math.MaxInt64-chunkOff {
		return 0, 0, fmt.Errorf("%w: chunk at %d of %d bytes, range [%d, %d)", ErrChunkRange, chunkOff, chunkLen, start, end)
	}
	lo = max(start, chunkOff)
	hi = min(end, chunkOff+chunkLen)
	if lo >= hi {
		return 0, 0, fmt.Errorf("%w: chunk at %d of %d bytes is outside range [%d, %d)", ErrChunkRange, chunkOff, chunkLen, start, end)
	}
	return lo - chunkOff, hi - chunkOff, nil
}
