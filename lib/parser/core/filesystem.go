package core

import (
	"context"
	"io"
	"time"
)

// FSType identifies a filesystem type.
type FSType string

const (
	FSFAT     FSType = "fat"
	FSExFAT   FSType = "exfat"
	FSNTFS    FSType = "ntfs"
	FSExt     FSType = "ext"
	FSHFS     FSType = "hfs"
	FSXFS     FSType = "xfs"
	FSUnknown FSType = "unknown"
)

// NodeKind classifies a filesystem entry.
type NodeKind int

const (
	KindFile NodeKind = iota
	KindDir
	KindOther
	KindSymlink
)

func (k NodeKind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindDir:
		return "dir"
	case KindSymlink:
		return "symlink"
	}
	return "other"
}

// AbsRange is an absolute, inclusive byte range within the Image — the
// behavioural equivalent of libtusk's fragments[]. A file's content is the
// concatenation of its AbsRanges in order. Sparse marks a hole (zero-filled,
// no backing bytes).
type AbsRange struct {
	Start  int64 // absolute, inclusive
	End    int64 // absolute, inclusive
	Sparse bool
}

// Len returns the number of bytes covered by the range.
func (r AbsRange) Len() int64 { return r.End - r.Start + 1 }

// MACB carries a file's timestamps. A nil pointer means the filesystem (or its
// parsing library) does not surface that particular timestamp.
//
//	Modified — content last-modified (mtime)
//	Accessed — last-accessed (atime)
//	Changed  — metadata last-changed (ctime / MFTChangeTime); nil where absent (e.g. FAT)
//	Born     — creation / birth time (crtime); nil where absent
type MACB struct {
	Modified *time.Time
	Accessed *time.Time
	Changed  *time.Time
	Born     *time.Time
}

// Node identifies a filesystem entry. ID is the filesystem-native identifier
// (inode number, MFT record number, HFS CNID, or a synthesised id for
// cluster-based filesystems) and is stable within a single Filesystem.
//
// Seq is the record's sequence or generation number where the filesystem has
// one, so a deleted entry can be checked against the record it names. Ref is
// the adapter's own handle for the node (a directory entry, an exFAT entry
// set); the adapter fills it during Walk and reads it back in Stat and
// Content, so it keeps no cache of the tree. Callers treat Ref as opaque.
type Node struct {
	ID        uint64
	Seq       uint32
	Parent    uint64
	Name      string // raw name as the filesystem stores it
	Path      string // full path, escaped where the name needed it
	Kind      NodeKind
	IsDeleted bool
	Prov      Prov
	Ref       any
}

// Stream is a named data stream attached to a node: an NTFS alternate data
// stream, an HFS resource fork, or an extended attribute.
type Stream struct {
	Name string
	Kind string // "ads" | "resource_fork" | "xattr"
	Size int64
}

// FileInfo is the resolved metadata for a node.
type FileInfo struct {
	Size         int64
	Kind         NodeKind
	IsDeleted    bool
	IsFragmented bool
	MACB         MACB
	Inode        uint64
	Target       string // symlink target, when Kind is KindSymlink
	Links        int    // hard-link count, 0 when unknown
}

// Capabilities reports which optional behaviours a Filesystem adapter supports,
// so callers can degrade honestly rather than silently returning empty data.
type Capabilities struct {
	MACB      bool // per-file timestamps
	Deleted   bool // deleted-entry enumeration
	Fragments bool // exported absolute data-run offsets
	Streams   bool // ADS / resource forks / xattrs
}

// Filesystem is the read-only interface every filesystem adapter implements.
//
// Walk visits nodes in preorder. Content says where a node's logical bytes
// are (image extents, inline data, or "decoded: read it through Open"); Open
// streams the logical bytes themselves. Problems that belong to no single
// node (an unreadable directory, a cycle, a cap) go to the Sink the adapter
// was opened with; problems with one node come back from that node's call.
type Filesystem interface {
	Type() FSType
	Capabilities() Capabilities

	// Walk calls fn for every node, live and (where supported) deleted. An
	// error from fn stops the walk and is returned; so does ctx.
	Walk(ctx context.Context, fn func(Node) error) error

	// Stat resolves a node's metadata.
	Stat(Node) (FileInfo, error)

	// Content locates a node's logical content. Data extents are absolute in
	// the image and already normalised (see NormalizeExtents).
	Content(Node) (Content, error)

	// Open streams a node's logical content. For ContentExtents and
	// ContentInline it yields the same bytes Content describes.
	Open(Node) (io.ReadCloser, error)

	// Streams lists named data streams (ADS, resource forks, xattrs).
	Streams(Node) ([]Stream, error)

	Close() error
}

// Limits bound what one filesystem may cost to walk. Zero fields take the
// defaults in DefaultLimits.
type Limits struct {
	MaxDepth   int   // directory depth below the root
	MaxEntries int64 // nodes per filesystem
	MaxExtents int   // extents per file
	MaxInline  int   // bytes of inline content carried in Content
	MaxName    int   // bytes of one name
}

// DefaultLimits are generous enough for real volumes and small enough that a
// crafted one cannot exhaust memory.
var DefaultLimits = Limits{
	MaxDepth:   1024,
	MaxEntries: 64 << 20,
	MaxExtents: 1 << 20,
	MaxInline:  16 << 20,
	MaxName:    4096,
}

// WithDefaults fills zero fields from DefaultLimits.
func (l Limits) WithDefaults() Limits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultLimits.MaxDepth
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultLimits.MaxEntries
	}
	if l.MaxExtents <= 0 {
		l.MaxExtents = DefaultLimits.MaxExtents
	}
	if l.MaxInline <= 0 {
		l.MaxInline = DefaultLimits.MaxInline
	}
	if l.MaxName <= 0 {
		l.MaxName = DefaultLimits.MaxName
	}
	return l
}

// OpenParams is what an adapter is built from. Reader is the partition alone
// (offset 0 is the partition's first byte), so each library opens with a base
// offset of 0 and the adapter adds Base exactly once, when it reports image
// offsets.
type OpenParams struct {
	Reader    io.ReaderAt
	Base      int64 // partition offset in the image
	Size      int64 // partition length
	ImageSize int64
	Sink      Sink
	Limits    Limits
}

// Bounds returns where the partition's extents may point.
func (p OpenParams) Bounds() Bounds {
	return Bounds{PartStart: p.Base, PartEnd: p.Base + p.Size, ImageSize: p.ImageSize}
}

// Report sends i to the sink, if there is one.
func (p OpenParams) Report(i Issue) {
	if p.Sink != nil {
		p.Sink(i)
	}
}
