// Package ext adapts libext (ext2/3/4) to core.Filesystem. ext is inode-based,
// so core.Node.ID is the inode number and metadata is resolved directly by
// number (no per-instance cache).
package ext

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aoiflux/libext"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

// direntSymlink is the ext directory-entry file type of a symbolic link.
const direntSymlink = 7

type extFS struct {
	fs        *libext.FS
	p         core.OpenParams
	bs        int64
	inlineMax uint64 // the most inline data an inode of this volume holds
	reported  int    // library warnings already passed to the sink
}

var _ core.Filesystem = (*extFS)(nil)

// deletedRef is the Ref of an inode found by the deleted scan, carrying the
// block map that survived with it.
type deletedRef struct{ d libext.DeletedEntry }

// Open mounts an ext filesystem from p.Reader. libext reports offsets
// relative to the partition, and the adapter adds p.Base once. Permissive
// mode makes unknown feature flags and damaged directory blocks warnings, not
// failures: evidence is read for what it holds.
func Open(p core.OpenParams) (core.Filesystem, error) {
	lim := p.Limits.WithDefaults()
	efs, err := libext.OpenWithOptions(p.Reader, libext.Options{
		ImageSize:  uint64(p.Size),
		Permissive: true,
		MaxExtents: lim.MaxExtents,
	})
	if err != nil {
		return nil, err
	}
	sb := efs.Superblock()
	return &extFS{fs: efs, p: p, bs: int64(sb.BlockSize), inlineMax: inlineDataInBlock + uint64(sb.InodeSize)}, nil
}

// inlineDataInBlock is how much inline data the block-pointer area holds; the
// rest is an extended attribute inside the inode record.
const inlineDataInBlock = 60

// checkInline refuses an inline inode that claims more data than an inode
// holds. libext v0.4.1 allocates the claimed size before it reads inline data
// (contents, directory listings, link targets), so one forged size field ran
// the process out of memory, which recover cannot contain (PAR-35).
func (a *extFS) checkInline(ino libext.Inode) error {
	if ino.HasInline && ino.Size > a.inlineMax {
		return fmt.Errorf("%w: inode %d claims %d bytes of inline data, more than an inode holds (%d)",
			core.ErrBadExtent, ino.Number, ino.Size, a.inlineMax)
	}
	return nil
}

// inlineData returns an inode's inline contents, after checkInline.
func (a *extFS) inlineData(inum uint32) ([]byte, bool, error) {
	ino, err := a.fs.ReadInode(inum)
	if err != nil {
		return nil, false, err
	}
	if !ino.HasInline {
		return nil, false, nil
	}
	if err := a.checkInline(ino); err != nil {
		return nil, true, err
	}
	return a.fs.InlineData(inum)
}

func (a *extFS) Type() core.FSType { return core.FSExt }

func (a *extFS) Capabilities() core.Capabilities {
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *extFS) Close() error {
	a.drainWarnings()
	return a.fs.Close()
}

// drainWarnings passes on the warnings libext accumulated since the last call.
func (a *extFS) drainWarnings() {
	ws := a.fs.Warnings()
	for _, w := range ws[min(a.reported, len(ws)):] {
		a.p.Report(core.Issue{Code: core.IssueLibrary, Detail: w.String()})
	}
	a.reported = len(ws)
}

// Walk visits the live tree from the root, one directory at a time (libext's
// own WalkDir abandons the whole walk at the first bad directory), then the
// inodes the deleted scan recovers.
func (a *extFS) Walk(ctx context.Context, fn func(core.Node) error) error {
	root, err := a.fs.GetRootDirectory()
	if err != nil {
		return err
	}
	w := &fsutil.TreeWalker{List: a.list, Sink: a.p.Sink, Limits: a.p.Limits}
	err = w.Walk(ctx, core.Node{ID: uint64(root.InodeNumber()), Kind: core.KindDir}, fn)
	a.drainWarnings()
	if err != nil {
		return err
	}
	err = a.walkDeleted(ctx, fn)
	a.drainWarnings()
	return err
}

func (a *extFS) list(dir core.Node) ([]core.Node, error) {
	ino, err := a.fs.ReadInode(uint32(dir.ID))
	if err != nil {
		return nil, err
	}
	if err := a.checkInline(ino); err != nil {
		return nil, err
	}
	entries, err := a.fs.ListDir(uint32(dir.ID))
	out := make([]core.Node, 0, len(entries))
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." || e.Inode == 0 {
			continue
		}
		n := core.Node{ID: uint64(e.Inode), Name: e.Name, Kind: core.KindFile}
		switch {
		case e.IsDirectory:
			n.Kind = core.KindDir
		case e.FileType == direntSymlink:
			n.Kind = core.KindSymlink
		}
		out = append(out, n)
	}
	return out, err
}

