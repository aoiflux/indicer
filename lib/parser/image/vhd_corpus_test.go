package image

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
)

// corpusVHD is the real corpus's directory of VHD and VHDX images, which
// only some machines have:
//
//	go test ./lib/parser/image -args -dues.corpus-vhd=DIR
var corpusVHD = testutil.NewDirFlag("dues.corpus-vhd", "a directory of real VHD and VHDX images, Hyper-V snapshot chains among them")

// The tests in this file read Hyper-V snapshot chains of the real corpus
// (-dues.corpus-vhd). They are the regression checks for two libvhdi defects
// DUES worked around until libvhdi v0.4.2 fixed them, and they failed against
// v0.4.1 read without the workarounds.

// corpusPartialChild is the first snapshot of the ubuntu chain. The block
// holding the ext4 root inode is PARTIALLY_PRESENT in it, and the inode starts
// 256 bytes into a sector. libyal vhdimount reads the inode as mode 040755 at
// this offset; libvhdi v0.4.1 returned inode 1 (PAR-31).
const (
	corpusPartialChild = "ubuntu_5CBE8416-848F-4749-B953-7977EAE1B40B.avhdx"
	ubuntuRootInodeOff = 1132663040
)

func TestOpenVHDFileReadsUnalignedInPartialBlocks(t *testing.T) {
	dir := corpusVHD.Require(t)
	path := filepath.Join(dir, corpusPartialChild)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("corpus image not present: %v", err)
	}
	im, err := OpenVHDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close()

	inode := make([]byte, 8)
	if _, err := im.ReadAt(inode, ubuntuRootInodeOff); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0xed, 0x41, 0, 0, 0, 0x10, 0, 0}; !bytes.Equal(inode, want) {
		t.Fatalf("root inode head = % x, want % x (mode 040755, size 4096)", inode, want)
	}

	const sector = 512
	base := int64(ubuntuRootInodeOff) &^ (sector - 1)
	aligned := make([]byte, 2*sector)
	if _, err := im.ReadAt(aligned, base); err != nil {
		t.Fatal(err)
	}
	for _, k := range []int64{1, 100, 256, 511, 600} {
		got := make([]byte, 64)
		if _, err := im.ReadAt(got, base+k); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, aligned[k:k+64]) {
			t.Fatalf("read at sector offset %d differs from the aligned read", k)
		}
	}
}

// corpusTailLayer is a snapshot of the ubuntu chain (not the live leaf). Block
// 63552 lies in its partial last chunk and is PARTIALLY_PRESENT, holding ext4
// extent tree nodes; libyal vhdimount reads them at these offsets, while
// libvhdi v0.4.1 read the base disk's older bytes there (PAR-33).
const corpusTailLayer = "ubuntu_B6F397AF-91E2-41AC-B269-1334DF75AD38.avhdx"

func TestOpenVHDFileReadsPartialLastChunk(t *testing.T) {
	dir := corpusVHD.Require(t)
	path := filepath.Join(dir, corpusTailLayer)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("corpus image not present: %v", err)
	}
	im, err := OpenVHDFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close()
	for off, want := range map[int64]string{
		133279600640: "0af30900540100000000000030070100",
		133279596544: "0af351005401000000000000d00d0100",
	} {
		got := make([]byte, 16)
		if _, err := im.ReadAt(got, off); err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != want {
			t.Fatalf("@%d = %x, want %s (an ext4 extent node)", off, got, want)
		}
		// The same bytes through an unaligned start.
		got = make([]byte, 12)
		if _, err := im.ReadAt(got, off+4); err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(got) != want[8:] {
			t.Fatalf("@%d = %x, want %s", off+4, got, want[8:])
		}
	}
}
