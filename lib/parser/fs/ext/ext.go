// Package ext adapts libext (ext2/3/4) to core.Filesystem. ext is inode-based,
// so core.Node.ID is the inode number and metadata is resolved directly by
// number (no per-instance cache).
package ext

import (
	"bytes"
	"fmt"
	"io"

	"github.com/aoiflux/libext"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

type extFS struct {
	fs   *libext.FS
	base int64
}

var _ core.Filesystem = (*extFS)(nil)

// Open mounts an ext filesystem from r. base is the partition's absolute byte
// offset within the image, added to data-run disk offsets so DataRuns returns
// image-absolute ranges.
func Open(r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	var (
		efs *libext.FS
		err error
	)
	if size > 0 {
		efs, err = libext.OpenWithSize(r, uint64(size))
	} else {
		efs, err = libext.Open(r)
	}
	if err != nil {
		return nil, err
	}
	return &extFS{fs: efs, base: base}, nil
}

func (a *extFS) Type() core.FSType { return core.FSExt }

func (a *extFS) Capabilities() core.Capabilities {
	// ext exposes no crtime, so MACB.Born is always nil.
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *extFS) Close() error { return nil }

func (a *extFS) Root() (core.Node, error) {
	root, err := a.fs.GetRootDirectory()
	if err != nil {
		return core.Node{}, err
	}
	return core.Node{ID: uint64(root.InodeNumber()), Name: "/", Path: "/", Kind: core.KindDir}, nil
}

// Walk visits the live tree from the root, then appends deleted/orphan inodes
// recovered by libext (under a synthetic /$deleted path when the original path
// is unknown).
func (a *extFS) Walk(fn func(core.Node) error) error {
	root, err := a.fs.GetRootDirectory()
	if err != nil {
		return err
	}
	rootIno := root.InodeNumber()
	if err := a.walk(rootIno, "", map[uint32]bool{rootIno: true}, fn); err != nil {
		return err
	}
	return a.emitDeleted(fn)
}

func (a *extFS) walk(inode uint32, prefix string, visited map[uint32]bool, fn func(core.Node) error) error {
	entries, err := a.fs.ListDir(inode)
	if err != nil {
		return nil // skip an unreadable directory, keep walking
	}
	for _, e := range entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." {
			continue
		}
		path := prefix + "/" + e.Name
		node := core.Node{ID: uint64(e.Inode), Name: e.Name, Path: path, Kind: boolKind(e.IsDirectory)}
		if err := fn(node); err != nil {
			return err
		}
		if e.IsDirectory && e.Inode != 0 && !visited[e.Inode] {
			visited[e.Inode] = true
			if err := a.walk(e.Inode, path, visited, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *extFS) emitDeleted(fn func(core.Node) error) error {
	deleted, err := a.fs.DeletedEntries()
	if err != nil {
		return nil // best-effort
	}
	for _, d := range deleted {
		name := d.Name
		if name == "" {
			name = fmt.Sprintf("inode-%d", d.Inode)
		}
		path := d.Path
		if path == "" {
			path = "/$deleted/" + name
		}
		node := core.Node{ID: uint64(d.Inode), Name: name, Path: path, Kind: modeKind(d.Mode), IsDeleted: true}
		if err := fn(node); err != nil {
			return err
		}
	}
	return nil
}

func (a *extFS) Stat(n core.Node) (core.FileInfo, error) {
	ino, err := a.fs.ReadInode(uint32(n.ID))
	if err != nil {
		return core.FileInfo{}, err
	}
	info := core.FileInfo{
		Size:      int64(ino.Size),
		Kind:      n.Kind,
		IsDeleted: n.IsDeleted || !ino.Dtime.IsZero(),
		Inode:     n.ID,
		MACB: core.MACB{
			Modified: fsutil.TimePtr(ino.Mtime),
			Accessed: fsutil.TimePtr(ino.Atime),
			Changed:  fsutil.TimePtr(ino.Ctime),
		},
	}
	if ex, err := a.fs.Extents(uint32(n.ID)); err == nil {
		info.IsFragmented = len(ex) > 1
	}
	return info, nil
}

// DataRuns returns the inode's data as image-absolute byte ranges. libext's
// ByteRange is already clamped to the file size; DiskOffset is partition-relative
// here (BaseOffset unset), so base is added to make it image-absolute.
func (a *extFS) DataRuns(n core.Node) ([]core.AbsRange, error) {
	runs, err := a.fs.DataRuns(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	out := make([]core.AbsRange, 0, len(runs))
	for _, r := range runs {
		if r.Length <= 0 {
			continue
		}
		if r.Sparse {
			out = append(out, core.AbsRange{Start: 0, End: r.Length - 1, Sparse: true})
			continue
		}
		start := a.base + r.DiskOffset
		out = append(out, core.AbsRange{Start: start, End: start + r.Length - 1})
	}
	return out, nil
}

func (a *extFS) Open(n core.Node) (io.ReadCloser, error) {
	data, err := a.fs.ReadFile(uint32(n.ID))
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (a *extFS) Streams(n core.Node) ([]core.Stream, error) {
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

func boolKind(isDir bool) core.NodeKind {
	if isDir {
		return core.KindDir
	}
	return core.KindFile
}

func modeKind(mode uint16) core.NodeKind {
	if mode&0xF000 == 0x4000 { // S_IFDIR
		return core.KindDir
	}
	return core.KindFile
}