func (a *extFS) walkDeleted(ctx context.Context, fn func(core.Node) error) error {
	lim := a.p.Limits.WithDefaults()
	err := a.fs.ScanDeletedContext(ctx, libext.DeletedScanOptions{}, func(d libext.DeletedEntry) error {
		prov := core.ProvDeleted | core.ProvRecovered
		if d.Source == libext.DeletedSourceDirSlack && d.Allocated {
			// A name left in a directory's slack for an inode that is in use
			// again: a rename's old name, or a reused inode. Metadata only.
			prov |= core.ProvStaleEntry
		}
		name := d.Name
		if name == "" {
			name = fmt.Sprintf("inode-%d", d.Inode)
			prov |= core.ProvSynthName
		}
		var path string
		var san bool
		if d.Path != "" {
			path = fsutil.EscapePath(d.Path)
		} else {
			path, san = fsutil.Join("/$deleted", name, lim.MaxName)
			prov |= core.ProvOrphan
		}
		if san {
			prov |= core.ProvNameSanitized
		}
		return fn(core.Node{
			ID: uint64(d.Inode), Parent: uint64(d.ParentInode), Name: name, Path: path,
			Kind: modeKind(d.Mode), IsDeleted: true, Prov: prov, Ref: deletedRef{d: d},
		})
	})
	if err != nil && ctx.Err() == nil {
		// Recovery supplements the live tree; its failure is an issue.
		a.p.Report(core.Issue{Code: core.IssueLibrary, Detail: "deleted scan: " + err.Error()})
		return nil
	}
	return err
}

func (a *extFS) Stat(n core.Node) (core.FileInfo, error) {
	if d, ok := n.Ref.(deletedRef); ok {
		return core.FileInfo{
			Size: int64(d.d.Size), Kind: n.Kind, IsDeleted: true, Inode: n.ID,
			MACB: macb(d.d.Times),
		}, nil
	}
	ino, err := a.fs.ReadInode(uint32(n.ID))
	if err != nil {
		return core.FileInfo{}, err
	}
	info := core.FileInfo{
		Size:      int64(ino.Size),
		Kind:      n.Kind,
		IsDeleted: n.IsDeleted || !ino.Dtime.IsZero(),
		Inode:     n.ID,
		MACB:      macb(ino.Timestamps()),
		Links:     int(ino.LinksCount),
	}
	switch {
	case ino.IsSymlink:
		info.Kind = core.KindSymlink
		if a.checkInline(ino) != nil {
			break
		}
		if f, err := a.fs.Open(uint32(n.ID)); err == nil {
			if target, err := f.ReadLink(); err == nil {
				info.Target = target
			}
		}
	case ino.IsDirectory:
		info.Kind = core.KindDir
	}
	if runs, err := a.fs.DataRuns(uint32(n.ID)); err == nil {
		data := 0
		for _, r := range runs {
			if !r.Sparse && !r.Unwritten {
				data++
			}
		}
		info.IsFragmented = data > 1
	}
	return info, nil
}

func macb(t libext.Timestamps) core.MACB {
	return core.MACB{
		Modified: fsutil.TimePtr(t.Mtime),
		Accessed: fsutil.TimePtr(t.Atime),
		Changed:  fsutil.TimePtr(t.Ctime),
		Born:     fsutil.TimePtr(t.Crtime),
	}
}

// Content locates an inode's data: inline data (ext4 inline_data) is carried
// in the record; otherwise its runs, with holes as sparse extents and
// preallocated, never-written extents as unwritten (they read as zeros,
// whatever the blocks hold, PAR-5). A deleted inode's content is the block
// map that survived with it, flagged reallocated when some of those blocks
// now belong to another file.
func (a *extFS) Content(n core.Node) (core.Content, error) {
	if d, ok := n.Ref.(deletedRef); ok {
		return a.deletedContent(d.d)
	}
	ino, err := a.fs.ReadInode(uint32(n.ID))
	if err != nil {
		return core.Content{}, err
	}
	size := int64(ino.Size)
	var c core.Content
	if ino.HasInline {
		if err := a.checkInline(ino); err != nil {
			return core.Content{}, err
		}
		data, _, err := a.fs.InlineData(uint32(n.ID))
		if err != nil && len(data) == 0 {
			return core.Content{}, err
		}
		c = core.InlineContent(size, data)
	} else {
		runs, err := a.fs.DataRuns(uint32(n.ID))
		if err != nil {
			return core.Content{}, err
		}
		ext := make([]core.Extent, 0, len(runs))
		for _, r := range runs {
			e := core.Extent{FileOffset: r.FileOffset, Length: r.Length}
			switch {
			case r.Sparse:
				e.Kind = core.ExtentSparse
			case r.Unwritten:
				e.Kind = core.ExtentUnwritten
			default:
				img, ok := addOffset(a.p.Base, r.DiskOffset)
				if !ok {
					return core.Content{}, fmt.Errorf("%w: disk offset %d", core.ErrBadExtent, r.DiskOffset)
				}
				e.Kind, e.ImageOffset = core.ExtentData, img
			}
			ext = append(ext, e)
		}
		if c, err = core.ExtentContent(size, ext); err != nil {
			return core.Content{}, err
		}
	}
	if ino.LinksCount > 1 && !ino.IsDirectory {
		c.Prov |= core.ProvHardLink
	}
	return c, nil
}

