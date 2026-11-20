// Package hfs adapts libhfs (classic HFS, HFS+ and HFSX, including an HFS
// wrapper around an embedded HFS+ volume) to core.Filesystem. HFS is
// CNID-based, so core.Node.ID is the catalog node id.
package hfs

import (
	"context"
	"errors"
	"fmt"
	"io"

	libhfs "github.com/aoiflux/libhfs"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

type hfsFS struct {
	vol *libhfs.Volume
	p   core.OpenParams
}

var _ core.Filesystem = (*hfsFS)(nil)

// deletedRef is the Ref of a node recovered by the deleted-record sweep: its
// catalog record no longer resolves through the live tree.
type deletedRef struct{ rec libhfs.DeletedRecord }

// Open mounts the volume on p.Reader. Byte ranges come from libhfs relative
// to the partition (allocation-block start and any embedded-volume offset
// included) and are made image-absolute by adding p.Base.
func Open(p core.OpenParams) (core.Filesystem, error) {
	// The zero Config caps on-disk-sized buffers at libhfs's 1 GiB default.
	vol, err := libhfs.OpenWithConfig(p.Reader, libhfs.Config{})
	if err != nil {
		return nil, err
	}
	return &hfsFS{vol: vol, p: p}, nil
}

func (a *hfsFS) Type() core.FSType { return core.FSHFS }

func (a *hfsFS) Capabilities() core.Capabilities {
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *hfsFS) Close() error {
	a.drainAnomalies()
	return a.vol.Close()
}

// drainAnomalies reports what libhfs tolerated while reading the volume.
func (a *hfsFS) drainAnomalies() {
	for _, an := range a.vol.Anomalies() {
		a.p.Report(core.Issue{Code: core.IssueLibrary, Detail: fmt.Sprintf("%s at %d: %s", an.Op, an.Offset, an.Detail)})
	}
}

// Walk visits the live catalog from the root, then the deleted records the
// sweep recovers. A deleted record keeps its folder or file kind, and sits
// under its original parent when that parent is still live.
func (a *hfsFS) Walk(ctx context.Context, fn func(core.Node) error) error {
	root, err := a.vol.GetRootDirectory()
	if err != nil {
		return err
	}
	w := &fsutil.TreeWalker{List: a.list, Sink: a.p.Sink, Limits: a.p.Limits}
	if err := w.Walk(ctx, core.Node{ID: uint64(root.CNID), Kind: core.KindDir}, fn); err != nil {
		return err
	}
	return a.walkDeleted(ctx, fn)
}

func (a *hfsFS) list(dir core.Node) ([]core.Node, error) {
	entries, err := a.vol.ReadDirCNID(uint32(dir.ID))
	out := make([]core.Node, 0, len(entries))
	for _, e := range entries {
		kind := core.KindFile
		if e.IsDirectory {
			kind = core.KindDir
		}
		out = append(out, core.Node{ID: uint64(e.CNID), Name: e.Name, Kind: kind})
	}
	return out, err
}

func (a *hfsFS) walkDeleted(ctx context.Context, fn func(core.Node) error) error {
	lim := a.p.Limits.WithDefaults()
	err := a.vol.WalkDeletedContext(ctx, libhfs.DefaultRecoveryOptions(), func(d libhfs.DeletedRecord) error {
		name := d.Record.Name
		prov := core.ProvDeleted | core.ProvRecovered
		if name == "" {
			name = fmt.Sprintf("cnid-%d", d.Record.CNID)
			prov |= core.ProvSynthName
		}
		parent := "/$deleted"
		if pp, err := a.vol.PathForCNID(d.Record.ParentCNID); err == nil {
			parent = pp
		}
		path, san := fsutil.Join(parent, name, lim.MaxName)
		if san {
			prov |= core.ProvNameSanitized
		}
		if d.Overwritten {
			prov |= core.ProvReallocated
		}
		kind := core.KindFile
		if d.Record.Type == libhfs.CatalogRecordFolder {
			kind = core.KindDir
		}
		return fn(core.Node{
			ID:        uint64(d.Record.CNID),
			Parent:    uint64(d.Record.ParentCNID),
			Name:      name,
			Path:      path,
			Kind:      kind,
			IsDeleted: true,
			Prov:      prov,
			Ref:       deletedRef{rec: d},
		})
	})
	if errors.Is(err, ctx.Err()) && ctx.Err() != nil {
		return err
	}
	if err != nil {
		// Recovery is a supplement to the live tree: its failure is an issue.
		a.p.Report(core.Issue{Code: core.IssueLibrary, Detail: "deleted-record sweep: " + err.Error()})
	}
	return nil
}

func (a *hfsFS) Stat(n core.Node) (core.FileInfo, error) {
	if d, ok := n.Ref.(deletedRef); ok {
		return statRecord(n, d.rec.Record), nil
	}
	rec, err := a.vol.OpenCNID(uint32(n.ID))
	if err != nil {
		return core.FileInfo{}, err
	}
	info := statRecord(n, rec)
	if t, err := a.vol.GetTimes(uint32(n.ID)); err == nil {
		info.MACB = macb(t)
	}
	if rec.IsSymlink() {
		info.Kind = core.KindSymlink
		if target, err := a.vol.ReadLink(uint32(n.ID)); err == nil {
			info.Target = target
		}
	}
	return info, nil
}

func statRecord(n core.Node, rec libhfs.CatalogRecord) core.FileInfo {
	info := core.FileInfo{Kind: n.Kind, IsDeleted: n.IsDeleted, Inode: n.ID, MACB: macb(rec.Times)}
	if n.Kind == core.KindFile {
		info.Size = int64(rec.DataFork.LogicalSize)
	}
	if rec.IsHardLink() {
		info.Links = int(rec.LinkCount)
	}
	return info
}

func macb(t libhfs.CatalogTimes) core.MACB {
	return core.MACB{
		Modified: fsutil.TimePtr(t.ContentModified),
		Accessed: fsutil.TimePtr(t.Accessed),
		Changed:  fsutil.TimePtr(t.AttrModified),
		Born:     fsutil.TimePtr(t.Created),
	}
}

// Content returns the data fork's byte ranges. A decmpfs-compressed file has
// no ranges to read; its content is decoded through Open. A deleted record
// carries only the extents in its own catalog record (the overflow tree
// belongs to live files), so its content is declared, not certain.
func (a *hfsFS) Content(n core.Node) (core.Content, error) {
	if d, ok := n.Ref.(deletedRef); ok {
		rec := d.rec.Record
		size := int64(rec.DataFork.LogicalSize)
		ranges, err := a.vol.ExtentRanges(rec.DataFork.Extents[:], size)
		if err != nil {
			return core.Content{}, err
		}
		c, err := a.rangesContent(size, ranges)
		if err != nil {
			return core.Content{}, err
		}
		c.Confidence = core.ConfDeclared
		if d.rec.Overwritten {
			c.Prov |= core.ProvReallocated
		}
		return c, nil
	}

	rec, err := a.vol.OpenCNID(uint32(n.ID))
	if err != nil {
		return core.Content{}, err
	}
	size := int64(rec.DataFork.LogicalSize)
	var prov core.Prov
	if rec.IsHardLink() {
		prov |= core.ProvHardLink
	}
	if rec.Compressed {
		return core.Content{Kind: core.ContentDecoded, Size: size, Prov: prov | core.ProvCompressed}, nil
	}
	ranges, err := a.vol.DataForkRanges(uint32(n.ID))
	if err != nil {
		return core.Content{}, err
	}
	c, err := a.rangesContent(size, ranges)
	c.Prov |= prov
	return c, err
}

func (a *hfsFS) rangesContent(size int64, ranges []libhfs.ByteRange) (core.Content, error) {
	ext := make([]core.Extent, 0, len(ranges))
	for _, r := range ranges {
		if r.Length <= 0 {
			continue
		}
		img, ok := addOffset(a.p.Base, r.DiskOffset)
		if !ok {
			return core.Content{}, fmt.Errorf("%w: disk offset %d", core.ErrBadExtent, r.DiskOffset)
		}
		ext = append(ext, core.Extent{FileOffset: r.ForkOffset, Length: r.Length, ImageOffset: img, Kind: core.ExtentData})
	}
	return core.ExtentContent(size, ext)
}

func addOffset(base, off int64) (int64, bool) {
	if off < 0 || off > (1<<63-1)-base {
		return 0, false
	}
	return base + off, true
}

func (a *hfsFS) Open(n core.Node) (io.ReadCloser, error) {
	if d, ok := n.Ref.(deletedRef); ok {
		f, err := a.vol.OpenDeleted(d.rec)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(f), nil
	}
	f, err := a.vol.OpenFileByCNID(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(f), nil
}

// Streams returns the resource fork (when present) and extended attributes.
func (a *hfsFS) Streams(n core.Node) ([]core.Stream, error) {
	if _, ok := n.Ref.(deletedRef); ok {
		return nil, nil
	}
	var out []core.Stream
	if rf, err := a.vol.OpenResourceForkByCNID(uint32(n.ID)); err == nil && rf.Size() > 0 {
		out = append(out, core.Stream{Name: "rsrc", Kind: "resource_fork", Size: rf.Size()})
	}
	if xs, err := a.vol.ListXAttrs(uint32(n.ID)); err == nil {
		for _, x := range xs {
			out = append(out, core.Stream{Name: x.Name, Kind: "xattr", Size: int64(x.Size)})
		}
	}
	return out, nil
}
