package entropy

import (
	"math"
	"testing"
)

func TestEntropy(t *testing.T) {
	all := make([]byte, 256*4)
	for i := range all {
		all[i] = byte(i)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want float64
	}{
		{"empty", nil, 0},
		{"constant", []byte("aaaaaaaa"), 0},
		{"two values", []byte("abababab"), 1},
		{"every value", all, 8},
	} {
		if got := Of(tc.data); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}

	// Counting in pieces is counting the whole.
	var c Counter
	for i := 0; i < len(all); i += 100 {
		c.Add(all[i:min(i+100, len(all))])
	}
	if c.Value() != Of(all) || c.Total() != uint64(len(all)) {
		t.Errorf("in pieces: %v over %d bytes, whole %v", c.Value(), c.Total(), Of(all))
	}
}
