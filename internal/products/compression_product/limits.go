package compression_product

import (
	"errors"
	"fmt"
	"io"
	"strconv"

	coreerr "indicer/internal/core/errors"
)

const (
	// Defaults for decompress_bytes, whose output travels in the response.
	defaultBytesOutputLimit = 64 << 20
	maxBytesOutputLimit     = 256 << 20

	// Defaults for what an extraction or file decompression may write.
	defaultMaxTotalBytes = 1 << 40 // 1 TiB
	defaultMaxEntries    = 1 << 20
	defaultMaxRatio      = 10_000
	defaultMaxPathBytes  = 4096
)

// ratioGrace is how much output may be written before the ratio limit
// applies: small, highly compressible archives are normal. Tests lower it.
var ratioGrace int64 = 1 << 30

// Limits bounds what an extraction or a file decompression may write. Zero
// fields take their defaults; a caller raises a limit per request with the
// "limits" parameter.
type Limits struct {
	// MaxTotalBytes bounds the bytes written in all (1 TiB).
	MaxTotalBytes int64 `json:"max_total_bytes,omitempty"`
	// MaxEntryBytes bounds one entry (MaxTotalBytes).
	MaxEntryBytes int64 `json:"max_entry_bytes,omitempty"`
	// MaxEntries bounds the number of entries (1,048,576).
	MaxEntries int64 `json:"max_entries,omitempty"`
	// MaxRatio bounds output bytes per input byte once more than 1 GiB has
	// been written (10,000).
	MaxRatio int64 `json:"max_ratio,omitempty"`
	// MaxPathBytes bounds an entry's path (4096).
	MaxPathBytes int64 `json:"max_path_bytes,omitempty"`
}

// resolve fills in the defaults, refusing negative values.
func (l Limits) resolve() (Limits, *coreerr.Error) {
	for name, v := range map[string]int64{
		"max_total_bytes": l.MaxTotalBytes, "max_entry_bytes": l.MaxEntryBytes,
		"max_entries": l.MaxEntries, "max_ratio": l.MaxRatio, "max_path_bytes": l.MaxPathBytes,
	} {
		if v < 0 {
			return Limits{}, coreerr.NewWithDetails(coreerr.CodeInvalidRequest, "limits must not be negative", map[string]string{"limit": name})
		}
	}
	if l.MaxTotalBytes == 0 {
		l.MaxTotalBytes = defaultMaxTotalBytes
	}
	if l.MaxEntryBytes == 0 || l.MaxEntryBytes > l.MaxTotalBytes {
		l.MaxEntryBytes = l.MaxTotalBytes
	}
	if l.MaxEntries == 0 {
		l.MaxEntries = defaultMaxEntries
	}
	if l.MaxRatio == 0 {
		l.MaxRatio = defaultMaxRatio
	}
	if l.MaxPathBytes == 0 {
		l.MaxPathBytes = defaultMaxPathBytes
	}
	return l, nil
}

// LimitError is a limit an extraction or decompression would pass.
type LimitError struct {
	Limit string // the Limits field's JSON name
	Value int64
	What  string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s exceeds %s (%d)", e.What, e.Limit, e.Value)
}

func limitExceeded(err *LimitError) *coreerr.Error {
	return coreerr.NewWithDetails(coreerr.CodeLimitExceeded, err.Error(), map[string]string{
		"limit": err.Limit, "limit_value": strconv.FormatInt(err.Value, 10),
	})
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// outputGuard counts bytes written by an extraction or decompression and
// fails once they pass the total limit, or the ratio limit against the
// compressed bytes read so far.
type outputGuard struct {
	limits Limits
	in     func() int64 // compressed bytes consumed
	out    int64
	grace  int64 // output allowed before the ratio applies; ratioGrace when 0
}

func (g *outputGuard) add(n int64) error {
	g.out += n
	if g.out > g.limits.MaxTotalBytes {
		return &LimitError{Limit: "max_total_bytes", Value: g.limits.MaxTotalBytes, What: "output"}
	}
	grace := g.grace
	if grace == 0 {
		grace = ratioGrace
	}
	if g.out > grace && g.in != nil {
		if in := g.in(); in <= 0 || g.out/max(in, 1) > g.limits.MaxRatio {
			return &LimitError{Limit: "max_ratio", Value: g.limits.MaxRatio, What: "compression ratio"}
		}
	}
	return nil
}

// guardedWriter passes writes to w and counts them against g.
type guardedWriter struct {
	w io.Writer
	g *outputGuard
}

func (gw guardedWriter) Write(p []byte) (int, error) {
	if err := gw.g.add(int64(len(p))); err != nil {
		return 0, err
	}
	return gw.w.Write(p)
}

// asLimitError returns the LimitError err carries, if any.
func asLimitError(err error) (*LimitError, bool) {
	var le *LimitError
	ok := errors.As(err, &le)
	return le, ok
}
