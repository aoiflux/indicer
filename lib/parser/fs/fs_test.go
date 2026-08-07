package fs

import (
	"bytes"
	"testing"

	"indicer/lib/parser/core"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		setup func([]byte)
		want  core.FSType
	}{
		{"ntfs", func(b []byte) { copy(b[3:], "NTFS    ") }, core.FSNTFS},
		{"exfat", func(b []byte) { copy(b[3:], "EXFAT   ") }, core.FSExFAT},
		{"fat32", func(b []byte) { copy(b[82:], "FAT32   ") }, core.FSFAT},
		{"fat16", func(b []byte) { copy(b[54:], "FAT16   ") }, core.FSFAT},
		{"fat12", func(b []byte) { copy(b[54:], "FAT12   ") }, core.FSFAT},
		{"xfs", func(b []byte) { copy(b[0:], "XFSB") }, core.FSXFS},
		{"hfs+", func(b []byte) { b[1024], b[1025] = 'H', '+' }, core.FSHFS},
		{"hfsx", func(b []byte) { b[1024], b[1025] = 'H', 'X' }, core.FSHFS},
		{"ext", func(b []byte) { b[1080], b[1081] = 0x53, 0xEF }, core.FSExt},
		{"unknown", func(b []byte) {}, core.FSUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := make([]byte, 2048)
			c.setup(buf)
			if got := Detect(bytes.NewReader(buf)); got != c.want {
				t.Fatalf("Detect = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOpenUnknownFS(t *testing.T) {
	if _, err := Open(core.FSUnknown, bytes.NewReader(make([]byte, 512)), 0, 512); err != ErrUnknownFS {
		t.Fatalf("Open(unknown) err = %v, want ErrUnknownFS", err)
	}
	if Supported(core.FSUnknown) {
		t.Fatal("Supported(unknown) = true, want false")
	}
	if !Supported(core.FSNTFS) {
		t.Fatal("Supported(ntfs) = false, want true")
	}
}
