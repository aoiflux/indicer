package core

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func data(fileOff, length, imgOff int64) Extent {
	return Extent{FileOffset: fileOff, Length: length, ImageOffset: imgOff, Kind: ExtentData}
}

func hole(fileOff, length int64) Extent {
	return Extent{FileOffset: fileOff, Length: length, Kind: ExtentSparse}
}

func TestNormalizeExtents(t *testing.T) {
	cases := []struct {
		name string
		in   []Extent
		size int64
		want []Extent
	}{
		{
			name: "sorted by file offset",
			in:   []Extent{data(4096, 4096, 90000), data(0, 4096, 10000)},
			size: 8192,
			want: []Extent{data(0, 4096, 10000), data(4096, 4096, 90000)},
		},
		{
			name: "block slack past the size is trimmed (PAR-4)",
			in:   []Extent{data(0, 4096, 10000)},
			size: 11,
			want: []Extent{data(0, 11, 10000)},
		},
		{
			name: "extents wholly past the size are dropped",
			in:   []Extent{data(0, 4096, 10000), data(4096, 4096, 20000)},
			size: 4096,
			want: []Extent{data(0, 4096, 10000)},
		},
		{
			name: "an interior gap becomes a hole",
			in:   []Extent{data(0, 4096, 10000), data(12288, 4096, 20000)},
			size: 16384,
			want: []Extent{data(0, 4096, 10000), hole(4096, 8192), data(12288, 4096, 20000)},
		},
		{
			name: "a leading gap becomes a hole",
			in:   []Extent{data(8192, 4096, 10000)},
			size: 12288,
			want: []Extent{hole(0, 8192), data(8192, 4096, 10000)},
		},
		{
			name: "a tail past the last extent is not invented",
			in:   []Extent{data(0, 4096, 10000)},
			size: 10000,
			want: []Extent{data(0, 4096, 10000)},
		},
		{
			name: "adjacent data contiguous on the image coalesces",
			in:   []Extent{data(0, 4096, 10000), data(4096, 4096, 14096)},
			size: 8192,
			want: []Extent{data(0, 8192, 10000)},
		},
		{
			name: "adjacent data apart on the image stays split",
			in:   []Extent{data(0, 4096, 10000), data(4096, 4096, 50000)},
			size: 8192,
			want: []Extent{data(0, 4096, 10000), data(4096, 4096, 50000)},
		},
		{
			name: "adjacent holes coalesce",
			in:   []Extent{hole(0, 4096), hole(4096, 4096), data(8192, 1, 7)},
			size: 8193,
			want: []Extent{hole(0, 8192), data(8192, 1, 7)},
		},
		{
			name: "unwritten and sparse stay distinct",
			in: []Extent{
				{FileOffset: 0, Length: 4096, Kind: ExtentUnwritten},
				hole(4096, 4096),
			},
			size: 8192,
			want: []Extent{
				{FileOffset: 0, Length: 4096, Kind: ExtentUnwritten},
				hole(4096, 4096),
			},
		},
		{
			name: "zero-length extents are dropped",
			in:   []Extent{data(0, 0, 10000), data(0, 10, 10000)},
			size: 10,
			want: []Extent{data(0, 10, 10000)},
		},
		{
			name: "a zero size yields nothing",
			in:   []Extent{data(0, 4096, 10000)},
			size: 0,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeExtents(tc.in, tc.size)
			if err != nil {
				t.Fatalf("NormalizeExtents: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestNormalizeExtentsRejects(t *testing.T) {
	cases := []struct {
		name string
		in   []Extent
	}{
		{"overlap", []Extent{data(0, 4096, 10000), data(2048, 4096, 50000)}},
		{"negative file offset", []Extent{data(-1, 10, 10000)}},
		{"negative length", []Extent{data(0, -10, 10000)}},
		{"negative image offset", []Extent{data(0, 10, -1)}},
		{"file range overflows", []Extent{data(math.MaxInt64-5, 10, 0)}},
		{"image range overflows", []Extent{data(0, 10, math.MaxInt64-5)}},
		{"unknown kind", []Extent{{FileOffset: 0, Length: 10, Kind: 9}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeExtents(tc.in, 1<<40); !errors.Is(err, ErrBadExtent) {
				t.Fatalf("err = %v, want ErrBadExtent", err)
			}
		})
	}
}

func TestNormalizeExtentsKeepsInput(t *testing.T) {
	in := []Extent{data(4096, 4096, 90000), data(0, 4096, 10000)}
	orig := append([]Extent(nil), in...)
	if _, err := NormalizeExtents(in, 8192); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Fatalf("input modified: %+v", in)
	}
}

func TestExtentContentTotals(t *testing.T) {
	c, err := ExtentContent(16384, []Extent{data(0, 4096, 10000), data(12288, 2048, 20000)})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != ContentExtents {
		t.Fatalf("kind %v", c.Kind)
	}
	if c.Size != 16384 || c.Located != 14336 || c.DataBytes != 6144 || c.HoleBytes != 8192 {
		t.Fatalf("totals size=%d located=%d data=%d hole=%d", c.Size, c.Located, c.DataBytes, c.HoleBytes)
	}
	if !c.Prov.Has(ProvTruncated) {
		t.Fatalf("content that stops short of its size must be flagged truncated: %v", c.Prov)
	}
}

func TestExtentContentEmpty(t *testing.T) {
	c, err := ExtentContent(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != ContentNone || c.Prov != 0 {
		t.Fatalf("empty file: kind %v prov %v", c.Kind, c.Prov)
	}
}

func TestContentValidate(t *testing.T) {
	b := Bounds{PartStart: 1 << 20, PartEnd: 2 << 20, ImageSize: 4 << 20}
	t.Run("inside", func(t *testing.T) {
		c, _ := ExtentContent(8192, []Extent{data(0, 4096, 1<<20), data(4096, 4096, 1<<20+8192)})
		c.Validate(b)
		if c.Prov.Has(ProvOutOfBounds) || c.Located != 8192 || len(c.Extents) != 2 {
			t.Fatalf("in-bounds content changed: %+v", c)
		}
	})
	t.Run("truncated at the first extent outside the partition", func(t *testing.T) {
		c, _ := ExtentContent(12288, []Extent{
			data(0, 4096, 1<<20),
			data(4096, 4096, 2<<20-100), // runs past the partition end
			data(8192, 4096, 1<<20+8192),
		})
		c.Validate(b)
		if !c.Prov.Has(ProvOutOfBounds) || !c.Prov.Has(ProvTruncated) {
			t.Fatalf("prov %v", c.Prov)
		}
		if len(c.Extents) != 1 || c.Located != 4096 || c.DataBytes != 4096 {
			t.Fatalf("not truncated at the first bad extent: %+v", c)
		}
	})
	t.Run("before the partition", func(t *testing.T) {
		c, _ := ExtentContent(4096, []Extent{data(0, 4096, 1<<20-1)})
		c.Validate(b)
		if !c.Prov.Has(ProvOutOfBounds) || c.Located != 0 || len(c.Extents) != 0 {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("past the image", func(t *testing.T) {
		c, _ := ExtentContent(4096, []Extent{data(0, 4096, 1<<20)})
		c.Validate(Bounds{PartStart: 0, PartEnd: 8 << 20, ImageSize: 1<<20 + 100})
		if !c.Prov.Has(ProvOutOfBounds) || c.Located != 0 {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("holes need no bounds", func(t *testing.T) {
		c, _ := ExtentContent(8192, []Extent{hole(0, 4096), data(4096, 4096, 1<<20)})
		c.Validate(b)
		if c.Prov.Has(ProvOutOfBounds) || c.Located != 8192 {
			t.Fatalf("%+v", c)
		}
	})
}

func TestProvString(t *testing.T) {
	p := ProvDeleted | ProvOutOfBounds
	if got := p.String(); got != "deleted|out_of_bounds" {
		t.Fatalf("String = %q", got)
	}
	if Prov(0).String() != "" {
		t.Fatal("empty prov must render empty")
	}
}

func TestUnwrittenFrom(t *testing.T) {
	ext := []Extent{data(0, 4096, 10000), data(4096, 4096, 50000)}
	got := UnwrittenFrom(ext, 6000)
	want := []Extent{data(0, 4096, 10000), data(4096, 1904, 50000), {FileOffset: 6000, Length: 2192, Kind: ExtentUnwritten}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if got := UnwrittenFrom(ext, 0); len(got) != 1 || got[0].Kind != ExtentUnwritten || got[0].Length != 8192 {
		t.Fatalf("valid 0: %+v", got)
	}
	c, _ := ExtentContent(8192, ext)
	c.MarkUnwritten(9000)
	if c.Prov.Has(ProvBeyondValidData) {
		t.Fatal("valid past the end changed nothing and must not be flagged")
	}
	c.MarkUnwritten(4096)
	if !c.Prov.Has(ProvBeyondValidData) || c.DataBytes != 4096 || c.HoleBytes != 4096 {
		t.Fatalf("%+v", c)
	}
}

func FuzzNormalizeExtents(f *testing.F) {
	f.Add([]byte{0, 0, 16, 0, 1, 0}, int64(100))
	f.Fuzz(func(t *testing.T, raw []byte, size int64) {
		var in []Extent
		for i := 0; i+6 <= len(raw) && len(in) < 64; i += 6 {
			in = append(in, Extent{
				FileOffset:  int64(raw[i]) << raw[i+1] % 40,
				Length:      int64(raw[i+2]) << (raw[i+3] % 40),
				ImageOffset: int64(raw[i+4]) << 20,
				Kind:        ExtentKind(raw[i+5] % 4),
			})
		}
		out, err := NormalizeExtents(in, size)
		if err != nil {
			return
		}
		var pos int64
		for _, e := range out {
			if e.FileOffset != pos || e.Length <= 0 || e.FileEnd() > size {
				t.Fatalf("not a tiling of [0, %d): %+v", size, out)
			}
			pos = e.FileEnd()
		}
	})
}
