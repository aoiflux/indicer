package fs

import (
	"bytes"
	"errors"
	"testing"

	"indicer/lib/parser/core"
)

func TestDetect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func([]byte)
		want   core.FSType
		status core.PartitionStatus
		detail string
	}{
		{"ntfs", func(b []byte) { copy(b[3:], "NTFS    ") }, core.FSNTFS, "", ""},
		{"exfat", func(b []byte) { copy(b[3:], "EXFAT   ") }, core.FSExFAT, "", ""},
		{"fat32", func(b []byte) { copy(b[82:], "FAT32   ") }, core.FSFAT, "", ""},
		{"fat16", func(b []byte) { copy(b[54:], "FAT16   ") }, core.FSFAT, "", ""},
		{"fat12", func(b []byte) { copy(b[54:], "FAT12   ") }, core.FSFAT, "", ""},
		{"fat32 backup boot sector only", func(b []byte) { copy(b[6*512+82:], "FAT32   ") }, core.FSFAT, "", ""},
		{"xfs", func(b []byte) { copy(b[0:], "XFSB") }, core.FSXFS, "", ""},
		{"hfs+", func(b []byte) { b[1024], b[1025] = 'H', '+' }, core.FSHFS, "", ""},
		{"hfsx", func(b []byte) { b[1024], b[1025] = 'H', 'X' }, core.FSHFS, "", ""},
		{"classic hfs", func(b []byte) { b[1024], b[1025] = 'B', 'D' }, core.FSHFS, "", ""},
		{"ext", func(b []byte) { b[1080], b[1081] = 0x53, 0xEF }, core.FSExt, "", ""},
		{"bitlocker", func(b []byte) { copy(b[3:], "-FVE-FS-") }, core.FSUnknown, core.PartEncrypted, "BitLocker"},
		{"luks", func(b []byte) { copy(b[0:], "LUKS\xba\xbe") }, core.FSUnknown, core.PartEncrypted, "LUKS"},
		{"apfs", func(b []byte) { copy(b[32:], "NXSB") }, core.FSUnknown, core.PartUnsupported, "APFS"},
		{"refs", func(b []byte) { copy(b[3:], "ReFS\x00\x00\x00\x00") }, core.FSUnknown, core.PartUnsupported, "ReFS"},
		{"btrfs", func(b []byte) { copy(b[0x10040:], "_BHRfS_M") }, core.FSUnknown, core.PartUnsupported, "btrfs"},
		{"lvm2", func(b []byte) { copy(b[512:], "LABELONE"); copy(b[512+24:], "LVM2 001") }, core.FSUnknown, core.PartUnsupported, "LVM2"},
		{"swap", func(b []byte) { copy(b[4096-10:], "SWAPSPACE2") }, core.FSUnknown, core.PartUnsupported, "swap"},
		{"unknown", func(b []byte) {}, core.FSUnknown, core.PartUnknown, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := make([]byte, 128<<10)
			c.setup(buf)
			d, err := DetectErr(bytes.NewReader(buf))
			if err != nil {
				t.Fatal(err)
			}
			if d.Type != c.want || d.Status != c.status || d.Detail != c.detail {
				t.Fatalf("DetectErr = %+v, want %q %q %q", d, c.want, c.status, c.detail)
			}
			if got := Detect(bytes.NewReader(buf)); got != c.want {
				t.Fatalf("Detect = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDetectShortVolume(t *testing.T) {
	t.Parallel()
	// A volume smaller than the probe windows is not a read error.
	buf := make([]byte, 2048)
	copy(buf[3:], "NTFS    ")
	d, err := DetectErr(bytes.NewReader(buf))
	if err != nil || d.Type != core.FSNTFS {
		t.Fatalf("DetectErr = %+v, %v", d, err)
	}
}

type failingReader struct{}

func (failingReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("device not ready") }

func TestDetectReadError(t *testing.T) {
	t.Parallel()
	if _, err := DetectErr(failingReader{}); err == nil {
		t.Fatal("a read error must not look like an unknown filesystem (PAR-16)")
	}
}

func TestOpenUnknownFS(t *testing.T) {
	t.Parallel()
	if _, err := Open(core.FSUnknown, core.OpenParams{Reader: bytes.NewReader(make([]byte, 512)), Size: 512}); err != ErrUnknownFS {
		t.Fatalf("Open(unknown) err = %v, want ErrUnknownFS", err)
	}
	if Supported(core.FSUnknown) {
		t.Fatal("Supported(unknown) = true, want false")
	}
	if !Supported(core.FSNTFS) {
		t.Fatal("Supported(ntfs) = false, want true")
	}
}

func FuzzDetect(f *testing.F) {
	f.Add([]byte("\xeb\x52\x90NTFS    "))
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := DetectErr(bytes.NewReader(b))
		if err == nil && d.Type != core.FSUnknown && d.Status != "" {
			t.Fatalf("supported type with a status: %+v", d)
		}
	})
}
