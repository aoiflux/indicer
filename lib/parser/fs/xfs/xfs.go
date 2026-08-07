// Package xfs adapts libxfs to core.Filesystem. It is the second reference
// FilesystemLayer adapter, exercising full MACB+birth timestamps, exported
// extents, heuristic deleted/carved recovery, and extended attributes.
package xfs

import (
	"bytes"
	"io"
	"time"

	"github.com/aoiflux/libxfs"

	"indicer/lib/parser/core"
)

type xfsFS struct {
	vol  *libxfs.Volume
	sb   libxfs.Superblock
	base int64 // partition offset within the image; added to data-run offsets
}

var _ core.Filesystem = (*xfsFS)(nil)

// Open mounts an XFS filesystem from r. base is the partition's absolute byte
// offset within the image, added to data-run offsets so DataRuns returns
// image-absolute ranges. size (the partition length) is unused — libxfs derives
// geometry internally — but kept for a uniform adapter signature.
func Open(r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	vol, err := libxfs.Open(r)
	if err != nil {
		return nil, err
	}
	return &xfsFS{vol: vol, sb: vol.Superblock(), base: base}, nil
}

func (a *xfsFS) Type() core.FSType { return core.FSXFS }

func (a *xfsFS) Capabilities() core.Capabilities {
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *xfsFS) Close() error { return a.vol.Close() }

func (a *xfsFS) Root() (core.Node, error) {
	ino, err := a.vol.ResolveInodeByPath("/")
	if err != nil {
		return core.Node{}, err
	}
	return core.Node{ID: ino, Name: "/", Path: "/", Kind: core.KindDir}, nil
}

// Walk lists every directory record reachable from the root — active entries
// plus deleted and heuristically-carved records (see DirectoryRecord.Kind).
// Recursion only descends into verified-active directories to avoid following
// carved (possibly bogus) inode numbers; a visited set guards against cycles.
func (a *xfsFS) Walk(fn func(core.Node) error) error {
	root, err := a.vol.ResolveInodeByPath("/")
	if err != nil {
		return err
	}
	return a.walkDir(root, "", map[uint64]bool{root: true}, fn)
}

func (a *xfsFS) walkDir(inode uint64, prefix string, visited map[uint64]bool, fn func(core.Node) error) error {
	records, err := a.vol.ScanDirectoryRecords(inode)
	if err != nil {
		return nil // skip an unreadable directory, keep walking the rest
	}
	for _, r := range records {
		if r.Name == "" || r.Name == "." || r.Name == ".." {
			continue
		}
		path := prefix + "/" + r.Name

		kind := core.KindFile
		descend := false
		if r.Kind == libxfs.RecordKindActive {
			if ino, err := a.vol.OpenInode(r.InodeNumber); err == nil && ino.IsDirectory() {
				kind = core.KindDir
				descend = !visited[r.InodeNumber]
			}
		}

		node := core.Node{
			ID:        r.InodeNumber,
			Name:      r.Name,
			Path:      path,
			Kind:      kind,
			IsDeleted: r.IsDeleted || r.IsCarved,
		}
		if err := fn(node); err != nil {
			return err
		}

		if descend {
			visited[r.InodeNumber] = true
			if err := a.walkDir(r.InodeNumber, path, visited, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *xfsFS) Stat(n core.Node) (core.FileInfo, error) {
	ino, err := a.vol.OpenInode(n.ID)
	if err != nil {
		return core.FileInfo{}, err
	}
	return core.FileInfo{
		Size:         int64(ino.Size),
		Kind:         n.Kind,
		IsDeleted:    n.IsDeleted,
		IsFragmented: ino.DataExtentCount > 1,
		Inode:        n.ID,
		MACB: core.MACB{
			Modified: timePtr(ino.ModificationTime()),
			Accessed: timePtr(ino.AccessTime()),
			Changed:  timePtr(ino.InodeChangeTime()),
			Born:     timePtr(ino.CreationTime()),
		},
	}, nil
}

// DataRuns returns the inode's data-fork extents as image-absolute byte ranges.
// XFS extents carry an AG-encoded filesystem block number; the conversion here
// mirrors libxfs's own read path (fsbno → AG index/relative block → byte). Sparse
// / unwritten extents are returned with Sparse=true and Start=0, End=length-1.
// Inline (resident) data has no extents — read it via Open.
func (a *xfsFS) DataRuns(n core.Node) ([]core.AbsRange, error) {
	ino, err := a.vol.OpenInode(n.ID)
	if err != nil {
		return nil, err
	}
	bs := int64(a.sb.BlockSize)
	out := make([]core.AbsRange, 0, len(ino.DataExtents))
	for _, e := range ino.DataExtents {
		length := int64(e.NumberOfBlocks) * bs
		if length <= 0 {
			continue
		}
		if e.RangeFlags&libxfs.ExtentFlagSparse != 0 {
			out = append(out, core.AbsRange{Start: 0, End: length - 1, Sparse: true})
			continue
		}
		start := a.base + a.fsbToByte(e.PhysicalBlockNumber)
		out = append(out, core.AbsRange{Start: start, End: start + length - 1})
	}
	return out, nil
}

// fsbToByte converts an XFS filesystem block number (AG-encoded) to a byte
// offset within the volume, mirroring libxfs volume.go.
func (a *xfsFS) fsbToByte(fsbno uint64) int64 {
	relBits := a.sb.RelativeBlockNumberBits
	agIndex := fsbno >> relBits
	relBlock := fsbno & ((uint64(1) << relBits) - 1)
	offsetBlocks := agIndex*uint64(a.sb.AllocationGroupSize) + relBlock
	return int64(offsetBlocks * uint64(a.sb.BlockSize))
}

func (a *xfsFS) Open(n core.Node) (io.ReadCloser, error) {
	data, err := a.vol.ReadFileData(n.ID)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (a *xfsFS) Streams(n core.Node) ([]core.Stream, error) {
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

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
