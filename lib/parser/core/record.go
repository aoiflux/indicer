package core

import "io"

// FileRecord is the per-node result of a scan, as the ingest pipeline
// receives it.
//
// Content is authoritative: where the bytes are, how sure the parser is, and
// what went wrong. Fragments and IsFragmented are the legacy single-list form
// of the same content (data extents as image ranges, holes as sparse ranges),
// kept for the libtusk-shaped JSON consumer until W6 stores Content directly.
type FileRecord struct {
	Name      string // raw name
	Path      string // escaped full path
	Stream    string // the alternate data stream the record stands for, if any
	Kind      NodeKind
	Size      int64
	IsDeleted bool
	Content   Content
	Prov      Prov // node and content provenance combined
	MACB      MACB
	Inode     uint64
	Seq       uint32
	Parent    uint64
	Target    string // symlink target
	Links     int
	Streams   []Stream
	Issues    []Issue // problems with this node: stat, content, streams

	// Open streams the node's logical bytes through the filesystem's own
	// reader (decoded content included). It is valid only while the
	// visitor call that received the record runs, and must not be called
	// concurrently with the walk.
	Open func() (io.ReadCloser, error) `json:"-"`

	IsFragmented bool       // legacy: more than one range
	Fragments    []AbsRange // legacy: Content.Ranges()

	// Provenance within the evidence being parsed.
	PartitionIndex int // owning partition index, or -1 for a whole-device filesystem
	FSType         FSType
}

// ContentLen returns the total number of bytes covered by the record's
// fragments. For a well-formed non-sparse file this equals Size.
func (r FileRecord) ContentLen() int64 {
	var n int64
	for _, f := range r.Fragments {
		n += f.Len()
	}
	return n
}
