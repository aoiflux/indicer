package partition

import (
	"bytes"
	"encoding/binary"
	"testing"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

func TestParseMBR(t *testing.T) {
	const sector = 512
	const partLBA = 2048
	const partSectors = 100
	total := int64((partLBA + partSectors + 1) * sector)

	buf := make([]byte, total)
	// One MBR partition entry at offset 446.
	e := buf[446:462]
	e[0] = 0x80 // bootable
	e[4] = 0x0c // type: FAT32 (LBA)
	binary.LittleEndian.PutUint32(e[8:12], partLBA)
	binary.LittleEndian.PutUint32(e[12:16], partSectors)
	buf[510], buf[511] = 0x55, 0xAA // MBR signature

	img := image.NewRaw(bytes.NewReader(buf), total)
	defer img.Close()

	tbl, err := Parse(img)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tbl.Scheme != core.SchemeMBR {
		t.Fatalf("scheme = %q, want mbr", tbl.Scheme)
	}
	alloc := tbl.Allocated()
	if len(alloc) != 1 {
		t.Fatalf("allocated partitions = %d, want 1", len(alloc))
	}
	if got := alloc[0].Offset; got != partLBA*sector {
		t.Fatalf("offset = %d, want %d", got, partLBA*sector)
	}
	if got := alloc[0].Size; got != partSectors*sector {
		t.Fatalf("size = %d, want %d", got, partSectors*sector)
	}
}

func TestParseWholeDeviceFallback(t *testing.T) {
	// No MBR signature / no recognised scheme → whole-device fallback.
	buf := make([]byte, 4096)
	img := image.NewRaw(bytes.NewReader(buf), int64(len(buf)))
	defer img.Close()

	tbl, err := Parse(img)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tbl.Scheme != core.SchemeNone {
		t.Fatalf("scheme = %q, want none", tbl.Scheme)
	}
	if len(tbl.Partitions) != 1 || tbl.Partitions[0].Size != 4096 {
		t.Fatalf("expected single whole-device partition of 4096 bytes, got %+v", tbl.Partitions)
	}
}
