// Package scan is the high-level orchestrator of the pure-Go parsing stack. It
// chains the image, partition, and filesystem layers and streams one
// core.FileRecord per node to a Visitor, so very large evidence does not have
// to fit in memory.
//
// Nothing is dropped silently. Every allocated partition-table entry is
// announced, whatever its filesystem, with a status that says what became of
// it; a node whose metadata or content cannot be read is still emitted, with
// the issue attached; and each partition ends with a bounded report of what
// was seen and what went wrong.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"indicer/internal/safe"
	"indicer/lib/parser/core"
	"indicer/lib/parser/fs"
	"indicer/lib/parser/fs/fsutil"
	"indicer/lib/parser/image"
	"indicer/lib/parser/partition"
)

// Visitor receives a scan as it happens. Any callback may be nil. An error
// from a callback stops the scan and is returned by Scan.
type Visitor struct {
	// BeginPartition announces a partition before its files. Status is
	// provisional for a partition being walked; EndPartition has the final
	// one.
	BeginPartition func(core.PartitionInfo) error
	File           func(core.PartitionInfo, core.FileRecord) error
	EndPartition   func(core.PartitionInfo, *core.IndexReport) error
}

// Options tune a scan. The zero value is ready to use.
type Options struct {
	Limits core.Limits
}

// SourceReadError means the evidence itself could not be read: the device
// went away, or a sector is unreadable. It is not a parse error, and it ends
// the scan (PAR-32): carrying on would turn one dead disk into a million
// per-file errors and an index that looks complete.
type SourceReadError struct {
	Offset int64
	Err    error
}

func (e *SourceReadError) Error() string {
	return fmt.Sprintf("evidence read failed at offset %d: %v", e.Offset, e.Err)
}

func (e *SourceReadError) Unwrap() error { return e.Err }

// trackedSource records the first read error the image returns. Running out
// of image is not an error here: filesystem libraries probe past the end of
// damaged volumes, and the partition section reader bounds them.
type trackedSource struct {
	core.Image
	mu  sync.Mutex
	err *SourceReadError
}

func (s *trackedSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.Image.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		s.mu.Lock()
		if s.err == nil {
			s.err = &SourceReadError{Offset: off + int64(n), Err: err}
		}
		s.mu.Unlock()
	}
	return n, err
}

func (s *trackedSource) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return nil
	}
	return s.err
}

