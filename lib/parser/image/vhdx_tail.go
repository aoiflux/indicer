package image

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/types"
)

// The last chunk of a differencing VHDX (PAR-33).
//
// A VHDX BAT interleaves one sector bitmap entry after every chunkRatio payload
// entries, and a differencing disk's BAT holds whole chunks: the bitmap entry
// for chunk c is at c*(ratio+1)+ratio even when the disk ends partway through
// that chunk. libvhdi v0.4.1 sizes the table as blocks+chunks entries, so when
// the last chunk is partial its bitmap entry falls outside and is taken as
// absent. Every PARTIALLY_PRESENT block in that chunk is then read wholly from
// the parent: the snapshot's own sectors there are replaced by older ones,
// with no error. A Hyper-V default 127 GiB disk ends 1536 blocks into its last
// 2048-block chunk, so its last 3 GiB are affected in every snapshot.
//
// tailResolver serves reads from the first affected offset on by walking the
// chain itself, with each differencing layer's BAT read from its file. Below
// that offset, and at any layer where no layer at or under it is affected,
// libvhdi serves the read.

// VHDX BAT entry states. Undefined (1) and unmapped (3) read as zeroes, like
// stateZero.
const (
	stateNotPresent = 0
	stateZero       = 2
	stateFull       = 6
	statePartial    = 7
)

// maxTailEntries bounds the BAT entries loaded per layer. The affected range
// is at most one chunk (2^23 sectors), so a valid disk needs at most 32769.
const maxTailEntries = 1 << 16

type batEntry struct {
	state  uint8
	offset int64 // payload file offset; meaningful for full and partial blocks
}

// vhdxLayer is one disk of a chain as the resolver sees it.
type vhdxLayer struct {
	// delegate reads this layer and everything under it through libvhdi.
	delegate io.ReaderAt

	// diff marks a differencing VHDX the resolver can resolve itself; the
	// fields below are set only for those.
	diff       bool
	src        io.ReaderAt // the layer's own file
	blockSize  int64
	sectorSize int64
	ratio      int64
	blocks     int64
	firstBlock int64      // the block of bat[0]
	bat        []batEntry // blocks firstBlock..blocks-1
	bitmaps    map[int64]int64

	// tailStart is the first virtual offset libvhdi misreads in this layer:
	// the start of its partial last chunk, or the disk size if it has none.
	tailStart int64
}

type tailResolver struct {
	layers  []*vhdxLayer
	size    int64
	start   int64
	minTail []int64 // minTail[i]: the lowest tailStart at or under layer i
}

func newTailResolver(layers []*vhdxLayer, size int64) (*tailResolver, error) {
	if len(layers) == 0 {
		return nil, errors.New("vhdx: empty chain")
	}
	r := &tailResolver{layers: layers, size: size, minTail: make([]int64, len(layers))}
	low := size
	for i := len(layers) - 1; i >= 0; i-- {
		low = min(low, layers[i].tailStart)
		r.minTail[i] = low
		if !layers[i].diff && i+1 < len(layers) && r.minTail[i+1] < size {
			return nil, fmt.Errorf("vhdx: chain layer %d cannot be resolved over the partial last chunk of a layer under it", i)
		}
	}
	r.start = r.minTail[0]
	return r, nil
}

func (r *tailResolver) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("vhdx: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	want := p
	if int64(len(p)) > r.size-off {
		want = p[:r.size-off]
	}
	if err := r.resolve(0, want, off); err != nil {
		return 0, err
	}
	if len(want) < len(p) {
		return len(want), io.EOF
	}
	return len(p), nil
}

