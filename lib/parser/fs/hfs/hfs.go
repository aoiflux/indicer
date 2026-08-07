// Package hfs adapts libhfs (HFS+/HFSX) to core.Filesystem. HFS is CNID-based,
// so core.Node.ID is the catalog node id (CNID), resolved directly.
package hfs

import (
	"fmt"
	"io"

	libhfs "github.com/aoiflux/libhfs"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

type hfsFS struct {
	vol  *libhfs.Volume
	base int64
	bs   int64 // allocation block size
}

var _ core.Filesystem = (*hfsFS)(nil)

// Open mounts an HFS+/HFSX filesystem from r. base is the partition's absolute
// byte offset within the image, added to fork-extent offsets so DataRuns returns
// image-absolute ranges.
func Open(r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	vol, err := libhfs.Open(r)
	if err != nil {
		return nil, err
	}
	return &hfsFS{vol: vol, base: base, bs: int64(vol.Header().BlockSize)}, nil
}

func (a *hfsFS) Type() core.FSType { return core.FSHFS }

func (a *hfsFS) Capabilities() core.Capabilities {
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *hfsFS) Close() error { return nil }

func (a *hfsFS) Root() (core.Node, error) {
	root, err := a.vol.GetRootDirectory()
	if err != nil {
		return core.Node{}, err
	}
	return core.Node{ID: uint64(root.CNID), Name: "/", Path: "/", Kind: core.KindDir}, nil
}

// Walk visits the live catalog from the root, then appends deleted records
// recovered by libhfs (under a synthetic /$deleted path).
func (a *hfsFS) Walk(fn func(core.Node) error) error {
	root, err := a.vol.GetRootDirectory()
	if err != nil {
		return err
	}
	if err := a.walk(root.CNID, "", map[uint32]bool{root.CNID: true}, fn); err != nil {
		return err
	}
	// Deleted recovery is best-effort and supplementary; ignore its walk errors.
	_ = a.vol.WalkDeleted(libhfs.DefaultRecoveryOptions(), func(d libhfs.DeletedRecord) error {
		name := d.Record.Name
		if name == "" {
			name = fmt.Sprintf("cnid-%d", d.Record.CNID)
		}
		return fn(core.Node{
			ID:        uint64(d.Record.CNID),
			Name:      name,
			Path:      "/$deleted/" + name,
			Kind:      core.KindFile,
			IsDeleted: true,
		})
	})
	return nil
}

func (a *hfsFS) walk(cnid uint32, prefix string, visited map[uint32]bool, fn func(core.Node) error) error {
	entries, err := a.vol.ReadDirCNID(cnid)
	if err != nil {
		return nil // skip an unreadable directory, keep walking
	}
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		path := prefix + "/" + e.Name
		node := core.Node{ID: uint64(e.CNID), Name: e.Name, Path: path, Kind: boolKind(e.IsDirectory)}
		if err := fn(node); err != nil {
			return err
		}
		if e.IsDirectory && e.CNID != 0 && !visited[e.CNID] {
			visited[e.CNID] = true
			if err := a.walk(e.CNID, path, visited, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *hfsFS) Stat(n core.Node) (core.FileInfo, error) {
	info := core.FileInfo{Kind: n.Kind, IsDeleted: n.IsDeleted, Inode: n.ID}
	if t, err := a.vol.GetTimes(uint32(n.ID)); err == nil {
		info.MACB = core.MACB{
			Modified: fsutil.TimePtr(t.ContentModified),
			Accessed: fsutil.TimePtr(t.Accessed),
			Changed:  fsutil.TimePtr(t.AttrModified),
			Born:     fsutil.TimePtr(t.Created),
		}
	}
	if n.Kind == core.KindFile {
		if f, err := a.vol.OpenFileByCNID(uint32(n.ID)); err == nil {
			info.Size = f.Size()
		}
		if exts, err := a.vol.ResolveDataForkExtents(uint32(n.ID)); err == nil {
			info.IsFragmented = len(exts) > 1
		}
	}
	return info, nil
}

// DataRuns returns the data fork's allocation-block extents as image-absolute
// byte ranges, trimmed to the file's logical size.
func (a *hfsFS) DataRuns(n core.Node) ([]core.AbsRange, error) {
	exts, err := a.vol.ResolveDataForkExtents(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	out := make([]core.AbsRange, 0, len(exts))
	for _, e := range exts {
		if e.BlockCount == 0 {
			continue
		}
		start := a.base + int64(e.StartBlock)*a.bs
		length := int64(e.BlockCount) * a.bs
		out = append(out, core.AbsRange{Start: start, End: start + length - 1})
	}
	var size int64
	if f, err := a.vol.OpenFileByCNID(uint32(n.ID)); err == nil {
		size = f.Size()
	}
	return fsutil.TrimToSize(out, size), nil
}

func (a *hfsFS) Open(n core.Node) (io.ReadCloser, error) {
	f, err := a.vol.OpenFileByCNID(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(f), nil
}

// Streams returns the resource fork (when present) and extended attributes.
func (a *hfsFS) Streams(n core.Node) ([]core.Stream, error) {
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

func boolKind(isDir bool) core.NodeKind {
	if isDir {
		return core.KindDir
	}
	return core.KindFile
}
