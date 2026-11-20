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
// and depth-capped; OnSkip names each directory it did not descend into or
// read only part of, and OnRecoveredScan says when the sweep for carved
// entries was incomplete.
func (a *exfatFS) Walk(ctx context.Context, fn func(core.Node) error) error {
	lim := a.p.Limits.WithDefaults()
	paths := &fsutil.RawPaths{MaxName: lim.MaxName}
	var last struct {
		id   int64
		path string
	}
	seen := map[int64]struct{}{}
	var emitted int64
	var synth uint64

	err := a.ex.WalkWithOptions(ctx, libxfat.WalkOptions{
		IncludeDeleted:            true,
		DescendDeletedDirectories: true,
		IncludeRecovered:          true,
		MaxDepth:                  lim.MaxDepth,
		OnSkip: func(raw string, e libxfat.Entry, reason libxfat.SkipReason, err error) error {
			path := last.path
			if id, ok := e.EntrySetOffset(); !ok || id != last.id || path == "" {
				path = fsutil.EscapePath(raw)
			}
			if i, ok := skipIssue(path, e, reason, err); ok {
				a.p.Report(i)
			}
			return nil
		},
		OnRecoveredScan: func(r *libxfat.RecoveredScanResult) error {
			if r.Truncated {
				a.p.Report(core.Issue{Code: core.IssueLibrary, Path: libxfat.RecoveredPath,
					Detail: fmt.Sprintf("the image ends inside the allocation bitmap: %d of the heap's clusters were not swept for carved entries", r.ClustersUnclassified)})
			}
			return nil
		},
	}, func(raw string, _ uint32, e libxfat.Entry) error {
		last.id, last.path = -1, ""
		id, ok := e.EntrySetOffset()
		if ok {
			last.id = id
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
		last.path = path

		if emitted >= lim.MaxEntries {
			a.p.Report(core.Issue{Code: core.IssueEntryBudget, Path: path, Detail: fmt.Sprintf("stopped after %d entries", emitted)})
			return fsutil.ErrEntryBudget
		}
		emitted++
		return fn(n)
	})
	return err
}

// skipIssue turns a directory libxfat did not descend into, or read only part
// of, into an issue. A deleted or carved directory whose clusters are in use
// again, were taken by a live directory, or lie outside the heap is ordinary
// and reported by nothing but its content's provenance.
func skipIssue(path string, e libxfat.Entry, reason libxfat.SkipReason, err error) (core.Issue, bool) {
	live := !e.IsDeleted() && !strings.HasPrefix(path, libxfat.RecoveredPath+"/")
	i := core.Issue{Path: path}
	if id, ok := e.EntrySetOffset(); ok {
		i.Inode = uint64(id)
	}
	switch {
	case reason == libxfat.SkipDepthLimit:
		i.Code, i.Detail = core.IssueDepthCap, fmt.Sprintf("depth %d", strings.Count(path, "/"))
	case reason == libxfat.SkipReadError:
		i.Code, i.Detail = core.IssueUnreadableDir, err.Error()
	case reason == libxfat.SkipPartialRead && live:
		i.Code, i.Detail = core.IssuePartialDir, err.Error()
	case reason == libxfat.SkipRevisit && live:
		i.Code, i.Detail = core.IssueCycle, "directory already visited"
	case reason == libxfat.SkipInvalidCluster && live:
		i.Code, i.Detail = core.IssueUnreadableDir, fmt.Sprintf("first cluster %d is outside the cluster heap", e.FirstCluster())
	default:
		return core.Issue{}, false
	}
	return i, true
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