// resolve fills p with the chain's bytes at off as seen from layer i.
func (r *tailResolver) resolve(i int, p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	if i >= len(r.layers) {
		return errors.New("vhdx: read falls through the base of the chain")
	}
	l := r.layers[i]
	if off+int64(len(p)) <= r.minTail[i] || !l.diff {
		return readFull(l.delegate, p, off)
	}
	if off < r.start {
		// Only the part from r.start on needs the walk.
		head := r.start - off
		if err := readFull(l.delegate, p[:head], off); err != nil {
			return err
		}
		p, off = p[head:], r.start
	}
	for len(p) > 0 {
		b, in := off/l.blockSize, off%l.blockSize
		seg := min(int64(len(p)), l.blockSize-in)
		if b < l.firstBlock || b-l.firstBlock >= int64(len(l.bat)) {
			return fmt.Errorf("vhdx: block %d outside the loaded table", b)
		}
		e := l.bat[b-l.firstBlock]
		var err error
		switch e.state {
		case stateNotPresent:
			err = r.resolve(i+1, p[:seg], off)
		case stateFull:
			err = readFull(l.src, p[:seg], e.offset+in)
		case statePartial:
			err = r.partial(i, l, e, b, in, p[:seg], off)
		default:
			// Undefined, zero and unmapped blocks read as zeroes, as libvhdi
			// reads them.
			clear(p[:seg])
		}
		if err != nil {
			return err
		}
		p, off = p[seg:], off+seg
	}
	return nil
}

// partial serves part of a PARTIALLY_PRESENT block: sectors its bitmap marks
// come from this layer's file, the rest from the parent.
func (r *tailResolver) partial(i int, l *vhdxLayer, e batEntry, b, in int64, p []byte, off int64) error {
	bm, ok := l.bitmaps[b/l.ratio]
	if !ok {
		return r.resolve(i+1, p, off)
	}
	spb := l.blockSize / l.sectorSize
	first, last := in/l.sectorSize, (in+int64(len(p))-1)/l.sectorSize
	bit0 := (b % l.ratio) * spb
	lo, hi := (bit0+first)/8, (bit0+last)/8
	bits := make([]byte, hi-lo+1)
	if err := readFull(l.src, bits, bm+lo); err != nil {
		return err
	}
	present := func(s int64) bool {
		pos := bit0 + s
		return bits[pos/8-lo]>>(pos%8)&1 == 1
	}
	for s := first; s <= last; {
		here := present(s)
		run := s + 1
		for run <= last && present(run) == here {
			run++
		}
		from := max(s*l.sectorSize, in) - in
		to := min(run*l.sectorSize, in+int64(len(p))) - in
		var err error
		if here {
			err = readFull(l.src, p[from:to], e.offset+in+from)
		} else {
			err = r.resolve(i+1, p[from:to], off+from)
		}
		if err != nil {
			return err
		}
		s = run
	}
	return nil
}

func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// vhdxLayout is what the resolver needs from a VHDX's headers.
type vhdxLayout struct {
	blockSize   int64
	sectorSize  int64
	virtualSize int64
	hasParent   bool
	batOffset   int64
	batLength   int64
}

var (
	regionBAT       = guid("2DC27766-F623-4200-9D64-115E9BFD4A08")
	regionMetadata  = guid("8B7CA206-4790-4B9A-B8FE-575F050F886E")
	metaFileParams  = guid("CAA16737-FA36-4D43-B3B6-33F0AA44E76B")
	metaVirtualSize = guid("2FA54224-CD1B-4876-B211-5DBED83BF4B8")
	metaLogicalSect = guid("8141BF1D-A96F-4709-BA47-F233A8FAAB5F")
	castagnoli      = crc32.MakeTable(crc32.Castagnoli)
)

