package compression

import (
	"bytes"
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func streamed(t *testing.T, parts ...[]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, p := range parts {
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(p); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestDecompressLimited(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefgh"), 4096)
	one, err := Compress(data, "fast")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		in    []byte
		limit int64
		want  int // decoded length, or -1 for ErrLimitExceeded
	}{
		{"declared size, at the limit", one, int64(len(data)), len(data)},
		{"declared size, over the limit", one, int64(len(data)) - 1, -1},
		{"no declared size, over the limit", streamed(t, data), int64(len(data)) - 1, -1},
		{"two frames, each under the limit", streamed(t, data, data), int64(len(data)) * 3 / 2, -1},
		{"two frames, under the limit", streamed(t, data, data), int64(len(data)) * 2, 2 * len(data)},
	}
	for _, c := range cases {
		out, err := DecompressLimited(c.in, c.limit)
		switch {
		case c.want < 0 && !errors.Is(err, ErrLimitExceeded):
			t.Errorf("%s: %d bytes, %v; want ErrLimitExceeded", c.name, len(out), err)
		case c.want >= 0 && (err != nil || !bytes.Equal(out, bytes.Repeat(data, c.want/len(data)))):
			t.Errorf("%s: %d bytes, %v; want %d", c.name, len(out), err, c.want)
		}
	}
	if _, err := DecompressLimited([]byte("not zstd"), 1<<20); err == nil || errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("garbage: %v", err)
	}
}
