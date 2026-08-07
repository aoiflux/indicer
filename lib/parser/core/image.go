package core

import "io"

// ImageFormat identifies a disk-image container format.
type ImageFormat string

const (
	FormatRaw     ImageFormat = "raw" // dd / img / .001 split
	FormatEWF     ImageFormat = "ewf" // E01 / Ex01 / L01 / Lx01
	FormatVHD     ImageFormat = "vhd" // VHD / VHDX
	FormatUnknown ImageFormat = "unknown"
)

// Image is a decoded, contiguous logical device. A filesystem or partition
// parser reads through it via ReadAt without caring whether the bytes come from a
// raw dd file, a decoded E01, a virtual disk, or a partition sub-range.
//
// Offsets passed to ReadAt are logical offsets within the decoded device
// [0, Size()). Implementations must be safe for concurrent use, matching the
// io.ReaderAt contract.
type Image interface {
	io.ReaderAt

	// Size is the logical size of the decoded device in bytes.
	Size() int64

	// Format reports the container format the image was decoded from.
	Format() ImageFormat

	// Close releases any resources (open files, caches) held by the image.
	Close() error
}
