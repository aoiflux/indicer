package core

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// ExtentKind says what backs a range of a file.
type ExtentKind uint8

const (
	// ExtentData is backed by image bytes at ImageOffset.
	ExtentData ExtentKind = iota
	// ExtentSparse is a hole: no bytes are allocated and it reads as zeros.
	ExtentSparse
	// ExtentUnwritten is allocated but never written (an ext4/XFS
	// preallocation, or NTFS data past the valid data length). It reads as
	// zeros; the blocks on disk hold whatever was there before.
	ExtentUnwritten
)

func (k ExtentKind) String() string {
	switch k {
	case ExtentData:
		return "data"
	case ExtentSparse:
		return "sparse"
	case ExtentUnwritten:
		return "unwritten"
	}
	return fmt.Sprintf("extent(%d)", uint8(k))
}

// Extent is one range of a file's logical content. ImageOffset is absolute in
// the image and is meaningful only for ExtentData.
type Extent struct {
	FileOffset  int64
	Length      int64
	ImageOffset int64
	Kind        ExtentKind
}

// FileEnd returns the logical offset just past the extent.
func (e Extent) FileEnd() int64 { return e.FileOffset + e.Length }

// ContentKind says how a file's content is reached.
type ContentKind uint8

const (
	// ContentNone: the node has no content (empty file, directory).
	ContentNone ContentKind = iota
	// ContentExtents: the content is the extents, read from the image.
	ContentExtents
	// ContentInline: the content is stored in filesystem metadata and carried
	// in Content.Inline (NTFS resident data, ext4 inline data).
	ContentInline
	// ContentDecoded: the content exists only after the filesystem decodes it
	// (NTFS LZNT1/WOF compression, HFS+ decmpfs) and is read through
	// Filesystem.Open.
	ContentDecoded
	// ContentUnavailable: the filesystem holds content DUES cannot locate or
	// decode; Issues says why.
	ContentUnavailable
)

func (k ContentKind) String() string {
	switch k {
	case ContentNone:
		return "none"
	case ContentExtents:
		return "extents"
	case ContentInline:
		return "inline"
	case ContentDecoded:
		return "decoded"
	case ContentUnavailable:
		return "unavailable"
	}
	return fmt.Sprintf("content(%d)", uint8(k))
}

// Prov is a set of provenance flags: how much a record can be trusted and why.
type Prov uint32

const (
	ProvDeleted                 Prov = 1 << iota // the entry is deleted
	ProvRecovered                                // found by a recovery sweep, not the live tree
	ProvReallocated                              // its clusters or record now belong to another file
	ProvAssumedContiguous                        // content assumed contiguous from the first cluster
	ProvChecksumMismatch                         // a directory-entry or name checksum did not match
	ProvSynthName                                // the name was synthesised; the original is lost
	ProvTruncated                                // less content was located than the size claims
	ProvOutOfBounds                              // an extent pointed outside the partition or image
	ProvStaleEntry                               // a directory entry names a record reused since
	ProvBrokenChain                              // the allocation chain ends early or is invalid
	ProvChainLoop                                // the allocation chain loops
	ProvAllocationContradiction                  // the allocation map disagrees with the entry
	ProvEncrypted                                // the content is encrypted (e.g. NTFS EFS)
	ProvCompressed                               // the content is compressed by the filesystem
	ProvFSMetadata                               // a filesystem metadata file ($MFT, $FAT, ...)
	ProvHardLink                                 // one of several names for the same content
	ProvOrphan                                   // no reachable parent directory
	ProvNameSanitized                            // the path was escaped; the raw name is kept
	ProvBeyondValidData                          // data past the valid data length reads as zeros
)

var provNames = []string{
	"deleted", "recovered", "reallocated", "assumed_contiguous", "checksum_mismatch",
	"synth_name", "truncated", "out_of_bounds", "stale_entry", "broken_chain",
	"chain_loop", "allocation_contradiction", "encrypted", "compressed",
	"fs_metadata", "hard_link", "orphan", "name_sanitized", "beyond_valid_data",
}

// Has reports whether every flag in f is set.
func (p Prov) Has(f Prov) bool { return p&f == f }

// String renders the set flags joined by "|".
func (p Prov) String() string {
	var parts []string
	for i, name := range provNames {
		if p&(1<<i) != 0 {
			parts = append(parts, name)
		}
	}
	if rest := p &^ (1<<len(provNames) - 1); rest != 0 {
		parts = append(parts, fmt.Sprintf("0x%x", uint32(rest)))
	}
	return strings.Join(parts, "|")
}

// Confidence grades how the content was located.
type Confidence uint8

