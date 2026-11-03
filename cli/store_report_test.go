package cli

import (
	"testing"

	"indicer/lib/ingest"
)

// TestStorageLine: what a store took, in the units and ratios the docs
// define (CLI_UX.md), including content that was all stored before and
// content that grew on disk.
func TestStorageLine(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		b    ingest.Bytes
		want string
	}{
		{ingest.Bytes{Read: 100 * mib, New: 50 * mib, Written: 10 * mib},
			"100 MiB read, 50.0 MiB new, 10.0 MiB written: dedup 2.00x, compression 5.00x, 90.0% saved"},
		{ingest.Bytes{Read: 8 * mib},
			"8.00 MiB read, 0 B new, 0 B written: all deduplicated, 100.0% saved"},
		{ingest.Bytes{Read: 100 * mib, New: 100 * mib, Written: 102 * mib},
			"100 MiB read, 100 MiB new, 102 MiB written: dedup 1.00x, compression 0.98x, grew 2.0%"},
		{ingest.Bytes{}, ""},
	}
	for _, c := range cases {
		if got := storageLine(c.b); got != c.want {
			t.Errorf("storageLine(%+v) = %q, want %q", c.b, got, c.want)
		}
	}
}
