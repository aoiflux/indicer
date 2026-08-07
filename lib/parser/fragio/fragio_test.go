package fragio

import (
	"bytes"
	"io"
	"testing"

	"indicer/lib/parser/core"
)

// buildImage lays out a 4 KiB backing image with known content at scattered
// offsets, and returns the reader plus the fragments describing a logical file
// whose content is "ABCDEFGHIJ" split across three non-contiguous runs.
func buildImage() (io.ReaderAt, []core.AbsRange, string) {
	img := make([]byte, 4096)
	copy(img[1000:], "ABCD") // run 1: logical 0..3
	copy(img[2000:], "EFG")  // run 2: logical 4..6
	copy(img[3000:], "HIJ")  // run 3: logical 7..9
	frags := []core.AbsRange{
		{Start: 1000, End: 1003},
		{Start: 2000, End: 2002},
		{Start: 3000, End: 3002},
	}
	return bytes.NewReader(img), frags, "ABCDEFGHIJ"
}

func TestReadAll(t *testing.T) {
	src, frags, want := buildImage()
	r := New(src, frags)

	if r.Size() != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", r.Size(), len(want))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != want {
		t.Fatalf("ReadAll = %q, want %q", got, want)
	}
}

func TestReadAtSpanningFragments(t *testing.T) {
	src, frags, want := buildImage()
	r := New(src, frags)

	// Read across all three fragments in one call.
	buf := make([]byte, len(want))
	n, err := r.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(want) || string(buf) != want {
		t.Fatalf("ReadAt = %q (n=%d), want %q", buf[:n], n, want)
	}

	// Read a slice straddling the run-1/run-2 boundary (logical 2..5 → "CDEF").
	straddle := make([]byte, 4)
	if _, err := r.ReadAt(straddle, 2); err != nil {
		t.Fatalf("ReadAt(straddle): %v", err)
	}
	if string(straddle) != "CDEF" {
		t.Fatalf("straddle = %q, want CDEF", straddle)
	}

	// Reading at/after the end returns io.EOF.
	if _, err := r.ReadAt(make([]byte, 1), int64(len(want))); err != io.EOF {
		t.Fatalf("ReadAt past end err = %v, want EOF", err)
	}
}

func TestSparseFragment(t *testing.T) {
	img := make([]byte, 2048)
	copy(img[500:], "XY")
	src := bytes.NewReader(img)
	frags := []core.AbsRange{
		{Start: 500, End: 501},           // "XY"
		{Start: 0, End: 3, Sparse: true}, // 4 zero bytes (hole)
		{Start: 600, End: 600},           // one byte
	}
	img[600] = 'Z'

	r := New(src, frags)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := []byte{'X', 'Y', 0, 0, 0, 0, 'Z'}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadAll = %v, want %v", got, want)
	}
}
