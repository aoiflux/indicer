package msgpackx

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// encodings returns values msgpack writes in every format family, from the
// smallest encoding of each kind to the 32-bit length forms.
func encodings(t testing.TB) map[string][]byte {
	t.Helper()
	values := map[string]any{
		"nil":      nil,
		"bools":    []bool{false, true},
		"fixints":  []int{0, 127, -1, -32},
		"ints":     []int64{128, 255, 256, 65535, 65536, math.MaxUint32, math.MaxUint32 + 1, -33, -128, -129, -32768, -32769, math.MinInt32, math.MinInt32 - 1},
		"uints":    []uint64{math.MaxUint64},
		"floats":   []any{float32(1.5), 2.25},
		"strs":     []string{"", strings.Repeat("a", 31), strings.Repeat("b", 32), strings.Repeat("c", 255), strings.Repeat("d", 256), strings.Repeat("e", 65536)},
		"bins":     [][]byte{{}, bytes.Repeat([]byte{1}, 255), bytes.Repeat([]byte{2}, 256), bytes.Repeat([]byte{3}, 65536)},
		"arrays":   [][]int{{}, make([]int, 15), make([]int, 16), make([]int, 65536)},
		"maps":     []map[int]int{{}, sized(15), sized(16), sized(65536)},
		"times":    []time.Time{time.Unix(1, 0), time.Unix(1, 5), time.Unix(1<<35, 5)},
		"nested":   map[string]any{"a": []any{map[string]any{"b": []any{1, "x", nil}}}},
		"struct":   struct{ A, B string }{"x", "y"},
		"as_array": asArray{A: 1, B: 2, C: "z"},
	}
	out := make(map[string][]byte, len(values))
	for name, v := range values {
		b, err := msgpack.Marshal(v)
		if err != nil {
			t.Fatal(name, err)
		}
		out[name] = b
	}
	// Extension formats msgpack does not pick for any value above.
	for name, b := range map[string][]byte{
		"fixext1":  {0xd4, 5, 1},
		"fixext2":  {0xd5, 5, 1, 2},
		"fixext16": append([]byte{0xd8, 5}, make([]byte, 16)...),
		"ext16":    append([]byte{0xc8, 0x01, 0x00, 5}, make([]byte, 256)...),
		"ext32":    append([]byte{0xc9, 0x00, 0x01, 0x00, 0x00, 5}, make([]byte, 65536)...),
		"array32":  append([]byte{0xdd, 0, 0, 0, 2}, 1, 2),
		"map32":    append([]byte{0xdf, 0, 0, 0, 1}, 1, 2),
		"str32":    append([]byte{0xdb, 0, 0, 0, 1}, 'x'),
		"bin32":    append([]byte{0xc6, 0, 0, 0, 1}, 'x'),
	} {
		out[name] = b
	}
	return out
}

type asArray struct {
	//lint:ignore U1000 msgpack reads the struct's options from this field
	_msgpack struct{} `msgpack:",as_array"`
	A, B     int
	C        string
}

func sized(n int) map[int]int {
	m := make(map[int]int, n)
	for i := range n {
		m[i] = i
	}
	return m
}

func TestCheckAcceptsWellFormedValues(t *testing.T) {
	for name, b := range encodings(t) {
		if err := Check(b); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		// Bytes after the value are ignored, as msgpack.Unmarshal ignores them.
		if err := Check(append(b[:len(b):len(b)], 0xc1, 0xdd)); err != nil {
			t.Errorf("%s with trailing bytes: %v", name, err)
		}
	}
}

func TestUnmarshalDecodesAsMsgpackDoes(t *testing.T) {
	for name, b := range encodings(t) {
		var want, got any
		werr := msgpack.Unmarshal(b, &want)
		gerr := Unmarshal(b, &got)
		if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(want, got) {
			t.Errorf("%s: Unmarshal = %v, %v; msgpack.Unmarshal = %v, %v", name, got, gerr, want, werr)
		}
	}
}

func TestCheckRefusesEveryTruncation(t *testing.T) {
	for name, b := range encodings(t) {
		step := 1
		if len(b) > 512 {
			step = len(b) / 97
		}
		for n := 0; n < len(b); n += step {
			if err := Check(b[:n]); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s cut to %d of %d bytes: %v, want ErrInvalid", name, n, len(b), err)
				break
			}
		}
	}
}

