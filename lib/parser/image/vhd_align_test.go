package image

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
)

// sectorStartOnly reproduces the libvhdi v0.4.1 defect sectorAlignedReader
// works around: a read starting inside a sector returns bytes from the start
// of that sector instead.
type sectorStartOnly struct {
	data   []byte
	sector int64
}

func (r sectorStartOnly) ReadAt(p []byte, off int64) (int, error) {
	off -= off % r.sector
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestSectorAlignedReaderUnalignedStarts(t *testing.T) {
	const sector = 512
	data := testutil.RandomBytes(1, 4*sector+100)
	r := sectorAlignedReader{r: sectorStartOnly{data: data, sector: sector}, sector: sector}

	for off := int64(0); off < 3*sector; off += 37 {
		for _, n := range []int{1, 255, sector, 700, 1500} {
			want := data[off:]
			if len(want) > n {
				want = want[:n]
			}
			got := make([]byte, n)
			k, err := r.ReadAt(got, off)
			if k != len(want) || !bytes.Equal(got[:k], want) {
				t.Fatalf("ReadAt(len %d, off %d) = %d bytes, content mismatch", n, off, k)
			}
			if k < n && !errors.Is(err, io.EOF) {
				t.Fatalf("short read at off %d returned %v, want io.EOF", off, err)
			}
			if k == n && err != nil {
				t.Fatalf("full read at off %d returned %v", off, err)
			}
		}
	}

	if k, err := r.ReadAt(make([]byte, 8), int64(len(data))+3); k != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read past end = %d, %v; want 0, io.EOF", k, err)
	}
}

// corpusPartialChild is the first snapshot of the ubuntu chain. The block
// holding the ext4 root inode is PARTIALLY_PRESENT in it, and the inode starts
// 256 bytes into a sector. libyal vhdimount reads the inode as mode 040755 at
// this offset; libvhdi v0.4.1 alone returns inode 1.
const (
	corpusPartialChild = "ubuntu_5CBE8416-848F-4749-B953-7977EAE1B40B.avhdx"
	ubuntuRootInodeOff = 1132663040
)

func TestOpenVHDFileReadsUnalignedInPartialBlocks(t *testing.T) {
	dir := testutil.RequireEnvDir(t, "DUES_CORPUS_VHD")
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