// guid encodes a GUID string in the mixed-endian form VHDX stores.
func guid(s string) [16]byte {
	g, err := libvhdi.ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

// parseVHDXLayout reads the region table and the metadata items the resolver
// needs. The region table is used from whichever copy has a valid checksum.
func parseVHDXLayout(r io.ReaderAt, fileSize int64) (vhdxLayout, error) {
	var lay vhdxLayout
	const tableSize = 64 << 10
	table := make([]byte, tableSize)
	var regions map[[16]byte][2]int64
	for _, at := range []int64{192 << 10, 256 << 10} {
		if err := readFull(r, table, at); err != nil {
			return lay, err
		}
		if string(table[:4]) != "regi" {
			continue
		}
		sum := binary.LittleEndian.Uint32(table[4:])
		binary.LittleEndian.PutUint32(table[4:], 0)
		if crc32.Checksum(table, castagnoli) != sum {
			continue
		}
		n := binary.LittleEndian.Uint32(table[8:])
		if n > 2047 {
			continue
		}
		regions = map[[16]byte][2]int64{}
		for k := uint32(0); k < n; k++ {
			e := table[16+32*k:]
			var id [16]byte
			copy(id[:], e)
			regions[id] = [2]int64{int64(binary.LittleEndian.Uint64(e[16:])), int64(binary.LittleEndian.Uint32(e[24:]))}
		}
		break
	}
	if regions == nil {
		return lay, errors.New("vhdx: no valid region table")
	}
	bat, okB := regions[regionBAT]
	meta, okM := regions[regionMetadata]
	if !okB || !okM {
		return lay, errors.New("vhdx: region table lacks the BAT or metadata region")
	}
	for _, reg := range [][2]int64{bat, meta} {
		if reg[0] < 0 || reg[1] <= 0 || reg[0] > fileSize-reg[1] {
			return lay, errors.New("vhdx: region outside the file")
		}
	}
	lay.batOffset, lay.batLength = bat[0], bat[1]

	mt := make([]byte, min(meta[1], tableSize))
	if err := readFull(r, mt, meta[0]); err != nil {
		return lay, err
	}
	if len(mt) < 32 || string(mt[:8]) != "metadata" {
		return lay, errors.New("vhdx: bad metadata table")
	}
	n := int(binary.LittleEndian.Uint16(mt[10:]))
	item := func(want [16]byte, size int) ([]byte, error) {
		for k := 0; k < n && 32+32*k+32 <= len(mt); k++ {
			e := mt[32+32*k:]
			if [16]byte(e[:16]) != want {
				continue
			}
			o, ln := int64(binary.LittleEndian.Uint32(e[16:])), int(binary.LittleEndian.Uint32(e[20:]))
			if ln < size || o < 0 || o > meta[1]-int64(size) {
				return nil, errors.New("vhdx: metadata item outside its region")
			}
			b := make([]byte, size)
			return b, readFull(r, b, meta[0]+o)
		}
		return nil, errors.New("vhdx: metadata item missing")
	}
	fp, err := item(metaFileParams, 8)
	if err != nil {
		return lay, err
	}
	vs, err := item(metaVirtualSize, 8)
	if err != nil {
		return lay, err
	}
	ls, err := item(metaLogicalSect, 4)
	if err != nil {
		return lay, err
	}
	lay.blockSize = int64(binary.LittleEndian.Uint32(fp))
	lay.hasParent = binary.LittleEndian.Uint32(fp[4:])&2 != 0
	lay.virtualSize = int64(binary.LittleEndian.Uint64(vs))
	lay.sectorSize = int64(binary.LittleEndian.Uint32(ls))
	if lay.blockSize < 1<<20 || lay.blockSize > 256<<20 || lay.blockSize&(lay.blockSize-1) != 0 ||
		(lay.sectorSize != 512 && lay.sectorSize != 4096) || lay.virtualSize <= 0 {
		return lay, errors.New("vhdx: implausible geometry")
	}
	return lay, nil
}

// chunkRatio is the number of payload blocks one sector bitmap covers.
func (lay vhdxLayout) chunkRatio() int64 { return (1 << 23) * lay.sectorSize / lay.blockSize }

// loadBAT loads the entries for the blocks from start to the end of the disk,
// and the sector bitmaps of the chunks they are in.
func (l *vhdxLayer) loadBAT(r io.ReaderAt, lay vhdxLayout, ratio, start int64) error {
	blocks := (lay.virtualSize + lay.blockSize - 1) / lay.blockSize
	first := start / lay.blockSize
	if first >= blocks {
		first = blocks
	}
	lo := first + first/ratio
	lastPayload := (blocks - 1) + (blocks-1)/ratio
	hi := ((blocks-1)/ratio)*(ratio+1) + ratio + 1
	if limit := lay.batLength / 8; hi > limit {
		if lastPayload >= limit {
			return errors.New("vhdx: BAT region too small for the disk")
		}
		hi = limit
	}
	if hi-lo > maxTailEntries {
		return fmt.Errorf("vhdx: %d BAT entries past the last full chunk", hi-lo)
	}
	buf := make([]byte, 8*(hi-lo))
	if err := readFull(r, buf, lay.batOffset+8*lo); err != nil {
		return err
	}
	at := func(idx int64) (uint64, bool) {
		if idx < lo || idx >= hi {
			return 0, false
		}
		return binary.LittleEndian.Uint64(buf[8*(idx-lo):]), true
	}
	decode := func(v uint64) batEntry {
		return batEntry{state: uint8(v & 7), offset: int64(v>>20) << 20}
	}
	l.firstBlock, l.blocks, l.ratio = first, blocks, ratio
	l.blockSize, l.sectorSize = lay.blockSize, lay.sectorSize
	l.bat = make([]batEntry, 0, blocks-first)
	for b := first; b < blocks; b++ {
		v, _ := at(b + b/ratio)
		l.bat = append(l.bat, decode(v))
	}
	l.bitmaps = map[int64]int64{}
	for c := first / ratio; c <= (blocks-1)/ratio; c++ {
		if v, ok := at(c*(ratio+1) + ratio); ok && v&7 == stateFull {
			l.bitmaps[c] = decode(v).offset
		}
	}
	return nil
}

// newTailFix returns a reader for d that serves partial last chunks correctly,
// with the layer files it opened, or a nil reader when no layer of the chain
// has a partial last chunk.
func newTailFix(d *libvhdi.Disk) (io.ReaderAt, []io.Closer, error) {
	var disks []*libvhdi.Disk
	for x := d; x != nil; x = x.Parent() {
		disks = append(disks, x)
	}
	size := int64(d.Size())
	type info struct {
		lay   vhdxLayout
		ratio int64
		tail  int64
	}
	infos := make([]*info, len(disks))
	start := size
	var files []io.Closer
	fail := func(err error) (io.ReaderAt, []io.Closer, error) {
		for _, f := range files {
			f.Close()
		}
		return nil, nil, err
	}
	srcs := make([]io.ReaderAt, len(disks))
	for i, x := range disks {
		if !x.IsDifferencing() || x.Format() != types.FileFormatVHDX {
			continue
		}
		f, err := os.Open(x.Path())
		if err != nil {
			return fail(fmt.Errorf("vhdx: reopen %s: %w", x.Path(), err))
		}
		files = append(files, f)
		st, err := f.Stat()
		if err != nil {
			return fail(err)
		}
		lay, err := parseVHDXLayout(f, st.Size())
		if err != nil {
			return fail(fmt.Errorf("vhdx: %s: %w", x.Path(), err))
		}
		if lay.blockSize != int64(x.BlockSize()) || lay.sectorSize != int64(x.SectorSize()) ||
			lay.virtualSize != size || !lay.hasParent {
			return fail(fmt.Errorf("vhdx: %s: headers disagree with the library's reading", x.Path()))
		}
		in := &info{lay: lay, ratio: lay.chunkRatio(), tail: size}
		blocks := (size + lay.blockSize - 1) / lay.blockSize
		if blocks%in.ratio != 0 {
			in.tail = (blocks - blocks%in.ratio) * lay.blockSize
		}
		start = min(start, in.tail)
		infos[i], srcs[i] = in, f
	}
	if start >= size {
		return fail(nil)
	}

	layers := make([]*vhdxLayer, len(disks))
	for i, x := range disks {
		sector := int64(x.SectorSize())
		if sector <= 0 {
			sector = types.DefaultSectorSize
		}
		l := &vhdxLayer{delegate: sectorAlignedReader{r: x, sector: sector}, tailStart: size}
		if in := infos[i]; in != nil {
			if x.HasLog() {
				// The resolver reads the BAT as stored; a log would change it.
				return fail(fmt.Errorf("vhdx: %s has a log to replay; its last chunk cannot be read correctly", x.Path()))
			}
			l.diff, l.src, l.tailStart = true, srcs[i], in.tail
			if err := l.loadBAT(srcs[i], in.lay, in.ratio, start); err != nil {
				return fail(fmt.Errorf("vhdx: %s: %w", x.Path(), err))
			}
		}
		layers[i] = l
	}
	r, err := newTailResolver(layers, size)
	if err != nil {
		return fail(err)
	}
	return r, files, nil
}
