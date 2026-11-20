package image

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	"indicer/internal/testutil/fsfixture"
)

// fuzzVHD opens a VHD or VHDX fixture with the fuzz input applied as patches
// (offset mod the size, then a byte to XOR in, five bytes each), with no
// parent resolution, and reads from where its headers say the disk is. An
// open may fail; nothing may panic, hang or read outside the disk.
func fuzzVHD(f *testing.F, name string) {
	var base []byte
	for _, m := range fsfixture.All(f) {
		if m.Name == name {
			b, err := os.ReadFile(fsfixture.Open(f, m))
			if err != nil {
				f.Fatal(err)
			}
			base = b
		}
	}
	if base == nil {
		f.Fatalf("no fixture %s", name)
	}
	f.Add([]byte{})
	f.Add(binary.LittleEndian.AppendUint32(nil, uint32(len(base)-512))) // the VHD footer
	f.Add(binary.LittleEndian.AppendUint32(nil, 64<<10))                // a VHDX header
	f.Fuzz(func(t *testing.T, patches []byte) {
		type undo struct {
			off int
			b   byte
		}
		var undos []undo
		for i := 0; i+5 <= len(patches) && i < 5*256; i += 5 {
			off := int(binary.LittleEndian.Uint32(patches[i:]) % uint32(len(base)))
			undos = append(undos, undo{off, base[off]})
			base[off] ^= patches[i+4]
		}
		defer func() {
			for i := len(undos) - 1; i >= 0; i-- {
				base[undos[i].off] = undos[i].b
			}
		}()
		img, err := OpenVHD(bytes.NewReader(base))
		if err != nil {
			return
		}
		defer img.Close()
		size := img.Size()
		if size < 0 {
			t.Fatalf("disk size %d", size)
		}
		buf := make([]byte, 4096)
		for _, off := range []int64{0, 511, size / 2, size - 4096, size - 1} {
			if off < 0 || off >= size {
				continue
			}
			if n, _ := img.ReadAt(buf, off); n > len(buf) || off+int64(n) > size {
				t.Fatalf("read %d bytes at %d of a %d-byte disk", n, off, size)
			}
		}
	})
}

func FuzzOpenVHD(f *testing.F)  { fuzzVHD(f, "vhd/fat16_dynamic_vhd") }
func FuzzOpenVHDX(f *testing.F) { fuzzVHD(f, "vhd/fat16_vhdx") }
