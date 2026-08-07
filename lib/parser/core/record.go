package core

// FileRecord is the unified per-file result the high-level parser emits for the
// ingest pipeline. It is the superset of today's single-range
// registerIndexedRange call: multi-fragment content plus MACB timestamps and
// stream metadata that the legacy libtusk JSON could not carry.
//
// Fragments are absolute Image byte ranges; the ingest sink reads them directly
// and chunk-dedups the bytes.
type FileRecord struct {
	Name         string
	Path         string
	Kind         NodeKind
	Size         int64
	IsDeleted    bool
	IsFragmented bool
	Fragments    []AbsRange
	MACB         MACB
	Inode        uint64
	Streams      []Stream

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
