// Package exfat adapts libxfat (exFAT) to core.Filesystem.
//
// exFAT is cluster-based and has no inode: the libxfat Entry is itself the file
// handle. The adapter therefore caches entries encountered during Walk and keys
// core.Node.ID to that cache, so Stat/DataRuns/Open/Streams can recover the
// Entry. Node IDs are opaque handles valid within this Filesystem instance after
// Walk (or Root) has visited the node.
package exfat

import (
	"bytes"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/aoiflux/libxfat"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/fsutil"
)

var errUnknownNode = errors.New("exfat: node not resolved (Walk must run before Stat/DataRuns)")

type exfatFS struct {
	ex    *libxfat.ExFAT
	r     io.ReaderAt // partition-relative reader, for content assembly
	base  int64       // partition offset within the image
	next  uint64      // node id counter
	cache map[uint64]libxfat.Entry
}

var _ core.Filesystem = (*exfatFS)(nil)

// Open mounts an exFAT filesystem from r (a partition-relative reader of length
// size). base is the partition's absolute byte offset within the image, added to
// cluster offsets so DataRuns returns image-absolute ranges.
func Open(r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	// r is partition-relative (a SectionReader over the volume), so Base is 0.
	//
	// Strict enables forensic name-checksum validation. The v1.1.0 strict path
	// false-positived on superfloppy exFAT (nonzero VBR PartitionOffset) and
	// silently blanked every name; libxfat v1.2.0 fixed it (verified: strict now
	// yields identical names/full paths to optimistic on that image).
	// IgnorePartitionOffset skips only the PartitionOffset cross-check, since
	// imaging tools routinely record a PartitionOffset that does not match where
	// the volume actually sits.
	ex, err := libxfat.Open(libxfat.Source{
		Reader:                r,
		Size:                  size,
		Strict:                true,
		IgnorePartitionOffset: true,
	})
	if err != nil {
		return nil, err
	}
	return &exfatFS{ex: ex, r: r, base: base, cache: make(map[uint64]libxfat.Entry)}, nil
}

func (a *exfatFS) Type() core.FSType { return core.FSExFAT }

func (a *exfatFS) Capabilities() core.Capabilities {
	// exFAT carries no ctime; Changed is always nil.
	return core.Capabilities{MACB: true, Deleted: true, Fragments: true, Streams: false}
}

func (a *exfatFS) Close() error { return nil }

func (a *exfatFS) put(e libxfat.Entry) uint64 {
	id := a.next
	a.next++
	a.cache[id] = e
	return id
}

func (a *exfatFS) Root() (core.Node, error) {
	// exFAT has no distinct root entry object; expose a synthetic root.
	return core.Node{ID: 0, Name: "/", Path: "/", Kind: core.KindDir}, nil
}

func (a *exfatFS) Walk(fn func(core.Node) error) error {
	root, err := a.ex.ReadRootDir()
	if err != nil {
		return err
	}
	// GetFullPathIndexableEntries recursively walks the tree and stamps each
	// entry's name with its full path (e.g. "/generator/.git/HEAD"). It returns
	// the indexable file entries — directory-table entries are intentionally not
	// surfaced (forensically the content that matters is the files).
	entries, err := a.ex.GetFullPathIndexableEntries(root, "/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := e.GetName()
		node := core.Node{
			ID:        a.put(e),
			Name:      path.Base(full),
			Path:      full,
			Kind:      entryKind(e),
			IsDeleted: e.IsDeleted(),
		}
		if err := fn(node); err != nil {
			return err
		}
	}

	// Deleted-in-place entries. GetFullPathIndexableEntries returns only live
	// entries, but exFAT deletes a file by clearing its directory entry's in-use
	// bit while the entry (and usually its contiguous clusters) stay intact in an
	// allocated directory table — so the unallocated-cluster carver
	// (RecoverDeletedEntries) does not see them. GetAllEntries(indexable=false)
	// does. These carry a base name with a " (deleted)" suffix and no
	// reconstructable parent path; surface them as deleted nodes so their content
	// is still recovered (matching libtusk/TSK deleted-file recovery).
	root2, err := a.ex.ReadRootDir()
	if err != nil {
		return nil // best effort: the live walk already succeeded
	}
	all, err := a.ex.GetAllEntries(root2, false)
	if err != nil {
		return nil
	}
	for _, e := range all {
		if !e.IsDeleted() {
			continue
		}
		name := strings.TrimSuffix(e.GetName(), " (deleted)")
		node := core.Node{
			ID:        a.put(e),
			Name:      name,
			Path:      "/" + name,
			Kind:      entryKind(e),
			IsDeleted: true,
		}
		if err := fn(node); err != nil {
			return err
		}
	}
	return nil
}

