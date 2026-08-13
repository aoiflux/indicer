package props_test

import (
	"math"
	"testing"
	"time"

	"indicer/lib/graph/props"
)

// roundTrip packs a single-key map and decodes it, so the test sees the same
// Go type msgpack would hand a real caller rather than the type that was
// written. That distinction is the whole point: msgpack picks its integer
// width from the value, so an int64 field comes back as int8 when small.
func roundTrip(t *testing.T, value any) props.Map {
	t.Helper()
	packed, err := props.Pack(map[string]any{"k": value})
	if err != nil {
		t.Fatal(err)
	}
	return props.Unpack(packed)
}

// TestInt64_EveryMsgpackWidth is the regression for the divergence between the
// three former helper copies. The enrichment copy switched on int64 and
// float64 only, so every value msgpack chose to encode narrower — which is
// most of them — read back as 0.
func TestInt64_EveryMsgpackWidth(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int64
	}{
		{"zero", int64(0), 0},
		{"positive fixint", int64(1), 1},
		{"positive fixint max", int64(127), 127},
		{"uint8 boundary", int64(128), 128},
		{"uint8 max", int64(255), 255},
		{"uint16 boundary", int64(256), 256},
		{"uint16 max", int64(65535), 65535},
		{"uint32 boundary", int64(65536), 65536},
		{"uint32 max", int64(4294967295), 4294967295},
		{"uint64 boundary", int64(4294967296), 4294967296},
		{"int64 max", int64(math.MaxInt64), math.MaxInt64},
		{"negative fixint", int64(-1), -1},
		{"negative fixint min", int64(-32), -32},
		{"int8 boundary", int64(-33), -33},
		{"int8 min", int64(-128), -128},
		{"int16 boundary", int64(-129), -129},
		{"int16 min", int64(-32768), -32768},
		{"int32 min", int64(math.MinInt32), math.MinInt32},
		{"int64 min", int64(math.MinInt64), math.MinInt64},

		{"declared uint64", uint64(9000000000), 9000000000},
		{"declared int", int(4096), 4096},
		{"declared int32", int32(70000), 70000},
		{"declared float64", float64(1234), 1234},
		{"declared float32", float32(2048), 2048},
		{"decimal string", "8675309", 8675309},

		{"realistic size", int64(8 * 1024 * 1024 * 1024), 8589934592},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roundTrip(t, tc.value).Int64("k"); got != tc.want {
				t.Errorf("Int64 = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInt64_UnconvertibleYieldsZero(t *testing.T) {
	for _, value := range []any{"not a number", true, []any{1, 2}, nil} {
		if got := roundTrip(t, value).Int64("k"); got != 0 {
			t.Errorf("Int64 of %#v = %d, want 0", value, got)
		}
	}
	if got := (props.Map{}).Int64("absent"); got != 0 {
		t.Errorf("Int64 of a missing key = %d, want 0", got)
	}
}

func TestFloat_CoercesAcrossWidths(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  float64
	}{
		{"float64", float64(5.42), 5.42},
		{"float32", float32(0.5), 0.5},
		{"int", int64(7), 7},
		{"small int", int64(1), 1},
		{"uint64", uint64(42), 42},
		{"numeric string", "7.81", 7.81},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roundTrip(t, tc.value).Float64("k"); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("Float64 = %v, want %v", got, tc.want)
			}
		})
	}
}

// Confidence is declared float32 throughout the model, so this is the path
// that actually runs on artefact and relation blobs.
func TestFloat32_RoundTripsConfidence(t *testing.T) {
	for _, want := range []float32{0, 0.55, 0.88, 0.92, 1} {
		if got := roundTrip(t, want).Float32("k"); got != want {
			t.Errorf("Float32 = %v, want %v", got, want)
		}
	}
}

// The enrichment copy rejected the string form. Index values are written with
// strconv.FormatBool, so the string form genuinely occurs.
func TestBool_AcceptsStringAndNumericForms(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{"true", true, true},
		{"false", false, false},
		{`string "true"`, "true", true},
		{`string "false"`, "false", false},
		{`string "1"`, "1", true},
		{`string "0"`, "0", false},
		{"nonzero int", int64(1), true},
		{"zero int", int64(0), false},
		{"garbage string", "yes please", false},
		{"wrong type", []any{1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roundTrip(t, tc.value).Bool("k"); got != tc.want {
				t.Errorf("Bool = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStr_HandlesBytesAndRejectsOthers(t *testing.T) {
	if got := roundTrip(t, "History").Str("k"); got != "History" {
		t.Errorf("Str = %q, want %q", got, "History")
	}
	if got := roundTrip(t, []byte("svchost.exe")).Str("k"); got != "svchost.exe" {
		t.Errorf("Str of []byte = %q, want %q", got, "svchost.exe")
	}
	// A number must not render as a string: a silent coercion here would put
	// plausible-looking values into a report.
	if got := roundTrip(t, int64(42)).Str("k"); got != "" {
		t.Errorf("Str of an int = %q, want \"\"", got)
	}
}

func TestStrings_DecodesArraysAndPromotesScalars(t *testing.T) {
	got := roundTrip(t, []string{"acquired", "verified"}).Strings("k")
	if len(got) != 2 || got[0] != "acquired" || got[1] != "verified" {
		t.Errorf("Strings = %#v, want [acquired verified]", got)
	}
	if got := roundTrip(t, "indexed").Strings("k"); len(got) != 1 || got[0] != "indexed" {
		t.Errorf("Strings of a scalar = %#v, want [indexed]", got)
	}
	if got := roundTrip(t, "").Strings("k"); got != nil {
		t.Errorf("Strings of an empty scalar = %#v, want nil", got)
	}
	if got := (props.Map{}).Strings("absent"); got != nil {
		t.Errorf("Strings of a missing key = %#v, want nil", got)
	}
}

func TestTime_AcceptsEveryStoredForm(t *testing.T) {
	want := time.Date(2026, 8, 13, 9, 4, 11, 0, time.UTC)

	if got := roundTrip(t, want.Format(time.RFC3339Nano)).Time("k"); !got.Equal(want) {
		t.Errorf("Time from RFC3339 = %v, want %v", got, want)
	}
	if got := roundTrip(t, want.UnixNano()).Time("k"); !got.Equal(want) {
		t.Errorf("Time from unix nanos = %v, want %v", got, want)
	}
	if got := roundTrip(t, "not a time").Time("k"); !got.IsZero() {
		t.Errorf("Time from garbage = %v, want zero", got)
	}
}

// Unpack must never hand back a nil Map: call sites index the result directly.
func TestUnpack_NeverNil(t *testing.T) {
	for _, name := range []string{"nil", "empty", "garbage"} {
		var input []byte
		switch name {
		case "empty":
			input = []byte{}
		case "garbage":
			input = []byte{0xc1, 0xff, 0x00}
		}
		if got := props.Unpack(input); got == nil {
			t.Errorf("Unpack(%s) returned nil", name)
		}
	}
}

func TestHas_DistinguishesAbsentFromZero(t *testing.T) {
	m := roundTrip(t, int64(0))
	if !m.Has("k") {
		t.Error("Has: a present zero value reported absent")
	}
	if m.Has("nope") {
		t.Error("Has: a missing key reported present")
	}
}
