// Package fat adapts libfat (FAT12/16/32) to core.Filesystem.
//
// Like exFAT, FAT is cluster-based with no inode: the libfat DirEntry is the
// file handle. The adapter caches entries seen during Walk and keys
// core.Node.ID to that cache. Node IDs are opaque handles valid within this
// Filesystem instance after Walk has visited the node.
package fat

import (
	"errors"
	"io"

	"github.com/aoiflux/libfat"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

var errUnknownNode = errors.New("fat: node not resolved (Walk must run before Stat/DataRuns)")

type fatFS struct {
	vol   *libfat.Volume
	base  int64
	next  uint64
	cache map[uint64]libfat.DirEntry
}

var _ core.Filesystem = (*fatFS)(nil)

// Open mounts a FAT filesystem from r. base is the partition's absolute byte
// offset within the image, added to fragment offsets so DataRuns returns
// image-absolute ranges. size is unused (libfat derives geometry internally).
func Open(r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	vol, err := libfat.Open(r)
	if err != nil {
		return nil, err
	}
	return &fatFS{vol: vol, base: base, cache: make(map[uint64]libfat.DirEntry)}, nil
}

func (a *fatFS) Type() core.FSType { return core.FSFAT }

func (a *fatFS) Capabilities() core.Capabilities {
	// FAT has no ctime (Changed always nil) and no streams.
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: false}
}

func (a *fatFS) Close() error { return nil }

func (a *fatFS) put(e libfat.DirEntry) uint64 {
	id := a.next
	a.next++
	a.cache[id] = e
	return id
}

func (a *fatFS) Root() (core.Node, error) {
	return core.Node{ID: 0, Name: "/", Path: "/", Kind: core.KindDir}, nil
}

func (a *fatFS) Walk(fn func(core.Node) error) error {
	root, err := a.vol.GetRootDirectory()
	if err != nil {
		return err
	}
	return a.walk(root, make(map[uint32]bool), fn)
}

func (a *fatFS) walk(dir *libfat.File, visited map[uint32]bool, fn func(core.Node) error) error {
	entries, err := dir.ReadDir()
	if err != nil {
		return nil // skip an unreadable directory, keep walking the rest
	}
	for _, e := range entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." {
			continue
		}
		path := e.Path
		if path == "" {
			path = "/" + e.Name
		}
		node := core.Node{
			ID:        a.put(e),
			Name:      e.Name,
			Path:      path,
			Kind:      entryKind(e),
			IsDeleted: e.Deleted,
		}
		if err := fn(node); err != nil {
			return err
		}
		if e.IsDirectory && !e.Deleted && e.FirstCluster != 0 && !visited[e.FirstCluster] {
			visited[e.FirstCluster] = true
			child, err := a.vol.OpenEntry(e)
			if err != nil {
				continue
			}
			if err := a.walk(child, visited, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *fatFS) Stat(n core.Node) (core.FileInfo, error) {
	e, ok := a.cache[n.ID]
	if !ok {
		return core.FileInfo{}, errUnknownNode
	}
	info := core.FileInfo{
		Size:      int64(e.Size),
		Kind:      n.Kind,
		IsDeleted: e.Deleted,
		Inode:     n.ID,
		MACB: core.MACB{
			Modified: fsutil.TimePtr(e.ModifiedAt),
			Accessed: fsutil.TimePtr(e.AccessedAt),
			Born:     fsutil.TimePtr(e.CreatedAt),
		},
	}
	if runs, err := a.vol.FragmentOffsets(e); err == nil {
		info.IsFragmented = len(runs) > 1
	}
	return info, nil
}

// DataRuns returns the entry's runs as image-absolute byte ranges, trimmed to
// the file's logical size. FAT has no sparse allocation.
func (a *fatFS) DataRuns(n core.Node) ([]core.AbsRange, error) {
	e, ok := a.cache[n.ID]
	if !ok {
		return nil, errUnknownNode
	}
	runs, err := a.vol.FragmentOffsets(e)
	if err != nil {
		return nil, err
	}
	out := make([]core.AbsRange, 0, len(runs))
	for _, r := range runs {
		if r.Length <= 0 {
			continue
		}
		start := a.base + r.StartByte
		out = append(out, core.AbsRange{Start: start, End: start + r.Length - 1, Sparse: r.Sparse})
	}
	return fsutil.TrimToSize(out, int64(e.Size)), nil
}

func (a *fatFS) Open(n core.Node) (io.ReadCloser, error) {
	e, ok := a.cache[n.ID]
	if !ok {
		return nil, errUnknownNode
	}
	file, err := a.vol.OpenEntry(e)
	if err != nil {
		return nil, err
	}
	rd, err := file.Reader()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(rd), nil
}

func (a *fatFS) Streams(_ core.Node) ([]core.Stream, error) { return nil, nil }

func entryKind(e libfat.DirEntry) core.NodeKind {
	if e.IsDirectory {
		return core.KindDir
	}
	return core.KindFile
}
