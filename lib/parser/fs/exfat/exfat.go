// Package exfat adapts libxfat (exFAT) to core.Filesystem.
//
// exFAT is cluster-based and has no inode: the libxfat Entry is the file
// handle. Walk hands each node its Entry as Node.Ref, so Stat and Content
// read it back without the adapter keeping a cache of the tree. Node.ID is
// the absolute offset of the entry set, which is stable within the volume.
package exfat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aoiflux/libxfat"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

type exfatFS struct {
	ex *libxfat.ExFAT
	p  core.OpenParams
}

var _ core.Filesystem = (*exfatFS)(nil)

// Open mounts an exFAT volume from p.Reader, which starts at the volume boot
// record, so libxfat reports partition-relative offsets and p.Base is added
// once, here.
func Open(p core.OpenParams) (core.Filesystem, error) {
	// Strict enables name-checksum validation; a mismatch is flagged on the
	// entry, not dropped. IgnorePartitionOffset skips only the PartitionOffset
	// cross-check, since imaging tools routinely record a PartitionOffset that
	// does not match where the volume actually sits.
	ex, err := libxfat.Open(libxfat.Source{
		Reader:                p.Reader,
		Size:                  p.Size,
		Strict:                true,
		IgnorePartitionOffset: true,
	})
	if err != nil {
		return nil, err
	}
	return &exfatFS{ex: ex, p: p}, nil
}

func (a *exfatFS) Type() core.FSType { return core.FSExFAT }

func (a *exfatFS) Capabilities() core.Capabilities {
	// exFAT carries no ctime; Changed is always nil.
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: false}
}

func (a *exfatFS) Close() error { return nil }

// Walk visits every entry: live, deleted in place (and the contents of
// deleted directories whose clusters are still free), then entry sets carved
// from unallocated clusters, under /$OrphanFiles. libxfat's walk is cycle-safe
// and depth-capped but silent about what it skips; a PreorderTracker reports
// unreadable directories, cycles and the depth cap.
func (a *exfatFS) Walk(ctx context.Context, fn func(core.Node) error) error {
	lim := a.p.Limits.WithDefaults()
	tracker := &fsutil.PreorderTracker{Sink: a.p.Sink, MaxDepth: lim.MaxDepth, Probe: a.probe}
	paths := &fsutil.RawPaths{MaxName: lim.MaxName}
	seen := map[int64]struct{}{}
	var emitted int64
	var synth uint64

	err := a.ex.WalkWithOptions(ctx, libxfat.WalkOptions{
		IncludeDeleted:            true,
		DescendDeletedDirectories: true,
		IncludeRecovered:          true,
		MaxDepth:                  lim.MaxDepth,
	}, func(raw string, _ uint32, e libxfat.Entry) error {
		id, ok := e.EntrySetOffset()
		if ok {
			if _, dup := seen[id]; dup {
				return nil // a carved copy of an entry the tree already gave
			}
			seen[id] = struct{}{}
		} else {
			synth++
			id = -int64(synth)
		}

		name := e.Name()
		path, san := paths.Join(raw, name)

		n := core.Node{
			ID:        uint64(id),
			Name:      name,
			Path:      path,
			Kind:      core.KindFile,
			IsDeleted: e.IsDeleted(),
			Prov:      entryProv(e, raw),
			Ref:       e,
		}
		if san {
			n.Prov |= core.ProvNameSanitized
		}
		if e.IsDir() {
			n.Kind = core.KindDir
			paths.Enter(raw, path)
		}
		liveDir := e.IsDir() && !e.IsDeleted() && !e.IsVirtualEntry() && !e.IsSpecialFile()
		tracker.Visit(path, liveDir, uint64(e.FirstCluster()), e)

		if emitted >= lim.MaxEntries {
			a.p.Report(core.Issue{Code: core.IssueEntryBudget, Path: path, Detail: fmt.Sprintf("stopped after %d entries", emitted)})
			return fsutil.ErrEntryBudget
		}
		emitted++
		return fn(n)
	})
	tracker.Flush()
	return err
}

func entryProv(e libxfat.Entry, raw string) core.Prov {
	var p core.Prov
	if e.IsDeleted() {
		p |= core.ProvDeleted
	}
	if strings.HasPrefix(raw, libxfat.RecoveredPath+"/") {
		p |= core.ProvRecovered | core.ProvOrphan
	}
	if e.NameChecksumMismatch() {
		p |= core.ProvChecksumMismatch
	}
	if e.HasSyntheticName() || e.HasNoName() {
		p |= core.ProvSynthName
	}
	if e.IsVirtualEntry() || e.IsSpecialFile() || e.IsRegion() || e.IsBitmapUpcase() || e.IsMetadataStream() {
		p |= core.ProvFSMetadata
	}
	return p
}