func (a *exfatFS) Stat(n core.Node) (core.FileInfo, error) {
	e, ok := a.cache[n.ID]
	if !ok {
		return core.FileInfo{}, errUnknownNode
	}
	return core.FileInfo{
		Size:         int64(e.GetSize()),
		Kind:         n.Kind,
		IsDeleted:    e.IsDeleted(),
		IsFragmented: e.HasFatChain(),
		Inode:        n.ID,
		MACB: core.MACB{
			Modified: fsutil.TimePtr(e.GetModifiedTime()),
			Accessed: fsutil.TimePtr(e.GetAccessedTime()),
			Born:     fsutil.TimePtr(e.GetCreatedTime()),
		},
	}, nil
}

// DataRuns returns the entry's cluster chain coalesced into contiguous
// image-absolute byte ranges, trimmed to the file's logical size.
func (a *exfatFS) DataRuns(n core.Node) ([]core.AbsRange, error) {
	e, ok := a.cache[n.ID]
	if !ok {
		return nil, errUnknownNode
	}
	cs := int64(a.ex.GetClusterSize())
	clusters, _, err := a.ex.GetClusterList(e)
	if err != nil || len(clusters) == 0 {
		// A deleted entry's FAT chain is often unreadable (or absent — exFAT sets
		// NoFatChain for contiguous files). Fall back to a contiguous run from the
		// first cluster, sized to the file, which is how the data was laid out and
		// what TSK reports for such files. Live files always have a chain, so this
		// only affects recovered/deleted entries.
		clusters = contiguousClusters(e, cs)
	}
	if len(clusters) == 0 {
		return nil, nil
	}

	var out []core.AbsRange
	flush := func(first uint32, count uint32) {
		start := a.base + int64(a.ex.GetClusterOffset(first))
		length := int64(count) * cs
		out = append(out, core.AbsRange{Start: start, End: start + length - 1})
	}

	runStart, runLen := clusters[0], uint32(1)
	for i := 1; i < len(clusters); i++ {
		if clusters[i] == runStart+runLen {
			runLen++
			continue
		}
		flush(runStart, runLen)
		runStart, runLen = clusters[i], 1
	}
	flush(runStart, runLen)

	return fsutil.TrimToSize(out, int64(e.GetSize())), nil
}

func (a *exfatFS) Open(n core.Node) (io.ReadCloser, error) {
	e, ok := a.cache[n.ID]
	if !ok {
		return nil, errUnknownNode
	}
	clusters, _, err := a.ex.GetClusterList(e)
	if err != nil {
		return nil, err
	}
	size := int64(e.GetSize())
	buf := make([]byte, size)
	cs := int64(a.ex.GetClusterSize())

	var written int64
	for _, c := range clusters {
		if written >= size {
			break
		}
		off := int64(a.ex.GetClusterOffset(c))
		want := cs
		if written+want > size {
			want = size - written
		}
		if _, err := a.r.ReadAt(buf[written:written+want], off); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		written += want
	}
	return io.NopCloser(bytes.NewReader(buf)), nil
}

func (a *exfatFS) Streams(_ core.Node) ([]core.Stream, error) { return nil, nil }

// contiguousClusters returns the cluster numbers a file of e's size would occupy
// if laid out contiguously from its first cluster. Used as a fallback for
// deleted entries whose FAT chain is no longer resolvable.
func contiguousClusters(e libxfat.Entry, clusterSize int64) []uint32 {
	size := int64(e.GetSize())
	first := e.GetEntryCluster()
	if size <= 0 || clusterSize <= 0 || first == 0 {
		return nil
	}
	n := (size + clusterSize - 1) / clusterSize
	out := make([]uint32, 0, n)
	for i := int64(0); i < n; i++ {
		out = append(out, first+uint32(i))
	}
	return out
}

func entryKind(e libxfat.Entry) core.NodeKind {
	if e.IsDir() {
		return core.KindDir
	}
	return core.KindFile
}
