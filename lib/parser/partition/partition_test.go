package partition

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/aoiflux/libtable"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

func TestParseMBR(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func mbrImage(total int64, entries ...[2]uint32) []byte {
	buf := make([]byte, total)
	for i, e := range entries {
		ent := buf[446+16*i : 446+16*(i+1)]
		ent[4] = 0x83
		binary.LittleEndian.PutUint32(ent[8:12], e[0])
		binary.LittleEndian.PutUint32(ent[12:16], e[1])
	}
	buf[510], buf[511] = 0x55, 0xAA
	return buf
}

func hasIssue(is []core.Issue, c core.IssueCode) bool {
	for _, i := range is {
		if i.Code == c {
			return true
		}
	}
	return false
}

// TestParseClampsToImage: a partition running past the end of the image is
// cut to the image and reported (PAR-17). (libtable refuses an MBR whose
// entry starts past the image, so that case never reaches the clamp.)
func TestParseClampsToImage(t *testing.T) {
	t.Parallel()
	const total = 1 << 20 // 2048 sectors
	buf := mbrImage(total, [2]uint32{2048 - 100, 1000}, [2]uint32{100, 10})
	img := image.NewRaw(bytes.NewReader(buf), total)
	tbl, issues, err := ParseReport(img)
	if err != nil {
		t.Fatal(err)
	}
	alloc := tbl.Allocated()
	if len(alloc) != 2 {
		t.Fatalf("partitions %+v", alloc)
	}
	// Sorted by start: the straddling partition is second.
	if alloc[1].Offset != (2048-100)*512 || alloc[1].Size != 100*512 {
		t.Fatalf("partition not clamped to the image: %+v", alloc[1])
	}
	if !hasIssue(issues, core.IssueClamped) {
		t.Fatalf("issues %v", issues)
	}
}

// TestConvertRefusesUnaddressableRanges: an entry whose byte range wraps or
// passes 2^63 (StartLBA past 2^54 at 512-byte sectors) is kept at the image
// end with size 0 and reported, never turned into a small, plausible offset
// (TABLE-U1).
func TestConvertRefusesUnaddressableRanges(t *testing.T) {
	t.Parallel()
	const size = 1 << 30
	tbl := &libtable.Table{BlockSize: 512, Partitions: []libtable.Partition{
		{Index: 1, StartLBA: 2048, LengthLBA: 4096, Flags: libtable.PartFlagAlloc},
		{Index: 2, StartLBA: 1 << 55, LengthLBA: 8, Flags: libtable.PartFlagAlloc},
		{Index: 3, StartLBA: 4096, LengthLBA: 1 << 60, Flags: libtable.PartFlagAlloc},
	}}
	out, issues := convert(tbl, size)
	if p := out.Partitions[0]; p.Offset != 2048*512 || p.Size != 4096*512 {
		t.Fatalf("ordinary partition: %+v", p)
	}
	for _, p := range out.Partitions[1:] {
		if p.Offset != size || p.Size != 0 {
			t.Errorf("partition %d: offset %d size %d; want the image end and 0", p.Index, p.Offset, p.Size)
		}
	}
	n := 0
	for _, i := range issues {
		if i.Code == core.IssueClamped {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("issues %v; want two partition_clamped", issues)
	}
}

// TestParseReportsTableWarnings: overlapping entries reach the report.
func TestParseReportsTableWarnings(t *testing.T) {
	t.Parallel()
	const total = 1 << 20
	buf := mbrImage(total, [2]uint32{100, 400}, [2]uint32{300, 400})
	tbl, issues, err := ParseReport(image.NewRaw(bytes.NewReader(buf), total))
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Allocated()) != 2 || !hasIssue(issues, core.IssueOverlap) {
		t.Fatalf("partitions %+v, issues %v", tbl.Allocated(), issues)
	}
}
