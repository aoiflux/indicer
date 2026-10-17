package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

// fuzzPatched scans a fixture with the fuzz input applied as patches: each
// 5 bytes are an offset (mod the image size) and a byte to XOR in. Patching
// a real volume reaches far deeper into a parser than random bytes would.
func fuzzPatched(f *testing.F, name string) {
	base := rawFixture(f, fixtureByNameF(f, name))
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 0xff})
	f.Add(binary.LittleEndian.AppendUint32(nil, 1024) /* superblock area */)
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
		img := image.NewRaw(bytes.NewReader(base), int64(len(base)))
		_, _ = Scan(context.Background(), img, Visitor{File: func(_ core.PartitionInfo, rec core.FileRecord) error {
			for _, e := range rec.Content.Extents {
				if e.Kind == core.ExtentData && (e.ImageOffset < 0 || e.ImageOffset+e.Length > int64(len(base))) {
					t.Fatalf("%s: extent outside the image", rec.Path)
				}
			}
			return nil
		}}, Options{})
	})
}

func fixtureByNameF(tb testing.TB, name string) fixture.Manifest {
	for _, m := range fixture.All(tb) {
		if m.Name == name {
			return m
		}
	}
	tb.Fatalf("no fixture %s", name)
	return fixture.Manifest{}
}

func FuzzScanFAT(f *testing.F)   { fuzzPatched(f, "fat/fat12") }
func FuzzScanExFAT(f *testing.F) { fuzzPatched(f, "exfat/exfat") }
func FuzzScanNTFS(f *testing.F)  { fuzzPatched(f, "ntfs/ntfs_compressed") }
func FuzzScanExt(f *testing.F)   { fuzzPatched(f, "ext/ext4") }
func FuzzScanHFS(f *testing.F)   { fuzzPatched(f, "hfs/hfs_classic") }
