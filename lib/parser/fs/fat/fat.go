// Package fat adapts libfat (FAT12/16/32) to core.Filesystem.
//
// FAT has no inode: the libfat DirEntry is the file handle. Walk hands each
// node its DirEntry as Node.Ref, so Stat and Content read it back without the
// adapter keeping a cache of the tree. Node.ID is the entry's absolute offset
// on the volume when known.
package fat

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/aoiflux/libfat"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

type fatFS struct {
	vol *libfat.Volume
	p   core.OpenParams
}

var _ core.Filesystem = (*fatFS)(nil)

// Open mounts a FAT volume from p.Reader with no base offset, so libfat
// reports partition-relative offsets and p.Base is added once, here. The
// filesystem's own regions ($MBR, $FAT1, ...) are listed as metadata files,
// and deleted entries get their long names back where the slots survive.
func Open(p core.OpenParams) (core.Filesystem, error) {
	vol, err := libfat.OpenWithOptions(p.Reader, libfat.OpenOptions{
		IncludeVirtualRootEntries: true,
		RecoverDeletedLongNames:   true,
	})
	if err != nil {
		return nil, err
	}
	return &fatFS{vol: vol, p: p}, nil
}

func (a *fatFS) Type() core.FSType { return core.FSFAT }

func (a *fatFS) Capabilities() core.Capabilities {
	// FAT has no ctime (Changed always nil) and no streams.
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: false}
}

func (a *fatFS) Close() error { return a.vol.Close() }

// Walk visits every entry: live, deleted (and the contents of deleted
// directories whose first cluster is still free), then directories found by
// the orphan scan, under /$OrphanFiles. libfat's walk is cycle-safe and
// depth-capped but silent about what it skips; a PreorderTracker reports
// unreadable directories, cycles and the depth cap.
func (a *fatFS) Walk(ctx context.Context, fn func(core.Node) error) error {
	lim := a.p.Limits.WithDefaults()
	tracker := &fsutil.PreorderTracker{Sink: a.p.Sink, MaxDepth: lim.MaxDepth, Probe: a.probe}
	paths := &fsutil.RawPaths{MaxName: lim.MaxName}
	seen := map[libfat.FileID]struct{}{}
	var emitted int64
	var synth uint64

	err := a.vol.WalkWithOptions(ctx, libfat.WalkOptions{
		IncludeDeleted:            true,
		DescendDeletedDirectories: true,
		IncludeOrphans:            true,
		MaxDepth:                  lim.MaxDepth,
	}, func(raw string, _ uint32, e libfat.DirEntry) error {
		if id, ok := a.vol.FileID(e); ok {
			if _, dup := seen[id]; dup {
				return nil // an orphan the tree already gave
			}
			seen[id] = struct{}{}
		}
		var nodeID uint64
		if e.EntryAbsoluteOffset >= 0 {
			nodeID = uint64(e.EntryAbsoluteOffset)
		} else {
			synth++
			nodeID = 1<<63 | synth
		}

		path, san := paths.Join(raw, e.Name)
		n := core.Node{ID: nodeID, Name: e.Name, Path: path, Kind: core.KindFile, IsDeleted: e.Deleted, Prov: entryProv(e), Ref: e}
		if san {
			n.Prov |= core.ProvNameSanitized
		}
		if e.IsDirectory {
			n.Kind = core.KindDir
			paths.Enter(raw, path)
		}
		tracker.Visit(path, e.IsDirectory && !e.Deleted && !e.Virtual && !e.Orphaned, uint64(e.FirstCluster), e)

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

func entryProv(e libfat.DirEntry) core.Prov {
	var p core.Prov
	if e.Deleted {
		p |= core.ProvDeleted
		if e.NameSource == libfat.NameSourceShort && !e.FirstCharRecovered {
			p |= core.ProvSynthName // the first character is lost
		}
	}
	if e.Recovered {
		p |= core.ProvRecovered
	}
	if e.Orphaned {
		p |= core.ProvRecovered | core.ProvOrphan
	}
	if e.Virtual {
		p |= core.ProvFSMetadata
	}
	return p
}

// probe counts a directory's children, for the tracker.
func (a *fatFS) probe(ref any) (int, error) {
	f, err := a.vol.OpenEntry(ref.(libfat.DirEntry))
	if err != nil {
		return 0, err
	}
	entries, err := f.ReadDir()
	return len(entries), err
}

func entryOf(n core.Node) (libfat.DirEntry, error) {
	e, ok := n.Ref.(libfat.DirEntry)
	if !ok {
		return libfat.DirEntry{}, errors.New("fat: node did not come from this filesystem's Walk")
	}
	return e, nil
}

func (a *fatFS) Stat(n core.Node) (core.FileInfo, error) {
	e, err := entryOf(n)
	if err != nil {
		return core.FileInfo{}, err
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
	if e.IsDirectory {
		info.Size = 0
	}
	return info, nil
}

// Content locates an entry's clusters. A live file follows its FAT chain. A
// deleted file's chain is cleared when it is deleted, so it is assumed
// contiguous from its first cluster (ConfAssumed, PAR-29) and flagged
// reallocated when that cluster now belongs to another file.
func (a *fatFS) Content(n core.Node) (core.Content, error) {
	e, err := entryOf(n)
	if err != nil {
		return core.Content{}, err
	}
	size := int64(e.Size)
	if size == 0 || e.IsDirectory {
		return core.Content{}, nil
	}
	lim := a.p.Limits.WithDefaults()
	res, err := a.vol.FragmentOffsetsWithOptions(e, libfat.FragmentOptions{
		AssumeContiguous: e.Deleted,
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
	switch {
	case res.Assumed:
		c.Prov |= core.ProvAssumedContiguous
		c.Confidence = core.ConfAssumed
	case e.Deleted:
		c.Confidence = core.ConfDeclared
	}
	if res.ChainBroken {
		c.Prov |= core.ProvBrokenChain
	}
	if res.LoopDetected {
		c.Prov |= core.ProvChainLoop
	}
	if res.FirstClusterReallocated {
		c.Prov |= core.ProvReallocated
	}
	if res.Truncated {
		c.Prov |= core.ProvTruncated
	}
	return c, nil
}

// Open streams a live file through libfat's own reader, an independent path
// from Content. A deleted file is served from its located content, since
// libfat reads only its first cluster; so are empty and virtual entries,
// which libfat will not open.
func (a *fatFS) Open(n core.Node) (io.ReadCloser, error) {
	e, err := entryOf(n)
	if err != nil {
		return nil, err
	}
	if e.Deleted || e.Orphaned || e.Virtual || e.Size == 0 {
		c, err := a.Content(n)
		if err != nil {
			return nil, err
		}
		return fsutil.OpenContent(a.p.Reader, a.p.Base, c)
	}
	f, err := a.vol.OpenEntry(e)
	if err != nil {
		return nil, err
	}
	ra, err := f.ReaderAt()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(io.NewSectionReader(ra, 0, f.Size())), nil
}

func (a *fatFS) Streams(core.Node) ([]core.Stream, error) { return nil, nil }
