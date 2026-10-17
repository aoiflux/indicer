// Package xfs adapts libxfs to core.Filesystem: MACB and birth times, data
// runs with their file offsets, holes and unwritten extents, inline (local)
// data, deleted and carved directory records, and extended attributes.
package xfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aoiflux/libxfs"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

// directory-entry file types (XFS ftype).
const (
	ftypeDir     = 2
	ftypeSymlink = 7
)

type xfsFS struct {
	vol *libxfs.Volume
	p   core.OpenParams
}

var _ core.Filesystem = (*xfsFS)(nil)

// Open mounts an XFS volume from p.Reader with no base offset, so libxfs
// reports partition-relative offsets and p.Base is added once, here.
func Open(p core.OpenParams) (core.Filesystem, error) {
	vol, err := libxfs.Open(p.Reader)
	if err != nil {
		return nil, err
	}
	return &xfsFS{vol: vol, p: p}, nil
}

func (a *xfsFS) Type() core.FSType { return core.FSXFS }

func (a *xfsFS) Capabilities() core.Capabilities {
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *xfsFS) Close() error { return a.vol.Close() }

// Walk lists every directory record reachable from the root: active entries,
// plus deleted and carved records recovered from directory free space. Only
// active directories are entered; a carved inode number may be bogus.
func (a *xfsFS) Walk(ctx context.Context, fn func(core.Node) error) error {
	root, err := a.vol.ResolveInodeByPath("/")
	if err != nil {
		return err
	}
	w := &fsutil.TreeWalker{List: a.list, Sink: a.p.Sink, Limits: a.p.Limits}
	return w.Walk(ctx, core.Node{ID: root, Kind: core.KindDir}, fn)
}

func (a *xfsFS) list(dir core.Node) ([]core.Node, error) {
	listing, err := a.vol.ScanDirectoryRecordsWithOptions(dir.ID, libxfs.DirectoryScanOptions{IncludeDeleted: true, BestEffort: true})
	for _, an := range listing.Anomalies {
		a.p.Report(core.Issue{Code: core.IssueLibrary, Inode: dir.ID, Detail: an.Code + ": " + an.Message})
	}
	if listing.Truncated && err == nil {
		err = errors.New("directory listing truncated at the scan limit")
	}
	out := make([]core.Node, 0, len(listing.Records))
	for _, r := range listing.Records {
		if r.Name == "" || r.Name == "." || r.Name == ".." || r.Kind == libxfs.RecordKindFreeSlot {
			continue
		}
		n := core.Node{ID: r.InodeNumber, Name: r.Name, Kind: core.KindFile}
		if r.IsDeleted || r.IsCarved {
			n.IsDeleted = true
			n.Prov |= core.ProvDeleted
			if r.IsCarved {
				n.Prov |= core.ProvRecovered
			}
		}
		switch {
		case r.FileType == ftypeDir:
			n.Kind = core.KindDir
		case r.FileType == ftypeSymlink:
			n.Kind = core.KindSymlink
		case r.FileType == 0 && !n.IsDeleted:
			// No ftype feature: ask the inode.
			if ino, err := a.vol.OpenInode(r.InodeNumber); err == nil && ino.IsDirectory() {
				n.Kind = core.KindDir
			}
		}
		out = append(out, n)
	}
	return out, err
}

// inodeOf opens a node's inode. A deleted record whose inode is free, or has
// been given to another file, has none of its own: such records are
// metadata only.
func (a *xfsFS) inodeOf(n core.Node) (*libxfs.Inode, bool, error) {
	if n.IsDeleted {
		return nil, false, nil
	}
	ino, err := a.vol.OpenInode(n.ID)
	if err != nil {
		return nil, false, err
	}
	return ino, true, nil
}

func (a *xfsFS) Stat(n core.Node) (core.FileInfo, error) {
	ino, ok, err := a.inodeOf(n)
	if err != nil {
		return core.FileInfo{}, err
	}
	info := core.FileInfo{Kind: n.Kind, IsDeleted: n.IsDeleted, Inode: n.ID}
	if !ok {
		return info, nil
	}
	info.Size = int64(ino.Size)
	info.Links = int(ino.NumberOfLinks)
	info.IsFragmented = ino.DataExtentCount > 1
	info.MACB = core.MACB{
		Modified: fsutil.TimePtr(ino.ModificationTime()),
		Accessed: fsutil.TimePtr(ino.AccessTime()),
		Changed:  fsutil.TimePtr(ino.InodeChangeTime()),
		Born:     fsutil.TimePtr(ino.CreationTime()),
	}
	switch {
	case ino.IsDirectory():
		info.Kind = core.KindDir
	case ino.FileMode&0xf000 == libxfs.FileTypeSymbolicLink:
		info.Kind = core.KindSymlink
		info.Target = a.linkTarget(n.ID, ino)
	}
	return info, nil
}