const (
	// ConfExact: from live allocation metadata.
	ConfExact Confidence = iota
	// ConfDeclared: from metadata that survives deletion but is no longer
	// authoritative (a deleted entry's own extents).
	ConfDeclared
	// ConfAssumed: inferred, e.g. a deleted FAT file assumed contiguous.
	ConfAssumed
	// ConfUnknown: nothing could be located.
	ConfUnknown
)

func (c Confidence) String() string {
	switch c {
	case ConfExact:
		return "exact"
	case ConfDeclared:
		return "declared"
	case ConfAssumed:
		return "assumed"
	case ConfUnknown:
		return "unknown"
	}
	return fmt.Sprintf("confidence(%d)", uint8(c))
}

// Content is where a file's logical bytes are.
//
// Invariants, which ExtentContent and Validate maintain: Extents are sorted,
// do not overlap, and tile [0, Located) exactly; Located ≤ Size; DataBytes
// and HoleBytes sum to Located. Content never pads a file with zeros it did
// not find: bytes past Located are unknown, not zero.
type Content struct {
	Kind       ContentKind
	Size       int64 // logical size the filesystem records
	Located    int64 // bytes [0, Located) are described
	DataBytes  int64 // bytes backed by image data
	HoleBytes  int64 // bytes that read as zeros (sparse or unwritten)
	Extents    []Extent
	Inline     []byte
	Prov       Prov
	Confidence Confidence
	Issues     []Issue
}

// ErrBadExtent is returned for extents that cannot describe a file: negative
// or overflowing ranges, an unknown kind, or extents that overlap.
var ErrBadExtent = errors.New("core: bad extent")

// NormalizeExtents returns in sorted by file offset, cut to size (dropping
// block slack past the end), with interior gaps filled by sparse holes and
// adjacent compatible extents merged. It never adds a hole after the last
// extent: whether a short tail is a hole depends on the filesystem, so the
// adapter appends one when it is. The input is not modified.
func NormalizeExtents(in []Extent, size int64) ([]Extent, error) {
	if size < 0 {
		return nil, fmt.Errorf("%w: negative size %d", ErrBadExtent, size)
	}
	ext := make([]Extent, 0, len(in))
	for _, e := range in {
		if err := checkExtent(e); err != nil {
			return nil, err
		}
		if e.Length == 0 || e.FileOffset >= size {
			continue
		}
		if e.FileEnd() > size {
			e.Length = size - e.FileOffset
		}
		ext = append(ext, e)
	}
	slices.SortStableFunc(ext, func(a, b Extent) int {
		switch {
		case a.FileOffset < b.FileOffset:
			return -1
		case a.FileOffset > b.FileOffset:
			return 1
		}
		return 0
	})

	var out []Extent
	var pos int64
	for _, e := range ext {
		if e.FileOffset < pos {
			return nil, fmt.Errorf("%w: extent at %d overlaps the one ending at %d", ErrBadExtent, e.FileOffset, pos)
		}
		if e.FileOffset > pos {
			out = appendExtent(out, Extent{FileOffset: pos, Length: e.FileOffset - pos, Kind: ExtentSparse})
		}
		out = appendExtent(out, e)
		pos = e.FileEnd()
	}
	return out, nil
}

func checkExtent(e Extent) error {
	switch {
	case e.Kind > ExtentUnwritten:
		return fmt.Errorf("%w: unknown kind %d", ErrBadExtent, e.Kind)
	case e.FileOffset < 0 || e.Length < 0:
		return fmt.Errorf("%w: negative range %d+%d", ErrBadExtent, e.FileOffset, e.Length)
	case e.FileOffset > math.MaxInt64-e.Length:
		return fmt.Errorf("%w: file range %d+%d overflows", ErrBadExtent, e.FileOffset, e.Length)
	case e.Kind == ExtentData && e.ImageOffset < 0:
		return fmt.Errorf("%w: negative image offset %d", ErrBadExtent, e.ImageOffset)
	case e.Kind == ExtentData && e.ImageOffset > math.MaxInt64-e.Length:
		return fmt.Errorf("%w: image range %d+%d overflows", ErrBadExtent, e.ImageOffset, e.Length)
	}
	return nil
}

// appendExtent appends e, merging it into the last extent when both are the
// same kind and, for data, contiguous on the image.
func appendExtent(out []Extent, e Extent) []Extent {
	if e.Kind != ExtentData {
		e.ImageOffset = 0
	}
	if n := len(out); n > 0 {
		last := &out[n-1]
		if last.Kind == e.Kind && last.FileEnd() == e.FileOffset &&
			(e.Kind != ExtentData || last.ImageOffset+last.Length == e.ImageOffset) {
			last.Length += e.Length
			return out
		}
	}
	return append(out, e)
}

