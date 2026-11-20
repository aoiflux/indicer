package ntfs

import (
	"bytes"
	"errors"
	"testing"
)

func TestLZNT1(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  []byte
		want []byte
	}{
		{
			// flags 0b10: a literal 'a', then a back-reference one byte back
			// for 9 bytes (token 0x0006: offset 0+1, length 6+3).
			name: "back-reference",
			src:  []byte{0x03, 0xB0, 0x02, 'a', 0x06, 0x00},
			want: bytes.Repeat([]byte("a"), 10),
		},
		{
			name: "literals",
			src:  []byte{0x04, 0xB0, 0x00, 'n', 't', 'f', 's'},
			want: []byte("ntfs"),
		},
		{
			name: "stored chunk",
			src:  append([]byte{0x02, 0x30}, "raw"...),
			want: []byte("raw"),
		},
		{
			name: "end marker",
			src:  []byte{0x04, 0xB0, 0x00, 'e', 'n', 'd', '!', 0x00, 0x00, 0xff, 0xff},
			want: []byte("end!"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := make([]byte, 64)
			n, err := lznt1(dst, tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(dst[:n], tc.want) {
				t.Fatalf("got %q want %q", dst[:n], tc.want)
			}
		})
	}
}

func TestLZNT1RejectsCorruptData(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"reference before the chunk": {0x03, 0xB0, 0x01, 0x00, 0x00},
		"chunk past the end":         {0x10, 0xB0, 0x00, 'x'},
		"truncated reference":        {0x02, 0xB0, 0x01, 0x00},
		"overruns the unit":          {0x03, 0xB0, 0x02, 'a', 0xff, 0x0f},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := lznt1(make([]byte, 16), src); !errors.Is(err, errLZNT1) {
				t.Fatalf("err %v, want errLZNT1", err)
			}
		})
	}
}

func FuzzLZNT1(f *testing.F) {
	f.Add([]byte{0x03, 0xB0, 0x02, 'a', 0x06, 0x00})
	f.Add([]byte{0x02, 0x30, 'r', 'a', 'w'})
	f.Fuzz(func(t *testing.T, src []byte) {
		dst := make([]byte, 4096)
		n, _ := lznt1(dst, src)
		if n < 0 || n > len(dst) {
			t.Fatalf("wrote %d of %d", n, len(dst))
		}
	})
}
