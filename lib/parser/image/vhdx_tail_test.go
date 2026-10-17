package image

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
)

// sparseFile is an io.ReaderAt over a few byte runs at arbitrary offsets,
// zero elsewhere.
type sparseFile map[int64][]byte

func (f sparseFile) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	for at, b := range f {
		lo, hi := max(off, at), min(off+int64(len(p)), at+int64(len(b)))
		if lo < hi {
			copy(p[lo-off:hi-off], b[lo-at:hi-at])
		}
	}
	return len(p), nil
}

// tailModel describes a differencing layer for the resolver tests: the state
// of each block, and for partially present blocks which sectors it holds.
type tailModel struct {
	states  map[int64]uint8
	present map[int64][]int64 // block -> sectors
}

const (
	tBlockSize  = 2048
	tSectorSize = 512
	tRatio      = 4
	tBlocks     = 10 // chunks 0 and 1 full, chunk 2 holds blocks 8 and 9 only
	tSize       = tBlocks * tBlockSize
)

// buildTailLayer lays a model out as a raw layer file and the decoded BAT the
// resolver works from, and returns the bytes the layer holds for each virtual
// offset it backs.
func buildTailLayer(seed int64, m tailModel, delegate io.ReaderAt, start int64) (*vhdxLayer, map[int64]byte) {
	file := sparseFile{}
	held := map[int64]byte{}
	l := &vhdxLayer{
		diff:       true,
		blockSize:  tBlockSize,
		sectorSize: tSectorSize,
		ratio:      tRatio,
		blocks:     tBlocks,
		firstBlock: start / tBlockSize,
		bitmaps:    map[int64]int64{},
		delegate:   delegate,
	}
	l.tailStart = (tBlocks - tBlocks%tRatio) * tBlockSize
	next := int64(1 << 20)
	bitmapAt := map[int64][]byte{}
	for b := l.firstBlock; b < tBlocks; b++ {
		st := m.states[b]
		e := batEntry{state: st}
		if st == stateFull || st == statePartial {
			data := testutil.RandomBytes(uint64(seed*1000+b), tBlockSize)
			file[next] = data
			e.offset = next
			next += 1 << 20
			for s := int64(0); s < tBlockSize/tSectorSize; s++ {
				if st == stateFull {
					for k := int64(0); k < tSectorSize; k++ {
						held[b*tBlockSize+s*tSectorSize+k] = data[s*tSectorSize+k]
					}
				}
			}
			for _, s := range m.present[b] {
				c := b / tRatio
				if bitmapAt[c] == nil {
					bitmapAt[c] = make([]byte, tRatio*tBlockSize/tSectorSize/8)
				}
				bit := (b%tRatio)*(tBlockSize/tSectorSize) + s
				bitmapAt[c][bit/8] |= 1 << (bit % 8)
				for k := int64(0); k < tSectorSize; k++ {
					held[b*tBlockSize+s*tSectorSize+k] = data[s*tSectorSize+k]
				}
			}
		}
		l.bat = append(l.bat, e)
	}
	for c, bm := range bitmapAt {
		file[next] = bm
		l.bitmaps[c] = next
		next += 1 << 20
	}
	l.src = file
	return l, held
}

