package propindex_test

import (
	"bytes"
	"math"
	"sort"
	"strconv"
	"testing"
	"time"

	"indicer/lib/graph/propindex"
)

// assertByteOrderMatchesValueOrder is the property every encoder must have:
// sorting the encodings must give the same sequence as sorting the values.
// Without it a range query answers wrongly, which is worse than not answering.
func assertByteOrderMatchesValueOrder[T any](t *testing.T, name string, values []T, encode func(T) []byte) {
	t.Helper()

	encoded := make([][]byte, len(values))
	for i, v := range values {
		encoded[i] = encode(v)
	}

	shuffled := make([][]byte, len(encoded))
	copy(shuffled, encoded)
	sort.Slice(shuffled, func(i, j int) bool { return bytes.Compare(shuffled[i], shuffled[j]) < 0 })

	for i := range encoded {
		if !bytes.Equal(encoded[i], shuffled[i]) {
			t.Errorf("%s: byte order does not match value order at position %d (value %v)",
				name, i, values[i])
			return
		}
	}
}

func TestInt64_OrderIsPreserved(t *testing.T) {
	values := []int64{
		math.MinInt64, math.MinInt32, -65536, -256, -129, -128, -33, -32, -1,
		0, 1, 32, 127, 128, 255, 256, 65535, 65536, math.MaxInt32, math.MaxInt64,
	}
	assertByteOrderMatchesValueOrder(t, "Int64", values, propindex.Int64)
}

func TestUint64_OrderIsPreserved(t *testing.T) {
	values := []uint64{0, 1, 127, 128, 255, 256, 65535, 1 << 20, 1 << 32, math.MaxUint64}
	assertByteOrderMatchesValueOrder(t, "Uint64", values, propindex.Uint64)
}

func TestFloat64_OrderIsPreserved(t *testing.T) {
	values := []float64{
		math.Inf(-1), -1e308, -1, -0.5, 0, 0.5, 1, 5.42, 7.81, 1e308, math.Inf(1),
	}
	assertByteOrderMatchesValueOrder(t, "Float64", values, propindex.Float64)
}

func TestTime_OrderIsPreserved(t *testing.T) {
	base := time.Date(2026, 8, 13, 9, 4, 11, 0, time.UTC)
	values := []time.Time{
		base.Add(-100 * 365 * 24 * time.Hour),
		base.Add(-time.Hour), base.Add(-time.Second), base,
		base.Add(time.Nanosecond), base.Add(time.Hour),
		base.Add(100 * 365 * 24 * time.Hour),
	}
	assertByteOrderMatchesValueOrder(t, "Time", values, propindex.Time)
}

// The reason Bool exists rather than reusing strconv.FormatBool.
func TestBool_IsNotStrconv(t *testing.T) {
	if bytes.Equal(propindex.Bool(true), []byte(strconv.FormatBool(true))) {
		t.Error("Bool encodes as strconv does; the point is that it does not")
	}
	if bytes.Compare(propindex.Bool(false), propindex.Bool(true)) >= 0 {
		t.Error("false must sort below true")
	}
}

// The concrete failure this package exists to prevent: decimal strings are not
// a valid byte-wise sort order, so a range query over them is wrong.
func TestDecimalStrings_AreNotAValidSortOrder(t *testing.T) {
	if bytes.Compare([]byte(strconv.Itoa(10)), []byte(strconv.Itoa(9))) >= 0 {
		t.Fatal("expected \"10\" to sort below \"9\" byte-wise; the premise of this " +
			"package no longer holds")
	}
	if bytes.Compare(propindex.Int64(10), propindex.Int64(9)) <= 0 {
		t.Error("Int64(10) must sort above Int64(9)")
	}
}

func TestDecoders_RoundTrip(t *testing.T) {
	if got, ok := propindex.DecodeInt64(propindex.Int64(-4711)); !ok || got != -4711 {
		t.Errorf("DecodeInt64 = %d, %v; want -4711, true", got, ok)
	}
	if got, ok := propindex.DecodeUint64(propindex.Uint64(1 << 40)); !ok || got != 1<<40 {
		t.Errorf("DecodeUint64 = %d, %v", got, ok)
	}
	if got, ok := propindex.DecodeFloat64(propindex.Float64(7.81)); !ok || got != 7.81 {
		t.Errorf("DecodeFloat64 = %v, %v", got, ok)
	}
	want := time.Date(2026, 8, 13, 9, 4, 11, 0, time.UTC)
	if got, ok := propindex.DecodeTime(propindex.Time(want)); !ok || !got.Equal(want) {
		t.Errorf("DecodeTime = %v, %v", got, ok)
	}
}

func TestPrefixUpperBound_BoundsTheRange(t *testing.T) {
	upper, ok := propindex.PrefixUpperBound([]byte("/Users/jane/"))
	if !ok {
		t.Fatal("PrefixUpperBound reported no bound for an ordinary prefix")
	}
	if bytes.Compare([]byte("/Users/jane/"), upper) >= 0 {
		t.Error("upper bound does not sit above the prefix")
	}
	if bytes.Compare([]byte("/Users/jane/Documents"), upper) >= 0 {
		t.Error("a value under the prefix falls outside the bound")
	}
	if bytes.Compare([]byte("/Users/janet"), upper) < 0 {
		t.Error("a value outside the prefix falls inside the bound")
	}
}

func TestDeclare_RequiresARegistry(t *testing.T) {
	if err := propindex.Declare(nil, nil); err == nil {
		t.Error("Declare accepted a nil registry")
	}
}

// The two sets must not overlap: a key cannot be both safe to declare and
// waiting on the rebuild, and getting that wrong would silently reinterpret
// stored values.
func TestKeySets_DoNotOverlap(t *testing.T) {
	for _, declarable := range propindex.DeclarableNodeKeys {
		if propindex.Deferred(declarable) {
			t.Errorf("%q is listed as both declarable and deferred", declarable)
		}
	}
}

// The deferred set is exactly the keys DUES still writes with strconv. If a
// key leaves that set, the write path must have been changed to an
// order-preserving encoder first.
func TestDeferredKeys_AreTheStrconvOnes(t *testing.T) {
	want := map[string]bool{
		"start": true, "end": true,
		"is_deleted": true, "is_fragmented": true, "deterministic_flag": true,
	}
	for _, key := range propindex.DeferredKeys {
		if !want[key] {
			t.Errorf("%q is deferred but is not one of the strconv-encoded keys", key)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("%q is written with strconv but is not deferred", key)
	}
}

// Timestamps must be declarable — the whole point of landing them was the
// timeline window.
func TestDeclarableKeys_IncludeTheTimestamps(t *testing.T) {
	declarable := map[string]bool{}
	for _, key := range propindex.DeclarableNodeKeys {
		declarable[key] = true
	}
	for _, key := range []string{"mtime", "atime", "ctime", "btime"} {
		if !declarable[key] {
			t.Errorf("%q is not declarable; a timeline query would fall back to a scan", key)
		}
	}
}
