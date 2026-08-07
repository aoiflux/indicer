package core

import (
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
)

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
type Node struct {
	ID        uint64
	Name      string
	Path      string
	Kind      NodeKind
	IsDeleted bool
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
}

// Capabilities reports which optional behaviours a Filesystem adapter supports,
// so callers can degrade honestly rather than silently returning empty data.
type Capabilities struct {
	MACB      bool // per-file timestamps
	Deleted   bool // deleted-entry enumeration
	Fragments bool // exported absolute data-run offsets
	Streams   bool // ADS / resource forks / xattrs
}

// Filesystem is the unified, read-only interface every filesystem adapter
// implements. It normalises the divergent backing-library APIs behind one shape.
type Filesystem interface {
	// Type reports the filesystem type.
	Type() FSType

	// Capabilities reports which optional behaviours this adapter supports.
	Capabilities() Capabilities

	// Root returns the root directory node.
	Root() (Node, error)

	// Walk visits every node in the tree, including deleted entries where the
	// filesystem and its backing library support recovery (see Capabilities).
	// Returning a non-nil error from fn stops the walk and is returned.
	Walk(fn func(Node) error) error

	// Stat resolves a node's metadata.
	Stat(Node) (FileInfo, error)

	// DataRuns returns the node's content as absolute Image byte ranges — the
	// libtusk fragments[] contract. It returns an empty slice for zero-length or
	// resident content (content stored inline in filesystem metadata).
	DataRuns(Node) ([]AbsRange, error)

	// Open assembles and returns the node's content for sequential reading.
	Open(Node) (io.ReadCloser, error)

	// Streams returns any named data streams attached to the node.
	Streams(Node) ([]Stream, error)

	// Close releases resources held by the filesystem.
	Close() error
}