// TestVHDXTailResolver checks reads in a differencing chain whose last chunk
// is partial against the content each layer holds: a sector comes from the
// nearest layer holding it, else the base. The delegates model libvhdi
// v0.4.1, which loses the last chunk's sector bitmap and so reads every
// partially present block there from the parent.
func TestVHDXTailResolver(t *testing.T) {
	base := testutil.RandomBytes(7, tSize)
	start := int64((tBlocks - tBlocks%tRatio) * tBlockSize)

	mid, midHeld := buildTailLayer(2, tailModel{
		states:  map[int64]uint8{8: statePartial, 9: stateNotPresent},
		present: map[int64][]int64{8: {0, 3}},
	}, nil, start)
	top, topHeld := buildTailLayer(1, tailModel{
		states:  map[int64]uint8{8: statePartial, 9: stateFull},
		present: map[int64][]int64{8: {1}},
	}, nil, start)
	// zero is a top layer whose last block reads as zeroes.
	zero, _ := buildTailLayer(3, tailModel{states: map[int64]uint8{8: stateNotPresent, 9: stateZero}}, nil, start)

	want := func(layers ...map[int64]byte) []byte {
		out := bytes.Clone(base)
		for i := range out {
			for _, h := range layers {
				if v, ok := h[int64(i)]; ok {
					out[i] = v
					break
				}
			}
		}
		return out
	}
	// The delegates are only consulted below the tail, where every layer reads
	// the base in this model.
	baseLayer := &vhdxLayer{delegate: bytes.NewReader(base), tailStart: tSize}
	mid.delegate = bytes.NewReader(base)
	top.delegate = bytes.NewReader(base)
	zero.delegate = bytes.NewReader(base)

	cases := []struct {
		name   string
		layers []*vhdxLayer
		want   []byte
	}{
		{"two layers", []*vhdxLayer{mid, baseLayer}, want(midHeld)},
		{"three layers", []*vhdxLayer{top, mid, baseLayer}, want(topHeld, midHeld)},
		{"zero block", []*vhdxLayer{zero, mid, baseLayer}, func() []byte {
			w := want(midHeld)
			clear(w[9*tBlockSize:])
			return w
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := newTailResolver(tc.layers, tSize)
			if err != nil {
				t.Fatal(err)
			}
			for off := int64(0); off < tSize; off += 211 {
				for _, n := range []int64{1, 300, tSectorSize, 1500, 5000} {
					n = min(n, tSize-off)
					got := make([]byte, n)
					if k, err := r.ReadAt(got, off); k != int(n) || err != nil {
						t.Fatalf("ReadAt(%d, %d) = %d, %v", n, off, k, err)
					}
					if !bytes.Equal(got, tc.want[off:off+n]) {
						t.Fatalf("ReadAt(%d, %d): content differs from the chain's", n, off)
					}
				}
			}
		})
	}
}

// TestLoadVHDXBATLastChunkBitmap reads a BAT whose last chunk is partial. The
// VHDX specification sizes a differencing BAT as whole chunks, so the last
// chunk's sector bitmap entry sits after its blocks' entries at
// c*(ratio+1)+ratio, past blocks+chunks entries. libvhdi v0.4.1 stops at
// blocks+chunks and loses it.
func TestLoadVHDXBATLastChunkBitmap(t *testing.T) {
	entry := func(state uint8, mib int64) uint64 { return uint64(mib)<<20 | uint64(state) }
	raw := make([]uint64, 3*(tRatio+1))
	for b := int64(0); b < tBlocks; b++ {
		raw[b+b/tRatio] = entry(statePartial, 10+b)
	}
	for c := int64(0); c < 3; c++ {
		raw[c*(tRatio+1)+tRatio] = entry(stateFull, 100+c)
	}
	buf := make([]byte, 8*len(raw))
	for i, v := range raw {
		binary.LittleEndian.PutUint64(buf[8*i:], v)
	}
	lay := vhdxLayout{blockSize: tBlockSize, sectorSize: tSectorSize, virtualSize: tSize, batOffset: 4096, batLength: int64(len(buf))}
	l := &vhdxLayer{}
	if err := l.loadBAT(sparseFile{4096: buf}, lay, tRatio, 6*tBlockSize); err != nil {
		t.Fatal(err)
	}
	if l.firstBlock != 6 || len(l.bat) != 4 {
		t.Fatalf("loaded blocks %d.., %d entries; want 6.., 4", l.firstBlock, len(l.bat))
	}
	for i, e := range l.bat {
		b := l.firstBlock + int64(i)
		if e.state != statePartial || e.offset != (10+b)<<20 {
			t.Fatalf("block %d = %+v", b, e)
		}
	}
	if got := l.bitmaps[2]; got != 102<<20 {
		t.Fatalf("last chunk bitmap at %d, want %d", got, 102<<20)
	}
	if got := l.bitmaps[1]; got != 101<<20 {
		t.Fatalf("chunk 1 bitmap at %d, want %d", got, 101<<20)
	}
}

// corpusTailLayer is a snapshot of the ubuntu chain (not the live leaf). Block
// 63552 lies in its partial last chunk and is PARTIALLY_PRESENT, holding ext4
// extent tree nodes; libyal vhdimount reads them at these offsets, while
// libvhdi v0.4.1 alone reads the base disk's older bytes there.
const corpusTailLayer = "ubuntu_B6F397AF-91E2-41AC-B269-1334DF75AD38.avhdx"

func TestOpenVHDFileReadsPartialLastChunk(t *testing.T) {
	dir := testutil.RequireEnvDir(t, "DUES_CORPUS_VHD")
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
