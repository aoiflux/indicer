package parser

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil/fsfixture"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fsfixture.Cleanup()
	os.Exit(code)
}

// TestParseMBRFindsExFATPartition wraps the exFAT fixture volume in an MBR
// disk at LBA 2048 and checks that the fallback partition parser reports it
// at its byte offset and length.
func TestParseMBRFindsExFATPartition(t *testing.T) {
	var vol []byte
	for _, m := range fsfixture.All(t) {
		if m.Name == "exfat/exfat" {
			b, err := os.ReadFile(fsfixture.Open(t, m))
			if err != nil {
				t.Fatal(err)
			}
			vol = b
		}
	}
	if vol == nil {
		t.Fatal("fixture exfat/exfat not found")
	}
	const startLBA = 2048
	disk := make([]byte, startLBA*512+len(vol))
	copy(disk[startLBA*512:], vol)
	entry := disk[446:462]
	entry[4] = 0x07 // exFAT/NTFS
	binary.LittleEndian.PutUint32(entry[8:], startLBA)
	binary.LittleEndian.PutUint32(entry[12:], uint32(len(vol)/512))
	disk[510], disk[511] = 0x55, 0xAA

	path := filepath.Join(t.TempDir(), "mbr_exfat.img")
	if err := os.WriteFile(path, disk, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	parts := parseMBR(int64(len(disk)), f)
	if len(parts) != 1 {
		t.Fatalf("got %d partitions, want 1: %+v", len(parts), parts)
	}
	if parts[0].Start != startLBA*512 || parts[0].Size != int64(len(vol)) {
		t.Fatalf("partition at %d size %d, want %d size %d", parts[0].Start, parts[0].Size, startLBA*512, len(vol))
	}
}
