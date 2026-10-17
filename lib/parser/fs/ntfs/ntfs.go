// Package ntfs adapts libntfs to core.Filesystem: MACB times, live and
// deleted entries, resident (inline), compressed and sparse content,
// alternate data streams, hard links and orphaned records.
package ntfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aoiflux/libntfs"

	"indicer/internal/safe"
	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

// firstUserRecord is the first MFT record that is not reserved for the
// filesystem's own metadata files.
const firstUserRecord = 24

// orphanDir holds records no directory reaches.
const orphanDir = "/$ORPHAN"

type ntfsFS struct {
	vol *libntfs.Volume
	p   core.OpenParams

	bitmapOnce sync.Once
	bitmap     []byte // $Bitmap, loaded the first time a deleted file is located
	bitmapErr  error
}

var _ core.Filesystem = (*ntfsFS)(nil)

// streamRef is the Ref of an alternate data stream node.
type streamRef struct{ name string }

// indexRef is the Ref of a deleted index entry: the $FILE_NAME copy the
// index kept is all that may be left of the file.
type indexRef struct{ e libntfs.DirEntry }

// Open mounts an NTFS volume from p.Reader, which starts at the boot sector.
// The volume is opened read-only, so a reader that also implements
// io.WriterAt can never be written through, and with no base offset, so
// libntfs reports partition-relative offsets and p.Base is added once, here.
func Open(p core.OpenParams) (core.Filesystem, error) {
	vol, err := libntfs.OpenWithOptions(p.Reader, libntfs.Options{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if vol.BaseOffset() != 0 {
		_ = vol.Close()
		return nil, fmt.Errorf("ntfs: unexpected base offset %d", vol.BaseOffset())
	}
	return &ntfsFS{vol: vol, p: p}, nil
}

func (a *ntfsFS) Type() core.FSType { return core.FSNTFS }

func (a *ntfsFS) Capabilities() core.Capabilities {
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *ntfsFS) Close() error { return a.vol.Close() }

// Walk visits the directory tree from the root, then sweeps the MFT for
// records no directory reached: in-use records whose parent is gone, and
// deleted records whose index entry was overwritten. Each file's alternate
// data streams follow it as "path:stream" nodes.
func (a *ntfsFS) Walk(ctx context.Context, fn func(core.Node) error) error {
	root, err := a.vol.GetRootDirectory()
	if err != nil {
		return err
	}
	lim := a.p.Limits.WithDefaults()
	count, err := a.mftRecords()
	if err != nil {
		return err
	}
	reached := newBitset(count)
	var emitted int64
	emit := func(n core.Node) error {
		if emitted >= lim.MaxEntries {
			a.p.Report(core.Issue{Code: core.IssueEntryBudget, Path: n.Path, Detail: fmt.Sprintf("stopped after %d entries", emitted)})
			return fsutil.ErrEntryBudget
		}
		emitted++
		if err := fn(n); err != nil {
			return err
		}
		if n.Kind == core.KindFile && !n.IsDeleted {
			return a.emitStreams(n, fn)
		}
		return nil
	}

	w := &fsutil.TreeWalker{
		List:   a.list,
		Sink:   a.p.Sink,
		Limits: lim,
		Descend: func(n core.Node) bool {
			return n.Kind == core.KindDir && !n.IsDeleted && !n.Prov.Has(core.ProvStaleEntry)
		},
	}
	reached.set(root.EntryNumber())
	err = w.Walk(ctx, core.Node{ID: root.EntryNumber(), Seq: uint32(root.SequenceNumber()), Kind: core.KindDir}, func(n core.Node) error {
		if !n.Prov.Has(core.ProvStaleEntry) {
			reached.set(n.ID)
		}
		return emit(n)
	})
	if err != nil {
		return err
	}
	return a.sweep(ctx, count, reached, emit)
}

// list reads one directory index. Each entry is checked against the record it
// names: a live entry whose record has since been reused, or a deleted entry
// whose record now belongs to another file, is stale.
func (a *ntfsFS) list(dir core.Node) (kids []core.Node, err error) {
	err = safe.Call(safe.Site{Component: "parser/ntfs", Op: "readdir", Inode: dir.ID}, func() error {
		f, err := a.vol.Open(dir.ID)
		if err != nil {
			return err
		}
		entries, err := f.ReadDir()
		for _, e := range entries {
			if e.Name == "" || e.Name == "." || e.Name == ".." {
				continue
			}
			kids = append(kids, a.entryNode(e))
		}
		return err
	})
	return kids, err
}

func (a *ntfsFS) entryNode(e libntfs.DirEntry) core.Node {
	n := core.Node{ID: e.EntryNum, Seq: uint32(e.SequenceNum), Name: e.Name, Kind: core.KindFile, IsDeleted: e.Deleted}
	if e.IsDirectory {
		n.Kind = core.KindDir
	}
	if e.Deleted {
		n.Prov |= core.ProvDeleted
	}
	if e.EntryNum < firstUserRecord && strings.HasPrefix(e.Name, "$") {
		n.Prov |= core.ProvFSMetadata
	}
	if e.Deleted {
		n.Ref = indexRef{e: e}
	}
	rec, err := a.vol.GetMFTEntry(e.EntryNum)
	if err != nil {
		if e.Deleted {
			// A deleted index entry recovered from slack can name a record
			// that never existed.
			n.Prov |= core.ProvStaleEntry
		}
		return n
	}
	switch {
	case !e.Deleted && (!rec.IsInUse() || (e.SequenceNum != 0 && rec.SequenceNum != e.SequenceNum)):
		n.Prov |= core.ProvStaleEntry
	case e.Deleted && !deletedRecordMatches(rec, e.SequenceNum):
		n.Prov |= core.ProvStaleEntry | core.ProvReallocated
	}
	if names := linkNames(rec); names > 1 {
		n.Prov |= core.ProvHardLink
	}
	return n
}

// deletedRecordMatches reports whether rec is still the file a deleted entry
// with sequence seq named. NTFS bumps the sequence when it frees a record, so
// a free record at seq or seq+1 is that file; a record in use, or further on,
// was reused.
func deletedRecordMatches(rec *libntfs.MFTEntry, seq uint16) bool {
	if rec.IsInUse() {
		return false
	}
	return seq == 0 || rec.SequenceNum == seq || rec.SequenceNum == seq+1
}

// linkNames counts a record's names, ignoring the DOS 8.3 twin of a long name.
func linkNames(rec *libntfs.MFTEntry) int {
	names, _ := rec.FileNames()
	n := 0
	for _, fn := range names {
		if fn.Namespace != libntfs.NamespaceDOS {
			n++
		}
	}
	return n
}

// mftRecords is how many MFT records the walk accounts for: the count the
// $MFT run list implies, cut to what the volume could hold. A damaged run
// list can claim billions of records, and the reached set and the sweep are
// both sized by this count.
func (a *ntfsFS) mftRecords() (uint64, error) {
	count, err := a.vol.MFTEntryCount()
	if err != nil {
		return 0, err
	}
	if rs := uint64(a.vol.MFTRecordSize()); rs > 0 && a.p.Size > 0 {
		if most := uint64(a.p.Size) / rs; count > most {
			a.p.Report(core.Issue{Code: core.IssueOutOfBounds, Detail: fmt.Sprintf("$MFT claims %d records; the volume holds at most %d", count, most)})
			count = most
		}
	}
	return count, nil
}

// sweep emits the records the tree did not reach. It reads the records itself
// rather than through libntfs's WalkMFT, which takes its count from the run
// list and skips unreadable records without calling back, so a forged count
// could not be stopped.
func (a *ntfsFS) sweep(ctx context.Context, count uint64, reached bitset, emit func(core.Node) error) error {
	lim := a.p.Limits.WithDefaults()
	visit := func(num uint64, rec *libntfs.MFTEntry) error {
		if reached.has(num) || rec.BaseRecordRef != 0 {
			return nil
		}
		fname, err := rec.GetFileName()
		if err != nil || fname.Name == "" {
			return nil // not a file record, or one with nothing to name it by
		}
		deleted := !rec.IsInUse()
		prov := core.ProvRecovered
		if deleted {
			prov |= core.ProvDeleted
		}
		parent := orphanDir
		if pp, err := a.vol.PathFor(fname.ParentDirectory); err == nil && a.parentMatches(fname) {
			parent = fsutil.EscapePath(pp)
		} else {
			prov |= core.ProvOrphan
		}
		path, san := fsutil.Join(parent, fname.Name, lim.MaxName)
		if san {
			prov |= core.ProvNameSanitized
		}
		kind := core.KindFile
		if rec.IsDirectory() {
			kind = core.KindDir
		}
		if linkNames(rec) > 1 {
			prov |= core.ProvHardLink
		}
		return emit(core.Node{
			ID: num, Seq: uint32(rec.SequenceNum), Parent: fname.ParentDirectory,
			Name: fname.Name, Path: path, Kind: kind, IsDeleted: deleted, Prov: prov,
		})
	}
	for num := uint64(firstUserRecord); num < count; num++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := a.vol.GetMFTEntry(num)
		if err != nil {
			if errors.Is(err, libntfs.ErrVolumeClosed) {
				return err
			}
			continue // unallocated or unparsable: a sweep meets many
		}
		if err := visit(num, rec); err != nil {
			return err
		}
	}
	return nil
}

// parentMatches reports whether a record's parent directory is still the one
// its $FILE_NAME names, by sequence number.
func (a *ntfsFS) parentMatches(fname *libntfs.FileName) bool {
	parent, err := a.vol.GetMFTEntry(fname.ParentDirectory)
	return err == nil && parent.IsInUse() && parent.IsDirectory() &&
		(fname.ParentSeqNum == 0 || parent.SequenceNum == fname.ParentSeqNum)
}

// emitStreams follows a file node with one node per alternate data stream.
func (a *ntfsFS) emitStreams(n core.Node, fn func(core.Node) error) error {
	rec, err := a.vol.GetMFTEntry(n.ID)
	if err != nil {
		return nil
	}
	lim := a.p.Limits.WithDefaults()
	for _, attr := range rec.DataStreams() {
		name := attrName(attr)
		if name == "" {
			continue
		}
		esc, san := fsutil.EscapeName(name)
		if len(esc) > lim.MaxName {
			continue
		}
		s := n
		s.Name = n.Name + ":" + name
		s.Path = n.Path + ":" + esc
		s.Ref = streamRef{name: name}
		if san {
			s.Prov |= core.ProvNameSanitized
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

func attrName(attr *libntfs.Attribute) string {
	switch {
	case attr.Resident != nil:
		return attr.Resident.Name
	case attr.NonResident != nil:
		return attr.NonResident.Name
	}
	return ""
}

// dataAttr returns the $DATA attribute a node reads: its named stream, or the
// primary (unnamed) one.
func dataAttr(rec *libntfs.MFTEntry, n core.Node) *libntfs.Attribute {
	if s, ok := n.Ref.(streamRef); ok {
		for _, attr := range rec.DataStreams() {
			if attrName(attr) == s.name {
				return attr
			}
		}
		return nil
	}
	return rec.FindPrimaryDataAttribute()
}

func attrSize(attr *libntfs.Attribute) int64 {
	switch {
	case attr == nil:
		return 0
	case attr.Resident != nil:
		return int64(len(attr.Resident.Value))
	case attr.NonResident != nil:
		return int64(attr.NonResident.RealSize)
	}
	return 0
}

func (a *ntfsFS) Stat(n core.Node) (core.FileInfo, error) {
	if ir, ok := n.Ref.(indexRef); ok && n.Prov.Has(core.ProvStaleEntry) {
		// The record is gone or someone else's: report what the index kept.
		e := ir.e
		info := core.FileInfo{Kind: n.Kind, IsDeleted: true, Inode: n.ID, MACB: core.MACB{
			Modified: fsutil.TimePtr(e.ModifyTime),
			Accessed: fsutil.TimePtr(e.AccessTime),
			Changed:  fsutil.TimePtr(e.MFTChangeTime),
			Born:     fsutil.TimePtr(e.CreateTime),
		}}
		if n.Kind == core.KindFile {
			info.Size = int64(e.Size)
		}
		return info, nil
	}
	rec, err := a.vol.GetMFTEntry(n.ID)
	if err != nil {
		return core.FileInfo{}, err
	}
	info := core.FileInfo{Kind: n.Kind, IsDeleted: n.IsDeleted, Inode: n.ID, Links: linkNames(rec)}
	if n.Kind == core.KindFile {
		info.Size = attrSize(dataAttr(rec, n))
		if info.Size == 0 && dataAttr(rec, n) == nil {
			if fname, err := rec.GetFileName(); err == nil {
				info.Size = int64(fname.RealSize)
			}
		}
	}
	if si, err := rec.GetStandardInformation(); err == nil && si != nil {
		info.MACB = core.MACB{
			Modified: fsutil.TimePtr(si.ModifyTime),
			Accessed: fsutil.TimePtr(si.AccessTime),
			Changed:  fsutil.TimePtr(si.MFTChangeTime),
			Born:     fsutil.TimePtr(si.CreateTime),
		}
	}
	return info, nil
}

// Content locates a node's data:
//   - resident data is inline (fixups already applied by the record parser);
//   - compressed data is decoded, read through Open;
//   - a WOF-compressed file keeps its content in a reparse point DUES cannot
//     decode yet, and is unavailable;
//   - everything else is its runs, with bytes past the initialized size read
//     as zeros. Encrypted data is located like any other, flagged encrypted:
//     the ciphertext is what the volume holds.
//
// A deleted file's runs come from its freed record. They are declared, not
// certain, and flagged reallocated when the clusters now belong to another
// file. A stale entry, whose record was reused, has no content of its own.
func (a *ntfsFS) Content(n core.Node) (core.Content, error) {
	if n.Prov.Has(core.ProvStaleEntry) {
		c := core.Content{Kind: core.ContentUnavailable, Prov: core.ProvReallocated, Confidence: core.ConfUnknown}
		if ir, ok := n.Ref.(indexRef); ok && ir.e.Size > 0 {
			c.Size = int64(ir.e.Size)
			c.Prov |= core.ProvTruncated
		}
		return c, nil
	}
	rec, err := a.vol.GetMFTEntry(n.ID)
	if err != nil {
		return core.Content{}, err
	}
	if n.IsDeleted && rec.IsInUse() {
		return core.Content{Kind: core.ContentUnavailable, Prov: core.ProvReallocated | core.ProvTruncated, Confidence: core.ConfUnknown}, nil
	}
	attr := dataAttr(rec, n)
	size := attrSize(attr)
	if attr == nil || size == 0 {
		return core.Content{}, nil
	}
	lim := a.p.Limits.WithDefaults()

	var c core.Content
	switch {
	case attr.Resident != nil:
		if size > int64(lim.MaxInline) {
			return core.Content{}, fmt.Errorf("ntfs: resident value of %d bytes", size)
		}
		c = core.InlineContent(size, attr.Resident.Value)
	case attr.IsCompressed():
		c = core.Content{Kind: core.ContentDecoded, Size: size, Prov: core.ProvCompressed}
		if n.IsDeleted {
			c.Confidence = core.ConfDeclared
		}
	default:
		if !n.IsDeleted {
			if wof, err := a.isWOF(n); err == nil && wof {
				return core.Content{Kind: core.ContentUnavailable, Size: size, Prov: core.ProvCompressed | core.ProvTruncated, Confidence: core.ConfUnknown,
					Issues: []core.Issue{{Code: core.IssueUnsupported, Path: n.Path, Inode: n.ID, Detail: "WOF-compressed content"}}}, nil
			}
		}
		frags, err := a.vol.AttributeFragments(attr)
		if err != nil {
			return core.Content{}, err
		}
		if len(frags) > lim.MaxExtents {
			frags = frags[:lim.MaxExtents]
		}
		ext := make([]core.Extent, 0, len(frags))
		for _, f := range frags {
			e := core.Extent{FileOffset: f.FileOffset, Length: f.Length, Kind: core.ExtentSparse}
			if !f.Sparse {
				if f.StartOffset < 0 || f.StartOffset > (1<<63-1)-a.p.Base {
					return core.Content{}, fmt.Errorf("%w: run at %d", core.ErrBadExtent, f.StartOffset)
				}
				e.Kind, e.ImageOffset = core.ExtentData, a.p.Base+f.StartOffset
			}
			ext = append(ext, e)
		}
		c, err = core.ExtentContent(size, ext)
		if err != nil {
			return core.Content{}, err
		}
		c.MarkUnwritten(int64(attr.NonResident.InitializedSize))
		if n.IsDeleted {
			c.Confidence = core.ConfDeclared
			if a.reallocated(c) {
				c.Prov |= core.ProvReallocated
			}
		}
	}
	if attr.IsEncrypted() {
		c.Prov |= core.ProvEncrypted
	}
	if n.Prov.Has(core.ProvHardLink) {
		c.Prov |= core.ProvHardLink
	}
	return c, nil
}

func (a *ntfsFS) isWOF(n core.Node) (bool, error) {
	f, err := a.vol.Open(n.ID)
	if err != nil {
		return false, err
	}
	rp, err := f.ReparsePoint()
	if err != nil || rp == nil {
		return false, err
	}
	return rp.ReparseType == libntfs.ReparsePointWOF, nil
}

// reallocated reports whether any cluster of c's data extents is allocated
// now, according to $Bitmap. An unreadable bitmap answers no and is reported
// once.
func (a *ntfsFS) reallocated(c core.Content) bool {
	a.bitmapOnce.Do(func() {
		f, err := a.vol.Open(libntfs.MFTEntryBitmap)
		if err != nil {
			a.bitmapErr = err
			return
		}
		a.bitmap, a.bitmapErr = f.ReadAll()
		if a.bitmapErr != nil {
			a.p.Report(core.Issue{Code: core.IssueLibrary, Detail: "$Bitmap: " + a.bitmapErr.Error()})
		}
	})
	if a.bitmapErr != nil {
		return false
	}
	cs := int64(a.vol.BytesPerCluster())
	for _, e := range c.Extents {
		if e.Kind != core.ExtentData {
			continue
		}
		first := (e.ImageOffset - a.p.Base) / cs
		last := (e.ImageOffset - a.p.Base + e.Length - 1) / cs
		for cl := first; cl <= last; cl++ {
			if cl/8 >= int64(len(a.bitmap)) || a.bitmap[cl/8]&(1<<(cl%8)) != 0 {
				return true
			}
		}
	}
	return false
}

// Open streams a node's logical content. Compressed streams are decoded by
// DUES (see lznt1.go); everything else is read through libntfs, with bytes
// past the initialized size cut to zeros (libntfs reads them raw). A deleted
// file, which libntfs will not open, is served from its located content.
func (a *ntfsFS) Open(n core.Node) (io.ReadCloser, error) {
	c, err := a.Content(n)
	if err != nil {
		return nil, err
	}
	if c.Kind == core.ContentDecoded {
		rec, err := a.vol.GetMFTEntry(n.ID)
		if err != nil {
			return nil, err
		}
		attr := dataAttr(rec, n)
		if attr == nil || attr.NonResident == nil {
			return nil, fmt.Errorf("ntfs: record %d has no compressed stream", n.ID)
		}
		cs, err := newCompressedStream(a.p.Reader, a.vol.BytesPerCluster(), attr.NonResident)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(io.NewSectionReader(cs, 0, c.Size)), nil
	}
	if n.IsDeleted {
		return fsutil.OpenContent(a.p.Reader, a.p.Base, c)
	}
	f, err := a.vol.Open(n.ID)
	if err != nil {
		return nil, err
	}
	if s, ok := n.Ref.(streamRef); ok {
		if f, err = f.OpenStream(s.name); err != nil {
			return nil, err
		}
	}
	rec, err := a.vol.GetMFTEntry(n.ID)
	if err != nil {
		return nil, err
	}
	attr := dataAttr(rec, n)
	size := attrSize(attr)
	var r io.Reader = io.NewSectionReader(f, 0, size)
	if attr != nil && attr.NonResident != nil && !attr.IsCompressed() {
		if init := int64(attr.NonResident.InitializedSize); init < size {
			r = io.MultiReader(io.NewSectionReader(f, 0, init), io.LimitReader(zeros{}, size-init))
		}
	}
	return io.NopCloser(r), nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Streams lists a file's alternate data streams.
func (a *ntfsFS) Streams(n core.Node) ([]core.Stream, error) {
	if _, ok := n.Ref.(streamRef); ok || n.Prov.Has(core.ProvStaleEntry) {
		return nil, nil
	}
	rec, err := a.vol.GetMFTEntry(n.ID)
	if err != nil {
		return nil, err
	}
	var out []core.Stream
	for _, attr := range rec.DataStreams() {
		if name := attrName(attr); name != "" {
			out = append(out, core.Stream{Name: name, Kind: "ads", Size: attrSize(attr)})
		}
	}
	return out, nil
}

// bitset marks MFT record numbers.
type bitset []uint64

func newBitset(n uint64) bitset { return make(bitset, (n+63)/64) }

func (b bitset) set(i uint64) {
	if i/64 < uint64(len(b)) {
		b[i/64] |= 1 << (i % 64)
	}
}

func (b bitset) has(i uint64) bool {
	return i/64 < uint64(len(b)) && b[i/64]&(1<<(i%64)) != 0
}