// probe counts a directory's children, for the tracker.
func (a *exfatFS) probe(ref any) (int, error) {
	entries, err := a.ex.ReadDir(ref.(libxfat.Entry))
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, libxfat.ErrEOF) {
		return 0, err
	}
	return len(entries), nil
}

func entryOf(n core.Node) (libxfat.Entry, error) {
	e, ok := n.Ref.(libxfat.Entry)
	if !ok {
		return libxfat.Entry{}, errors.New("exfat: node did not come from this filesystem's Walk")
	}
	return e, nil
}

func (a *exfatFS) Stat(n core.Node) (core.FileInfo, error) {
	e, err := entryOf(n)
	if err != nil {
		return core.FileInfo{}, err
	}
	info := core.FileInfo{
		Kind:      n.Kind,
		IsDeleted: e.IsDeleted(),
		Inode:     n.ID,
		MACB: core.MACB{
			Modified: fsutil.TimePtr(e.ModifiedTime()),
			Accessed: fsutil.TimePtr(e.AccessedTime()),
			Born:     fsutil.TimePtr(e.CreatedTime()),
		},
	}
	if !e.IsDir() {
		info.Size = int64(e.Size())
		info.IsFragmented = !e.IsContiguous()
	}
	return info, nil
}

// Content locates an entry's clusters. A live file with a FAT chain is walked;
// a contiguous (NoFatChain) file is its declared run; a deleted file is never
// chained through the current FAT, which belongs to whatever reused it, and
// is assumed contiguous from its first cluster unless it declared itself so.
// Bytes past the valid data length read as zeros.
func (a *exfatFS) Content(n core.Node) (core.Content, error) {
	e, err := entryOf(n)
	if err != nil {
		return core.Content{}, err
	}
	size := int64(e.Size())
	if size == 0 {
		return core.Content{Size: 0}, nil
	}
	lim := a.p.Limits.WithDefaults()
	res, err := a.ex.FragmentOffsetsWithOptions(e, libxfat.FragmentOptions{
		AssumeContiguous: e.IsDeleted(),
		MaxRuns:          lim.MaxExtents,
	})
	if err != nil {
		return core.Content{}, err
	}
	ext := make([]core.Extent, 0, len(res.Ranges))
	for _, r := range res.Ranges {
		if r.StartByte < 0 || r.StartByte > (1<<63-1)-a.p.Base {
			return core.Content{}, fmt.Errorf("%w: cluster offset %d", core.ErrBadExtent, r.StartByte)
		}
		ext = append(ext, core.Extent{FileOffset: r.FileOffset, Length: r.Length, ImageOffset: a.p.Base + r.StartByte, Kind: core.ExtentData})
	}
	c, err := core.ExtentContent(size, ext)
	if err != nil {
		return core.Content{}, err
	}
	c.MarkUnwritten(res.ValidBytes)

	switch {
	case res.Assumed:
		c.Prov |= core.ProvAssumedContiguous
		c.Confidence = core.ConfAssumed
	case e.IsDeleted():
		c.Confidence = core.ConfDeclared
	}
	if res.ChainBroken {
		c.Prov |= core.ProvBrokenChain
	}
	if res.LoopDetected {
		c.Prov |= core.ProvChainLoop
	}
	if res.AllocationContradiction {
		c.Prov |= core.ProvAllocationContradiction
	}
	if res.FirstClusterReallocated {
		c.Prov |= core.ProvReallocated
	}
	if res.Truncated {
		c.Prov |= core.ProvTruncated
	}
	return c, nil
}

// Open streams a live file through libxfat's own reader, which is an
// independent path from Content; a deleted file has no library reader and is
// served from its located content.
func (a *exfatFS) Open(n core.Node) (io.ReadCloser, error) {
	e, err := entryOf(n)
	if err != nil {
		return nil, err
	}
	if e.IsDeleted() {
		c, err := a.Content(n)
		if err != nil {
			return nil, err
		}
		return fsutil.OpenContent(a.p.Reader, a.p.Base, c)
	}
	f, err := a.ex.OpenEntry(e)
	if err != nil {
		return nil, err
	}
	ra, err := f.ReaderAt()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.NewSectionReader(ra, 0, f.Size())), nil
}

func (a *exfatFS) Streams(core.Node) ([]core.Stream, error) { return nil, nil }