func (a *extFS) deletedContent(d libext.DeletedEntry) (core.Content, error) {
	size := int64(d.Size)
	if d.Recoverable == libext.RecoveryNone {
		c := core.Content{Kind: core.ContentUnavailable, Size: size, Confidence: core.ConfUnknown}
		if size > 0 {
			c.Prov |= core.ProvTruncated
		}
		return c, nil
	}
	var ext []core.Extent
	for _, x := range d.Extents {
		if x.Inline() {
			if data, ok, err := a.inlineData(d.Inode); ok && err == nil {
				c := core.InlineContent(size, data)
				c.Confidence = core.ConfDeclared
				return c, nil
			}
			continue
		}
		if x.Sparse() {
			continue // gaps become holes when normalised
		}
		if x.LogicalBlock > uint64(1<<62)/uint64(a.bs) || x.PhysicalBlock > uint64(1<<62)/uint64(a.bs) || x.Blocks > uint64(1<<62)/uint64(a.bs) {
			return core.Content{}, fmt.Errorf("%w: extent %+v", core.ErrBadExtent, x)
		}
		e := core.Extent{FileOffset: int64(x.LogicalBlock) * a.bs, Length: int64(x.Blocks) * a.bs, Kind: core.ExtentUnwritten}
		if !x.Unwritten() {
			img, ok := addOffset(a.p.Base, int64(x.PhysicalBlock)*a.bs)
			if !ok {
				return core.Content{}, fmt.Errorf("%w: extent %+v", core.ErrBadExtent, x)
			}
			e.Kind, e.ImageOffset = core.ExtentData, img
		}
		ext = append(ext, e)
	}
	c, err := core.ExtentContent(size, ext)
	if err != nil {
		return core.Content{}, err
	}
	c.Confidence = core.ConfDeclared
	if d.Recoverable == libext.RecoveryPartial {
		c.Prov |= core.ProvReallocated
	}
	return c, nil
}

func addOffset(base, off int64) (int64, bool) {
	if off < 0 || off > (1<<63-1)-base {
		return 0, false
	}
	return base + off, true
}

// Open streams a regular file through libext's reader, an independent path
// from Content, except where that reader is wrong: libext reads inline data
// as zeros, so inline content is served from InlineData. A deleted inode is
// served from its located content.
func (a *extFS) Open(n core.Node) (io.ReadCloser, error) {
	if _, ok := n.Ref.(deletedRef); ok {
		c, err := a.Content(n)
		if err != nil {
			return nil, err
		}
		return fsutil.OpenContent(a.p.Reader, a.p.Base, c)
	}
	ino, err := a.fs.ReadInode(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	if ino.HasInline {
		if err := a.checkInline(ino); err != nil {
			return nil, err
		}
		data, _, err := a.fs.InlineData(uint32(n.ID))
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	f, err := a.fs.Open(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.NewSectionReader(f, 0, f.Size())), nil
}

func (a *extFS) Streams(n core.Node) ([]core.Stream, error) {
	if _, ok := n.Ref.(deletedRef); ok {
		return nil, nil
	}
	xl, err := a.fs.GetXAttrs(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	names := xl.ListXAttrNames()
	out := make([]core.Stream, 0, len(names))
	for _, name := range names {
		out = append(out, core.Stream{Name: name, Kind: "xattr", Size: int64(len(xl.GetXAttrValue(name)))})
	}
	return out, nil
}

func modeKind(mode uint16) core.NodeKind {
	switch mode & 0xF000 {
	case 0x4000: // S_IFDIR
		return core.KindDir
	case 0xA000: // S_IFLNK
		return core.KindSymlink
	case 0x8000, 0: // S_IFREG, or unknown
		return core.KindFile
	}
	return core.KindOther
}