// ExtentContent normalises extents for a file of size and returns them as
// Content with its totals computed. A file whose extents stop short of size
// is flagged ProvTruncated.
func ExtentContent(size int64, extents []Extent) (Content, error) {
	norm, err := NormalizeExtents(extents, size)
	if err != nil {
		return Content{}, err
	}
	c := Content{Size: size, Extents: norm}
	c.total()
	return c, nil
}

// InlineContent returns content stored in filesystem metadata.
func InlineContent(size int64, data []byte) Content {
	if int64(len(data)) > size {
		data = data[:size]
	}
	c := Content{Kind: ContentInline, Size: size, Inline: data, Located: int64(len(data)), DataBytes: int64(len(data))}
	if c.Located < size {
		c.Prov |= ProvTruncated
	}
	if size == 0 {
		c.Kind = ContentNone
	}
	return c
}

// total recomputes Kind, Located, DataBytes and HoleBytes from Extents.
func (c *Content) total() {
	c.Located, c.DataBytes, c.HoleBytes = 0, 0, 0
	for _, e := range c.Extents {
		if e.Kind == ExtentData {
			c.DataBytes += e.Length
		} else {
			c.HoleBytes += e.Length
		}
		c.Located = e.FileEnd()
	}
	switch {
	case c.Size == 0:
		c.Kind = ContentNone
	case len(c.Extents) > 0:
		c.Kind = ContentExtents
	case c.Kind == ContentNone || c.Kind == ContentExtents:
		c.Kind = ContentUnavailable
	}
	if c.Located < c.Size {
		c.Prov |= ProvTruncated
	}
}

// Bounds are where an extent's image range may lie: inside its partition and
// inside the image.
type Bounds struct {
	PartStart int64
	PartEnd   int64 // exclusive
	ImageSize int64
}

// Validate cuts the content at the first data extent that lies outside b,
// flagging ProvOutOfBounds and ProvTruncated. Everything before it is kept, so
// a file with one corrupt run still yields the part that can be trusted.
func (c *Content) Validate(b Bounds) {
	if c.Kind != ContentExtents {
		return
	}
	limit := min(b.PartEnd, b.ImageSize)
	for i, e := range c.Extents {
		if e.Kind != ExtentData {
			continue
		}
		if e.ImageOffset < b.PartStart || e.ImageOffset > limit || e.Length > limit-e.ImageOffset {
			c.Extents = c.Extents[:i]
			c.Prov |= ProvOutOfBounds | ProvTruncated
			c.Issues = append(c.Issues, Issue{
				Code:   IssueOutOfBounds,
				Detail: fmt.Sprintf("extent at file offset %d maps to image [%d, +%d) outside [%d, %d)", e.FileOffset, e.ImageOffset, e.Length, b.PartStart, limit),
			})
			c.total()
			return
		}
	}
}

// Cap keeps at most max extents, flagging the content truncated, and
// reports whether it cut anything.
func (c *Content) Cap(max int) bool {
	if max <= 0 || len(c.Extents) <= max {
		return false
	}
	c.Extents = c.Extents[:max:max]
	c.Issues = append(c.Issues, Issue{Code: IssueTooManyExtents, Detail: fmt.Sprintf("kept the first %d extents", max)})
	c.total()
	return true
}

// UnwrittenFrom returns ext with every data byte at or past valid turned
// into ExtentUnwritten: the bytes past a file's valid data length (exFAT
// ValidDataLength, NTFS InitializedSize) read as zeros whatever the clusters
// hold. ext must be normalised; the result is too.
func UnwrittenFrom(ext []Extent, valid int64) []Extent {
	if valid < 0 {
		valid = 0
	}
	var out []Extent
	for _, e := range ext {
		switch {
		case e.Kind != ExtentData || e.FileEnd() <= valid:
			out = appendExtent(out, e)
		case e.FileOffset >= valid:
			out = appendExtent(out, Extent{FileOffset: e.FileOffset, Length: e.Length, Kind: ExtentUnwritten})
		default:
			head := valid - e.FileOffset
			out = appendExtent(out, Extent{FileOffset: e.FileOffset, Length: head, ImageOffset: e.ImageOffset, Kind: ExtentData})
			out = appendExtent(out, Extent{FileOffset: valid, Length: e.Length - head, Kind: ExtentUnwritten})
		}
	}
	return out
}

// MarkUnwritten applies UnwrittenFrom to c and recomputes its totals,
// flagging ProvBeyondValidData when anything changed.
func (c *Content) MarkUnwritten(valid int64) {
	if c.Kind != ContentExtents || valid >= c.Located {
		return
	}
	c.Extents = UnwrittenFrom(c.Extents, valid)
	c.Prov |= ProvBeyondValidData
	c.total()
}
