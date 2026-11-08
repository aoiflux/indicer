package dbio_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"indicer/lib/dbio"
	"indicer/lib/structs"
)

func TestExtentReaderReadsDataAndHoles(t *testing.T) {
	t.Parallel()
	src := bytes.Repeat([]byte("0123456789"), 10) // 100 bytes
	ext := []structs.Extent{
		{FileOffset: 0, Length: 4, Offset: 10, Kind: structs.ExtentData},  // "0123"
		{FileOffset: 4, Length: 3, Kind: structs.ExtentSparse},            // zeros
		{FileOffset: 7, Length: 5, Offset: 95, Kind: structs.ExtentData},  // "56789"
		{FileOffset: 12, Length: 2, Kind: structs.ExtentUnwritten},        // zeros
		{FileOffset: 14, Length: 3, Offset: 41, Kind: structs.ExtentData}, // "123"
	}
	r, err := dbio.NewExtentReader(bytes.NewReader(src), ext)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("0123\x00\x00\x0056789\x00\x00123")
	if r.Size() != int64(len(want)) {
		t.Fatalf("size %d, want %d", r.Size(), len(want))
	}
	got, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read %q, %v; want %q", got, err, want)
	}
	// Every offset and length agrees with the whole.
	for off := range len(want) {
		for n := 1; off+n <= len(want)+2; n++ {
			p := make([]byte, n)
			m, err := r.ReadAt(p, int64(off))
			end := min(off+n, len(want))
			if m != end-off || !bytes.Equal(p[:m], want[off:end]) {
				t.Fatalf("ReadAt(%d, %d) = %d %q, want %q", n, off, m, p[:m], want[off:end])
			}
			if off+n > len(want) && !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt(%d, %d) past the end: err %v, want EOF", n, off, err)
			}
			if off+n <= len(want) && err != nil {
				t.Fatalf("ReadAt(%d, %d): %v", n, off, err)
			}
		}
	}
}

func TestExtentReaderShortSource(t *testing.T) {
	t.Parallel()
	r, err := dbio.NewExtentReader(bytes.NewReader(make([]byte, 10)), []structs.Extent{{FileOffset: 0, Length: 8, Offset: 6}})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReadAt(make([]byte, 8), 0); n != 4 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read past the evidence: %d, %v; want 4, unexpected EOF", n, err)
	}
}

func TestExtentReaderRejectsBadExtents(t *testing.T) {
	t.Parallel()
	for name, ext := range map[string][]structs.Extent{
		"gap":      {{FileOffset: 0, Length: 4}, {FileOffset: 5, Length: 1}},
		"overlap":  {{FileOffset: 0, Length: 4}, {FileOffset: 3, Length: 1}},
		"empty":    {{FileOffset: 0, Length: 0}},
		"kind":     {{FileOffset: 0, Length: 1, Kind: 9}},
		"negative": {{FileOffset: 0, Length: 1, Offset: -1}},
		"overflow": {{FileOffset: 0, Length: 1 << 62, Offset: 1 << 62}, {FileOffset: 1 << 62, Length: 1 << 62}},
	} {
		if _, err := dbio.NewExtentReader(bytes.NewReader(nil), ext); !errors.Is(err, dbio.ErrBadExtents) {
			t.Errorf("%s: err %v, want ErrBadExtents", name, err)
		}
	}
	r, err := dbio.NewExtentReader(bytes.NewReader(nil), nil)
	if err != nil || r.Size() != 0 {
		t.Fatalf("no extents: %v, size %d", err, r.Size())
	}
}
