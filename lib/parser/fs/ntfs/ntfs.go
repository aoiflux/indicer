// Package ntfs adapts libntfs to core.Filesystem. It is the reference
// FilesystemLayer adapter: it exposes the full contract (MACB, deleted
// enumeration, absolute data-run offsets, and alternate data streams) that the
// other filesystem adapters conform to.
package ntfs

import (
	"fmt"
	"io"
	"time"

	"github.com/aoiflux/libntfs"

	"indicer/lib/parser/core"
)

// ntfsFS adapts a libntfs.Volume to core.Filesystem.
type ntfsFS struct {
	vol  *libntfs.Volume
	base int64 // partition offset within the image; added to data-run offsets
}

var _ core.Filesystem = (*ntfsFS)(nil)

// Open mounts an NTFS filesystem from r. base is the partition's absolute byte
// offset within the image, added to data-run offsets so DataRuns returns
// image-absolute ranges; pass base=0 when r is the whole image. size (the
// partition length) is unused — libntfs derives geometry internally — but kept
// for a uniform adapter signature.
func Open(r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	vol, err := libntfs.Open(r)
	if err != nil {
		return nil, err
	}
	return &ntfsFS{vol: vol, base: base}, nil
}

func (a *ntfsFS) Type() core.FSType { return core.FSNTFS }

func (a *ntfsFS) Capabilities() core.Capabilities {
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: true}
}

func (a *ntfsFS) Close() error { return a.vol.Close() }

func (a *ntfsFS) Root() (core.Node, error) {
	root, err := a.vol.GetRootDirectory()
	if err != nil {
		return core.Node{}, err
	}
	return core.Node{ID: root.EntryNumber(), Name: root.Name(), Path: "/", Kind: core.KindDir}, nil
}

// Walk visits every entry reachable from the root, including deleted entries
// recovered from index slack. It is resilient: an unreadable directory subtree
// is skipped rather than aborting the whole walk. A visited set guards against
// hard-link / reparse cycles.
func (a *ntfsFS) Walk(fn func(core.Node) error) error {
	root, err := a.vol.GetRootDirectory()
	if err != nil {
		return err
	}
	return a.walkDir(root, "", make(map[uint64]bool), fn)
}

func (a *ntfsFS) walkDir(dir *libntfs.File, prefix string, visited map[uint64]bool, fn func(core.Node) error) error {
	if visited[dir.EntryNumber()] {
		return nil
	}
	visited[dir.EntryNumber()] = true

	entries, err := safeReadDir(dir)
	if err != nil {
		return nil // skip an unreadable subtree, keep walking the rest
	}

	for _, e := range entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." {
			continue
		}
		path := prefix + "/" + e.Name
		node := core.Node{
			ID:        e.EntryNum,
			Name:      e.Name,
			Path:      path,
			Kind:      dirEntryKind(e),
			IsDeleted: e.Deleted,
		}
		if err := fn(node); err != nil {
			return err
		}
		if e.IsDirectory && !e.Deleted && !visited[e.EntryNum] {
			child, err := a.vol.Open(e.EntryNum)
			if err != nil {
				continue
			}
			if err := a.walkDir(child, path, visited, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *ntfsFS) Stat(n core.Node) (info core.FileInfo, err error) {
	defer recoverAs("Stat", &err)
	file, err := a.vol.Open(n.ID)
	if err != nil {
		return core.FileInfo{}, err
	}
	info = core.FileInfo{
		Size:         file.Size(),
		Kind:         n.Kind,
		IsDeleted:    n.IsDeleted,
		IsFragmented: file.IsFragmented(),
		Inode:        n.ID,
	}
	if si, err := file.GetMetadata(); err == nil && si != nil {
		info.MACB = core.MACB{
			Modified: timePtr(si.ModifyTime),
			Accessed: timePtr(si.AccessTime),
			Changed:  timePtr(si.MFTChangeTime),
			Born:     timePtr(si.CreateTime),
		}
	}
	return info, nil
}

// DataRuns returns the node's default $DATA stream as image-absolute byte
// ranges. Resident (MFT-inline) content yields no ranges — read it via Open.
// Sparse holes are returned with Sparse=true and Start=0, End=length-1 (their
// length, not an offset — callers must zero-fill sparse ranges).
func (a *ntfsFS) DataRuns(n core.Node) (runs []core.AbsRange, err error) {
	defer recoverAs("DataRuns", &err)
	file, err := a.vol.Open(n.ID)
	if err != nil {
		return nil, err
	}
	frags, err := file.Fragments()
	if err != nil {
		return nil, err
	}
	out := make([]core.AbsRange, 0, len(frags))
	for _, fr := range frags {
		switch {
		case fr.Resident:
			continue
		case fr.Sparse:
			if fr.Length > 0 {
				out = append(out, core.AbsRange{Start: 0, End: fr.Length - 1, Sparse: true})
			}
		default:
			// libntfs EndOffset is exclusive (StartOffset+Length); AbsRange.End
			// is inclusive (the last byte), so subtract one.
			out = append(out, core.AbsRange{Start: a.base + fr.StartOffset, End: a.base + fr.EndOffset - 1})
		}
	}
	return out, nil
}

func (a *ntfsFS) Open(n core.Node) (rc io.ReadCloser, err error) {
	defer recoverAs("Open", &err)
	file, err := a.vol.Open(n.ID)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(file), nil
}

func (a *ntfsFS) Streams(n core.Node) (streams []core.Stream, err error) {
	defer recoverAs("Streams", &err)
	file, err := a.vol.Open(n.ID)
	if err != nil {
		return nil, err
	}
	var out []core.Stream
	for _, s := range file.Streams() {
		if !s.IsAlternate() {
			continue
		}
		out = append(out, core.Stream{Name: s.Name, Kind: "ads", Size: int64(s.Size)})
	}
	return out, nil
}

// safeReadDir calls dir.ReadDir but converts a panic from libntfs (it can index
// out of range on malformed/edge-case directory indexes) into an error so the
// walk skips that subtree instead of crashing the whole scan.
func safeReadDir(dir *libntfs.File) (entries []libntfs.DirEntry, err error) {
	defer func() {
		if r := recover(); r != nil {
			entries, err = nil, fmt.Errorf("ntfs: recovered panic in ReadDir: %v", r)
		}
	}()
	return dir.ReadDir()
}

// recoverAs turns a panic in a libntfs call into an error via the named return,
// keeping a single corrupt record from crashing the scan.
func recoverAs(where string, err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("ntfs: recovered panic in %s: %v", where, r)
	}
}

func dirEntryKind(e libntfs.DirEntry) core.NodeKind {
	if e.IsDirectory {
		return core.KindDir
	}
	return core.KindFile
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
