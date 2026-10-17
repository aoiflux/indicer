package fsutil

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"indicer/lib/parser/core"
)

func TestOpenContent(t *testing.T) {
	part := make([]byte, 64)
	for i := range part {
		part[i] = byte('a' + i%26)
	}
	const base = 1000 // the partition starts at image offset 1000
	c, err := core.ExtentContent(12, []core.Extent{
		{FileOffset: 0, Length: 4, ImageOffset: base + 10, Kind: core.ExtentData},
		{FileOffset: 4, Length: 3, Kind: core.ExtentSparse},
		{FileOffset: 7, Length: 5, ImageOffset: base + 30, Kind: core.ExtentData},
	})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := OpenContent(bytes.NewReader(part), base, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte{}, part[10:14]...), 0, 0, 0), part[30:35]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}

	short, _ := core.ExtentContent(20, c.Extents)
	rc, _ = OpenContent(bytes.NewReader(part), base, short)
	if got, err := io.ReadAll(rc); !errors.Is(err, ErrNotLocated) || len(got) != 12 {
		t.Fatalf("truncated content: %d bytes, %v", len(got), err)
	}

	rc, _ = OpenContent(nil, 0, core.InlineContent(3, []byte("xyz")))
	if got, _ := io.ReadAll(rc); string(got) != "xyz" {
		t.Fatalf("inline %q", got)
	}
}