func TestCheckRefusesMalformedValues(t *testing.T) {
	for name, b := range map[string][]byte{
		"reserved code":          {0xc1},
		"reserved code in array": {0x92, 0x01, 0xc1},
		"array32 of 4 billion":   {0xdd, 0xff, 0xff, 0xff, 0xff, 0xc0, 0xc0},
		"array16 past the end":   {0xdc, 0x00, 0x03, 0xc0, 0xc0},
		"map16 needs two values": {0xde, 0x00, 0x02, 0x01, 0x02, 0x03},
		"fixmap past the end":    {0x81, 0x01},
		"str32 past the end":     {0xdb, 0x00, 0x00, 0x01, 0x00, 'x'},
		"bin8 past the end":      {0xc4, 0x02, 'x'},
		"ext8 type missing":      {0xc7, 0x00},
		"int64 cut":              {0xd3, 0, 0, 0},
		"length cut":             {0xdd, 0x00, 0x01},
		"empty":                  {},
	} {
		if err := Check(b); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func TestCheckLimitsDepth(t *testing.T) {
	nest := func(depth int) []byte {
		return append(bytes.Repeat([]byte{0x91}, depth), 0xc0)
	}
	if err := Check(nest(MaxDepth)); err != nil {
		t.Errorf("%d levels: %v", MaxDepth, err)
	}
	if err := Check(nest(MaxDepth + 1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("%d levels: %v, want ErrInvalid", MaxDepth+1, err)
	}
	// Empty containers do not count: nothing is decoded inside them.
	if err := Check(append(bytes.Repeat([]byte{0x91}, MaxDepth), 0x90)); err != nil {
		t.Errorf("%d levels ending in an empty array: %v", MaxDepth, err)
	}
}

// TestUnmarshalBoundsAllocation decodes a record whose slice header declares
// 16M elements in a few bytes. msgpack v5.4.1 alone allocates the whole slice
// (512 MiB here) and copies it before finding the data missing.
func TestUnmarshalBoundsAllocation(t *testing.T) {
	type extent struct{ A, B, C, D int64 }
	type record struct {
		Name    string
		Extents []extent
	}
	data, err := msgpack.Marshal(record{Name: "x", Extents: []extent{{}}})
	if err != nil {
		t.Fatal(err)
	}
	// The slice is the value's last field: its one element is a fixmap
	// header and four fields, preceded by a fixarray header of 1.
	at := bytes.LastIndexByte(data, 0x91)
	if at < 0 {
		t.Fatalf("no fixarray header in %x", data)
	}
	forged := append(append(data[:at:at], 0xdd, 0x01, 0x00, 0x00, 0x00), data[at+1:]...)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	var r record
	err = Unmarshal(forged, &r)
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("decoding %d bytes allocated %d bytes", len(forged), grew)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Unmarshal = %v, want ErrInvalid", err)
	}
}

// FuzzCheck decodes its input as it is, which must not panic or allocate
// without bound, and checks that Check never refuses what msgpack writes: the
// input also drives a generator of nested values, which are encoded and
// checked whole and cut by a byte.
func FuzzCheck(f *testing.F) {
	for _, b := range encodings(f) {
		if len(b) < 4096 {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var v any
		_ = Unmarshal(data, &v)

		g := generator{data: data}
		value := g.value(0)
		b, err := msgpack.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := Check(b); err != nil {
			t.Fatalf("Check refused %x (from %#v): %v", b, value, err)
		}
		if len(b) > 0 {
			if err := Check(b[:len(b)-1]); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Check accepted %x cut by one byte: %v", b, err)
			}
		}
	})
}

// generator builds a value from fuzz input, one byte per choice.
type generator struct {
	data []byte
	pos  int
}

func (g *generator) next() byte {
	if g.pos >= len(g.data) {
		return 0
	}
	b := g.data[g.pos]
	g.pos++
	return b
}

func (g *generator) value(depth int) any {
	kind := g.next()
	n := int(g.next())
	if depth >= MaxDepth-1 {
		kind %= 8
	}
	switch kind % 11 {
	case 0:
		return nil
	case 1:
		return kind&0x80 != 0
	case 2:
		return int64(n) << (kind % 7 * 8) * int64(1-2*int(kind>>7))
	case 3:
		return uint64(n) << (kind % 8 * 8)
	case 4:
		return float64(n) / 3
	case 5:
		return float32(n) / 7
	case 6:
		return strings.Repeat("s", n<<(kind%3*3))
	case 7:
		return bytes.Repeat([]byte{byte(n)}, n<<(kind%3*3))
	case 8:
		return time.Unix(int64(n)<<(kind%5*8), int64(kind))
	case 9:
		a := make([]any, n%20)
		for i := range a {
			a[i] = g.value(depth + 1)
		}
		return a
	default:
		m := make(map[string]any, n%20)
		for i := 0; i < n%20; i++ {
			m[strings.Repeat("k", i+1)] = g.value(depth + 1)
		}
		return m
	}
}
