// Package scan is the high-level orchestrator of the pure-Go parsing stack. It
// chains the image, partition, and filesystem layers to produce
// core.FileRecords — the behavioural equivalent of libtusk_analyze, but pure Go.
//
// The stack is walked lazily: WalkImage streams one FileRecord per file (with
// its partition and filesystem context) rather than materialising the whole
// tree, so very large evidence does not have to fit in memory.
package scan

import (
	"errors"
	"io"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs"
	"indicer/lib/parser/image"
	"indicer/lib/parser/partition"
)

// FileVisitor receives each file discovered during a scan, together with the
// partition and filesystem type it came from. Returning a non-nil error stops
// the scan and is propagated by WalkImage.
type FileVisitor func(part core.Partition, fsType core.FSType, rec core.FileRecord) error

// WalkPath opens the image at path (auto-detecting raw/EWF/VHD) and walks it.
func WalkPath(path string, fn FileVisitor) error {
	img, err := image.Open(path)
	if err != nil {
		return err
	}
	defer img.Close()
	return WalkImage(img, fn)
}

// WalkImage parses the partition table over img and, for every allocated
// partition carrying a recognised filesystem, walks the filesystem and calls fn
// for each file. Partitions that cannot be parsed or whose filesystem is not
// recognised are skipped (the scan is resilient by design).
func WalkImage(img core.Image, fn FileVisitor) error {
	table, err := partition.Parse(img)
	if err != nil {
		return err
	}
	for _, p := range table.Partitions {
		if !p.Allocated || p.Size <= 0 {
			continue
		}
		if err := walkPartition(img, p, fn); err != nil {
			return err
		}
	}
	return nil
}

func walkPartition(img core.Image, p core.Partition, fn FileVisitor) error {
	section := io.NewSectionReader(img, p.Offset, p.Size)

	fsys, err := fs.OpenAuto(section, p.Offset, p.Size)
	if err != nil {
		// Unrecognised or unmountable filesystem — skip this partition.
		// (Carving unallocated / unknown space is a separate, later concern.)
		if errors.Is(err, fs.ErrUnknownFS) {
			return nil
		}
		return nil
	}
	defer fsys.Close()

	fsType := fsys.Type()
	return fsys.Walk(func(n core.Node) error {
		rec, err := buildRecord(fsys, p, fsType, n)
		if err != nil {
			return nil // skip a file we cannot resolve, keep walking
		}
		return fn(p, fsType, rec)
	})
}

// buildRecord assembles a FileRecord from a node's metadata, data-run offsets,
// and streams. Directories carry no fragments or streams.
func buildRecord(fsys core.Filesystem, p core.Partition, fsType core.FSType, n core.Node) (core.FileRecord, error) {
	info, err := fsys.Stat(n)
	if err != nil {
		return core.FileRecord{}, err
	}
	rec := core.FileRecord{
		Name:           n.Name,
		Path:           n.Path,
		Kind:           n.Kind,
		Size:           info.Size,
		IsDeleted:      info.IsDeleted,
		IsFragmented:   info.IsFragmented,
		MACB:           info.MACB,
		Inode:          info.Inode,
		PartitionIndex: p.Index,
		FSType:         fsType,
	}
	if n.Kind == core.KindFile {
		if runs, err := fsys.DataRuns(n); err == nil {
			rec.Fragments = runs
			if len(runs) > 1 {
				rec.IsFragmented = true
			}
		}
		if streams, err := fsys.Streams(n); err == nil {
			rec.Streams = streams
		}
	}
	return rec, nil
}
