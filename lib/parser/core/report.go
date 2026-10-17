package core

import (
	"fmt"
	"slices"
)

// IssueCode classifies something a parser could not do, or did with less
// certainty than usual.
type IssueCode string

const (
	IssuePanicRecovered IssueCode = "panic_recovered" // a library call panicked
	IssueReadError      IssueCode = "read_error"      // the source could not be read
	IssueStatError      IssueCode = "stat_error"      // a node's metadata could not be resolved
	IssueContentError   IssueCode = "content_error"   // a node's content could not be located
	IssueStreamsError   IssueCode = "streams_error"   // a node's streams could not be listed
	IssueUnreadableDir  IssueCode = "unreadable_dir"  // a directory could not be listed
	IssuePartialDir     IssueCode = "partial_dir"     // a directory was only partly listed
	IssueCycle          IssueCode = "cycle"           // a directory was reached twice
	IssueDepthCap       IssueCode = "depth_cap"       // the walk hit its depth limit
	IssueEntryBudget    IssueCode = "entry_budget"    // the walk hit its entry limit
	IssueNameSanitized  IssueCode = "name_sanitized"  // a name was escaped for the path
	IssueOutOfBounds    IssueCode = "out_of_bounds"   // an extent left the partition or image
	IssueTooManyExtents IssueCode = "too_many_extents"
	IssueBadExtent      IssueCode = "bad_extent"  // extents overlapped or overflowed
	IssueUnsupported    IssueCode = "unsupported" // a feature the adapter cannot read
	IssueLibrary        IssueCode = "library"     // a warning the backing library reported
	IssueOverlap        IssueCode = "partition_overlap"
	IssueClamped        IssueCode = "partition_clamped" // a partition ran past the image
	IssueTable          IssueCode = "partition_table"   // a partition-table warning
)

// Issue is one problem, with enough detail to find it again.
type Issue struct {
	Code   IssueCode
	Path   string
	Inode  uint64
	Detail string
}

func (i Issue) String() string {
	s := string(i.Code)
	if i.Path != "" {
		s += " " + i.Path
	}
	if i.Inode != 0 {
		s += fmt.Sprintf(" (inode %d)", i.Inode)
	}
	if i.Detail != "" {
		s += ": " + i.Detail
	}
	return s
}

// Sink receives issues found while a filesystem is open, outside any one
// node's Content: unreadable directories, cycles, caps, library warnings.
type Sink func(Issue)

// Discard is a Sink that drops everything.
func Discard(Issue) {}

// maxSamples bounds the example issues kept per code.
const maxSamples = 8

// IndexReport summarises one partition's scan. Counters are exact; Warnings
// counts every issue by code and keeps the first few of each as samples, so a
// volume with a million bad entries still yields a small report.
type IndexReport struct {
	Partition PartitionInfo

	FilesSeen   int64 // file records emitted
	DirsSeen    int64
	OtherSeen   int64 // symlinks and special files
	Deleted     int64
	Fragmented  int64 // more than one data extent
	Sparse      int64 // at least one hole
	Inline      int64
	Decoded     int64
	Unavailable int64
	Truncated   int64 // located less than the size
	BytesData   int64

	Warnings map[IssueCode]int64
	Samples  map[IssueCode][]Issue
}

// Add counts issue i.
func (r *IndexReport) Add(i Issue) {
	if r.Warnings == nil {
		r.Warnings = map[IssueCode]int64{}
		r.Samples = map[IssueCode][]Issue{}
	}
	r.Warnings[i.Code]++
	if len(r.Samples[i.Code]) < maxSamples {
		r.Samples[i.Code] = append(r.Samples[i.Code], i)
	}
}

// Codes returns the issue codes seen, sorted.
func (r *IndexReport) Codes() []IssueCode {
	out := make([]IssueCode, 0, len(r.Warnings))
	for c := range r.Warnings {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// PartitionStatus is what became of one partition-table entry.
type PartitionStatus string

const (
	PartIndexed     PartitionStatus = "indexed"     // a filesystem was walked
	PartPartial     PartitionStatus = "partial"     // the walk stopped early (panic or cap)
	PartUnknown     PartitionStatus = "unknown"     // no filesystem was recognised
	PartUnsupported PartitionStatus = "unsupported" // recognised, but DUES cannot read it
	PartEncrypted   PartitionStatus = "encrypted"   // a recognised encrypted volume
	PartReadError   PartitionStatus = "read_error"  // the source could not be read
	PartOpenError   PartitionStatus = "open_error"  // the filesystem failed to mount
)

// PartitionInfo describes one partition as the scan saw it.
type PartitionInfo struct {
	Partition
	FSType FSType
	Status PartitionStatus
	Detail string // e.g. "btrfs", "BitLocker", or the mount error
}

// ImageReport summarises a whole scan.
type ImageReport struct {
	Format     ImageFormat
	Size       int64
	Scheme     PartitionScheme
	Partitions []IndexReport
	Warnings   []Issue // image-level: table warnings, overlaps, clamping
}