// Scan parses img's partition table and walks every partition that holds a
// filesystem an adapter can read. It returns the report of the whole image;
// the report is returned even with an error, covering what was done.
func Scan(ctx context.Context, img core.Image, v Visitor, opts Options) (*core.ImageReport, error) {
	opts.Limits = opts.Limits.WithDefaults()
	src := &trackedSource{Image: img}
	rep := &core.ImageReport{Format: img.Format(), Size: img.Size()}

	table, issues, err := partition.ParseReport(src)
	if err != nil {
		if serr := src.failed(); serr != nil {
			return rep, serr
		}
		return rep, fmt.Errorf("parse partition table: %w", err)
	}
	rep.Scheme = table.Scheme
	rep.Warnings = issues

	walked := false
	for _, p := range table.Partitions {
		if !p.Allocated {
			continue
		}
		r, err := scanPartition(ctx, src, p, v, opts)
		rep.Partitions = append(rep.Partitions, r)
		if err != nil {
			return rep, err
		}
		if r.Partition.Status == core.PartIndexed || r.Partition.Status == core.PartPartial {
			walked = true
		}
	}

	// A table parsed but nothing in it mounted: the "table" may be a
	// volume boot record that looks like one (a FAT boot sector carries the
	// 0x55AA signature). Try the device itself as one volume.
	if !walked && table.Scheme != core.SchemeNone {
		d, err := fs.DetectErr(io.NewSectionReader(src, 0, img.Size()))
		if err != nil {
			return rep, sourceErr(src, err)
		}
		if d.Type != core.FSUnknown {
			whole := core.Partition{Index: -1, Offset: 0, Size: img.Size(), TypeName: "whole-device", Allocated: true}
			r, err := scanPartition(ctx, src, whole, v, opts)
			rep.Partitions = append(rep.Partitions, r)
			if err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

func sourceErr(src *trackedSource, err error) error {
	if serr := src.failed(); serr != nil {
		return serr
	}
	return err
}

func scanPartition(ctx context.Context, src *trackedSource, p core.Partition, v Visitor, opts Options) (rep core.IndexReport, err error) {
	info := core.PartitionInfo{Partition: p, FSType: core.FSUnknown}
	rep.Partition = info
	end := func() error {
		rep.Partition = info
		if v.EndPartition != nil {
			return v.EndPartition(info, &rep)
		}
		return nil
	}
	begin := func() error {
		if v.BeginPartition != nil {
			return v.BeginPartition(info)
		}
		return nil
	}
	announce := func() error {
		if err := begin(); err != nil {
			return err
		}
		return end()
	}

	if p.Size <= 0 {
		info.Status, info.Detail = core.PartUnknown, "empty or outside the image"
		return rep, announce()
	}
	section := io.NewSectionReader(src, p.Offset, p.Size)

	det, err := fs.DetectErr(section)
	if err != nil {
		info.Status, info.Detail = core.PartReadError, err.Error()
		if aerr := announce(); aerr != nil {
			return rep, aerr
		}
		return rep, sourceErr(src, err)
	}
	if det.Type == core.FSUnknown {
		info.Status, info.Detail = det.Status, det.Detail
		return rep, announce()
	}
	info.FSType = det.Type

	params := core.OpenParams{
		Reader:    section,
		Base:      p.Offset,
		Size:      p.Size,
		ImageSize: src.Size(),
		Sink:      rep.Add,
		Limits:    opts.Limits,
	}
	fsys, err := fs.Open(det.Type, params)
	if err != nil {
		if serr := src.failed(); serr != nil {
			info.Status, info.Detail = core.PartReadError, serr.Error()
			if aerr := announce(); aerr != nil {
				return rep, aerr
			}
			return rep, serr
		}
		info.Status, info.Detail = core.PartOpenError, err.Error()
		return rep, announce()
	}
	defer func() {
		if cerr := fsys.Close(); cerr != nil && err == nil {
			rep.Add(core.Issue{Code: core.IssueLibrary, Detail: "close: " + cerr.Error()})
		}
	}()

	info.Status = core.PartIndexed
	if err := begin(); err != nil {
		return rep, err
	}

	var cbErr error
	walkErr := fsys.Walk(ctx, func(n core.Node) error {
		rec := buildRecord(fsys, p, det.Type, n, params)
		if serr := src.failed(); serr != nil {
			return serr
		}
		count(&rep, rec)
		if v.File != nil {
			if err := v.File(info, rec); err != nil {
				cbErr = err
				return err
			}
		}
		return nil
	})
	switch {
	case cbErr != nil:
		return rep, cbErr
	case src.failed() != nil:
		serr := src.failed()
		info.Status, info.Detail = core.PartReadError, serr.Error()
		if eerr := end(); eerr != nil {
			return rep, eerr
		}
		return rep, serr
	case walkErr != nil && ctx.Err() != nil:
		info.Status, info.Detail = core.PartPartial, "cancelled"
		_ = end()
		return rep, ctx.Err()
	case walkErr != nil:
		// A panic, an exhausted budget, or a root the library cannot read:
		// what was emitted stands, and the partition is marked partial.
		info.Status, info.Detail = core.PartPartial, walkErr.Error()
		code := core.IssueUnreadableDir
		switch {
		case errors.Is(walkErr, fsutil.ErrEntryBudget):
			code = core.IssueEntryBudget
		case isPanic(walkErr):
			code = core.IssuePanicRecovered
		}
		rep.Add(core.Issue{Code: code, Detail: walkErr.Error()})
	}
	return rep, end()
}

// buildRecord resolves a node into a record. Failures are attached to the
// record, never a reason to drop it.
func buildRecord(fsys core.Filesystem, p core.Partition, fsType core.FSType, n core.Node, params core.OpenParams) core.FileRecord {
	rec := core.FileRecord{
		Name:           n.Name,
		Path:           n.Path,
		Stream:         n.Stream,
		Kind:           n.Kind,
		IsDeleted:      n.IsDeleted,
		Prov:           n.Prov,
		Inode:          n.ID,
		Seq:            n.Seq,
		Parent:         n.Parent,
		PartitionIndex: p.Index,
		FSType:         fsType,
	}
	if n.IsDeleted {
		rec.Prov |= core.ProvDeleted
	}
	info, err := fsys.Stat(n)
	if err != nil {
		rec.Issues = append(rec.Issues, issue(core.IssueStatError, n, err))
		return rec
	}
	rec.Size = info.Size
	rec.IsDeleted = rec.IsDeleted || info.IsDeleted
	if rec.IsDeleted {
		rec.Prov |= core.ProvDeleted
	}
	rec.MACB = info.MACB
	if info.Inode != 0 {
		rec.Inode = info.Inode
	}
	rec.Target = info.Target
	rec.Links = info.Links
	if info.Kind == core.KindSymlink {
		rec.Kind = core.KindSymlink
	}
	if rec.Kind != core.KindFile {
		return rec
	}
	rec.Open = func() (io.ReadCloser, error) { return fsys.Open(n) }

	c, err := fsys.Content(n)
	if err != nil {
		rec.Issues = append(rec.Issues, issue(core.IssueContentError, n, err))
		c = core.Content{Kind: core.ContentUnavailable, Size: rec.Size, Confidence: core.ConfUnknown}
		if rec.Size > 0 {
			c.Prov |= core.ProvTruncated
		}
	}
	c.Validate(params.Bounds())
	c.Cap(params.Limits.MaxExtents)
	rec.Content = c
	rec.Prov |= c.Prov
	for _, i := range c.Issues {
		if i.Path == "" {
			i.Path = n.Path
		}
		rec.Issues = append(rec.Issues, i)
	}
	rec.Fragments = c.Ranges()
	rec.IsFragmented = len(rec.Fragments) > 1 || info.IsFragmented

	if streams, err := fsys.Streams(n); err != nil {
		rec.Issues = append(rec.Issues, issue(core.IssueStreamsError, n, err))
	} else {
		rec.Streams = streams
	}
	return rec
}

func issue(code core.IssueCode, n core.Node, err error) core.Issue {
	if isPanic(err) {
		code = core.IssuePanicRecovered
	}
	return core.Issue{Code: code, Path: n.Path, Inode: n.ID, Detail: err.Error()}
}

func isPanic(err error) bool { return safe.IsPanic(err) }

func count(rep *core.IndexReport, rec core.FileRecord) {
	switch rec.Kind {
	case core.KindFile:
		rep.FilesSeen++
	case core.KindDir:
		rep.DirsSeen++
	default:
		rep.OtherSeen++
	}
	if rec.IsDeleted {
		rep.Deleted++
	}
	for _, i := range rec.Issues {
		rep.Add(i)
	}
	if rec.Kind != core.KindFile {
		return
	}
	c := rec.Content
	data := 0
	for _, e := range c.Extents {
		if e.Kind == core.ExtentData {
			data++
		}
	}
	if data > 1 {
		rep.Fragmented++
	}
	if c.HoleBytes > 0 {
		rep.Sparse++
	}
	switch c.Kind {
	case core.ContentInline:
		rep.Inline++
	case core.ContentDecoded:
		rep.Decoded++
	case core.ContentUnavailable:
		rep.Unavailable++
	}
	if c.Prov.Has(core.ProvTruncated) {
		rep.Truncated++
	}
	rep.BytesData += c.DataBytes
}

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

// WalkImage is Scan with only a per-file callback, for callers that predate
// the Visitor.
func WalkImage(img core.Image, fn FileVisitor) error {
	_, err := Scan(context.Background(), img, Visitor{
		File: func(p core.PartitionInfo, rec core.FileRecord) error {
			return fn(p.Partition, p.FSType, rec)
		},
	}, Options{})
	return err
}
