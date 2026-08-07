package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/parser/core"
)

func TestNewRaw(t *testing.T) {
	data := []byte("the quick brown fox")
	im := NewRaw(bytes.NewReader(data), int64(len(data)))
	defer im.Close()

	if im.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", im.Size(), len(data))
	}
	if im.Format() != core.FormatRaw {
		t.Fatalf("Format = %q, want raw", im.Format())
	}

	buf := make([]byte, 5)
	n, err := im.ReadAt(buf, 4)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if got := string(buf[:n]); got != "quick" {
		t.Fatalf("ReadAt = %q, want %q", got, "quick")
	}
}

func TestOpenRawFile(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 4096)
	path := filepath.Join(t.TempDir(), "disk.dd")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	im, err := OpenRawFile(path)
	if err != nil {
		t.Fatalf("OpenRawFile: %v", err)
	}
	defer im.Close()

	if im.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", im.Size(), len(data))
	}
	buf := make([]byte, 16)
	if _, err := im.ReadAt(buf, 100); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, bytes.Repeat([]byte{0xAB}, 16)) {
		t.Fatalf("ReadAt returned unexpected bytes")
	}
}

func TestOpenRawSegments(t *testing.T) {
	dir := t.TempDir()
	// Three segments; a full logical device of A*10 + B*10 + C*10.
	parts := [][]byte{
		bytes.Repeat([]byte("A"), 10),
		bytes.Repeat([]byte("B"), 10),
		bytes.Repeat([]byte("C"), 10),
	}
	paths := make([]string, len(parts))
	var want []byte
	for i, p := range parts {
		paths[i] = filepath.Join(dir, "img.00"+string(rune('1'+i)))
		if err := os.WriteFile(paths[i], p, 0o644); err != nil {
			t.Fatal(err)
		}
		want = append(want, p...)
	}

	im, err := OpenRawSegments(paths)
	if err != nil {
		t.Fatalf("OpenRawSegments: %v", err)
	}
	defer im.Close()

	if im.Size() != 30 {
		t.Fatalf("Size = %d, want 30", im.Size())
	}

	// Read the whole device in one call — must span both boundaries.
	full := make([]byte, 30)
	n, err := im.ReadAt(full, 0)
	if err != nil {
		t.Fatalf("ReadAt(full): %v", err)
	}
	if n != 30 || !bytes.Equal(full, want) {
		t.Fatalf("full read = %q, want %q", full[:n], want)
	}

	// Read a slice straddling the first boundary (offsets 8..12 → AABB).
	straddle := make([]byte, 4)
	if _, err := im.ReadAt(straddle, 8); err != nil {
		t.Fatalf("ReadAt(straddle): %v", err)
	}
	if got := string(straddle); got != "AABB" {
		t.Fatalf("straddle read = %q, want %q", got, "AABB")
	}

	// Reading at/after the end returns io.EOF.
	if _, err := im.ReadAt(make([]byte, 1), 30); err == nil {
		t.Fatalf("ReadAt past end: expected error, got nil")
	}
}