// maxLinkTarget bounds a symlink target read; Linux caps them at 4 KiB.
const maxLinkTarget = 4096

func (a *xfsFS) linkTarget(id uint64, ino *libxfs.Inode) string {
	if ino.ForkType == libxfs.ForkTypeInlineData {
		return string(ino.InlineData)
	}
	if ino.Size > maxLinkTarget {
		return ""
	}
	buf := make([]byte, ino.Size)
	n, _ := a.vol.ReadInodeData(id, buf, 0)
	return string(buf[:n])
}

// Content locates an inode's data. Local-format inodes hold it inline.
// Otherwise the data runs are used with their own file offsets: holes are
// sparse, preallocated extents unwritten, and the last block's slack past the
// size is cut off (PAR-4). libxfs drops a run whose block it cannot resolve
// instead of reporting it, which shows as a gap in a fork that is otherwise
// gap-free; the content stops there rather than reading the gap as a hole.
func (a *xfsFS) Content(n core.Node) (core.Content, error) {
	ino, ok, err := a.inodeOf(n)
	if err != nil {
		return core.Content{}, err
	}
	if !ok {
		return core.Content{Kind: core.ContentUnavailable, Confidence: core.ConfUnknown}, nil
	}
	size := int64(ino.Size)
	var c core.Content
	if ino.ForkType == libxfs.ForkTypeInlineData {
		c = core.InlineContent(size, ino.InlineData)
	} else {
		runs, err := a.vol.DataRuns(context.Background(), n.ID)
		if err != nil {
			return core.Content{}, err
		}
		var ext []core.Extent
		var next int64
		gap := false
		for _, r := range runs {
			if r.FileOffset > 1<<62 || r.LengthBytes > 1<<62 {
				return core.Content{}, fmt.Errorf("%w: run %+v", core.ErrBadExtent, r)
			}
			off, length := int64(r.FileOffset), int64(r.LengthBytes)
			if off != next {
				gap = true
				break
			}
			e := core.Extent{FileOffset: off, Length: length}
			switch {
			case r.IsUnwritten:
				e.Kind = core.ExtentUnwritten
			case r.IsSparse:
				e.Kind = core.ExtentSparse
			default:
				if r.StartOffset > uint64(1<<63-1-a.p.Base) {
					return core.Content{}, fmt.Errorf("%w: run at %d", core.ErrBadExtent, r.StartOffset)
				}
				e.Kind, e.ImageOffset = core.ExtentData, a.p.Base+int64(r.StartOffset)
			}
			ext = append(ext, e)
			next = off + length
		}
		if c, err = core.ExtentContent(size, ext); err != nil {
			return core.Content{}, err
		}
		if gap && c.Located < size {
			c.Issues = append(c.Issues, core.Issue{Code: core.IssueBadExtent, Path: n.Path, Inode: n.ID, Detail: fmt.Sprintf("data runs stop at %d with a gap", next)})
		}
	}
	if ino.NumberOfLinks > 1 {
		c.Prov |= core.ProvHardLink
	}
	return c, nil
}

// Open streams an inode through libxfs's own reader, which zero-fills holes
// and unwritten extents itself.
func (a *xfsFS) Open(n core.Node) (io.ReadCloser, error) {
	ino, ok, err := a.inodeOf(n)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("xfs: a deleted record has no content of its own")
	}
	if ino.ForkType == libxfs.ForkTypeInlineData {
		return io.NopCloser(bytes.NewReader(ino.InlineData)), nil
	}
	ra, err := a.vol.OpenInodeReader(n.ID)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.NewSectionReader(ra, 0, int64(ino.Size))), nil
}

func (a *xfsFS) Streams(n core.Node) ([]core.Stream, error) {
	if n.IsDeleted {
		return nil, nil
	}
	attrs, err := a.vol.ListInodeExtendedAttributes(n.ID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Stream, 0, len(attrs))
	for _, at := range attrs {
		name := at.Name
		if at.Namespace != "" {
			name = at.Namespace + "." + at.Name
		}
		out = append(out, core.Stream{Name: name, Kind: "xattr", Size: int64(len(at.Value))})
	}
	return out, nil
}
