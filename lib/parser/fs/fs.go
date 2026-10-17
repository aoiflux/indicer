// Package fs detects and opens filesystems, dispatching to the per-format
// adapters in its sub-packages (ntfs, xfs, exfat, fat, ext, hfs).
package fs

import (
	"errors"
	"fmt"
	"io"

	"indicer/internal/safe"
	"indicer/lib/parser/core"
	"indicer/lib/parser/fs/exfat"
	"indicer/lib/parser/fs/ext"
	"indicer/lib/parser/fs/fat"
	"indicer/lib/parser/fs/hfs"
	"indicer/lib/parser/fs/ntfs"
	"indicer/lib/parser/fs/xfs"
)

// ErrUnknownFS is returned when no adapter recognises the filesystem.
var ErrUnknownFS = errors.New("fs: unrecognised filesystem")

// openers builds each filesystem's adapter.
var openers = map[core.FSType]func(core.OpenParams) (core.Filesystem, error){
	core.FSNTFS:  ntfs.Open,
	core.FSXFS:   xfs.Open,
	core.FSExFAT: exfat.Open,
	core.FSFAT:   fat.Open,
	core.FSExt:   ext.Open,
	core.FSHFS:   hfs.Open,
}

// Supported reports whether an adapter exists for fsType.
func Supported(fsType core.FSType) bool {
	_, ok := openers[fsType]
	return ok
}

// Open mounts fsType from p. The result is guarded: a panic in the backing
// library becomes an error on the call that raised it.
func Open(fsType core.FSType, p core.OpenParams) (fsys core.Filesystem, err error) {
	o, ok := openers[fsType]
	if !ok {
		return nil, ErrUnknownFS
	}
	p.Limits = p.Limits.WithDefaults()
	if p.ImageSize == 0 {
		p.ImageSize = p.Base + p.Size
	}
	// Mounting parses the boot sector and root structures: untrusted input.
	fsys, err = safe.Value(safe.Site{Component: "parser/" + string(fsType), Op: "mount", FSType: string(fsType), Offset: p.Base},
		func() (core.Filesystem, error) { return o(p) })
	if err != nil {
		return nil, err
	}
	return core.Guard(fsys), nil
}

// Detection is what the first bytes of a volume say it is.
type Detection struct {
	// Type is the filesystem an adapter can open, or FSUnknown.
	Type core.FSType
	// Status is "" for a supported Type. Otherwise it says why the volume
	// will not be walked: PartEncrypted, PartUnsupported (recognised, no
	// adapter) or PartUnknown (nothing recognised).
	Status core.PartitionStatus
	// Detail names what was recognised, e.g. "BitLocker" or "btrfs".
	Detail string
}

// probe windows: the boot area, and the btrfs superblock at 64 KiB.
const (
	headLen  = 8 << 10
	btrfsOff = 0x10040
)

// DetectErr sniffs the volume at the start of r. A read error is returned
// as an error (PAR-16): a device that cannot be read is not an unknown
// filesystem. A volume shorter than the probe windows is fine.
func DetectErr(r io.ReaderAt) (Detection, error) {
	head, err := readProbe(r, 0, headLen)
	if err != nil {
		return Detection{}, err
	}
	if t := detectSupported(head); t != core.FSUnknown {
		return Detection{Type: t}, nil
	}
	switch {
	case hasAt(head, 3, "-FVE-FS-"):
		return Detection{Type: core.FSUnknown, Status: core.PartEncrypted, Detail: "BitLocker"}, nil
	case hasAt(head, 0, "LUKS\xba\xbe"):
		return Detection{Type: core.FSUnknown, Status: core.PartEncrypted, Detail: "LUKS"}, nil
	case hasAt(head, 32, "NXSB"):
		return Detection{Type: core.FSUnknown, Status: core.PartUnsupported, Detail: "APFS"}, nil
	case hasAt(head, 3, "ReFS\x00\x00\x00\x00"):
		return Detection{Type: core.FSUnknown, Status: core.PartUnsupported, Detail: "ReFS"}, nil
	case isLVM2(head):
		return Detection{Type: core.FSUnknown, Status: core.PartUnsupported, Detail: "LVM2"}, nil
	case hasAt(head, 4096-10, "SWAPSPACE2"), hasAt(head, 4096-10, "SWAP-SPACE"):
		return Detection{Type: core.FSUnknown, Status: core.PartUnsupported, Detail: "swap"}, nil
	}
	btrfs, err := readProbe(r, btrfsOff, 8)
	if err != nil {
		return Detection{}, err
	}
	if hasAt(btrfs, 0, "_BHRfS_M") {
		return Detection{Type: core.FSUnknown, Status: core.PartUnsupported, Detail: "btrfs"}, nil
	}
	return Detection{Type: core.FSUnknown, Status: core.PartUnknown}, nil
}

// Detect sniffs the filesystem type, returning FSUnknown when nothing an
// adapter supports matches or the volume cannot be read.
func Detect(r io.ReaderAt) core.FSType {
	d, err := DetectErr(r)
	if err != nil {
		return core.FSUnknown
	}
	return d.Type
}

// readProbe reads up to n bytes at off; a short read at the end of the
// volume is not an error.
func readProbe(r io.ReaderAt, off int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	k, err := r.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("fs: read volume header at %d: %w", off, err)
	}
	return buf[:k], nil
}

func detectSupported(buf []byte) core.FSType {
	switch {
	case hasAt(buf, 3, "NTFS    "): // boot-sector OEM id
		return core.FSNTFS
	case hasAt(buf, 3, "EXFAT   "):
		return core.FSExFAT
	case isFAT(buf, 0):
		return core.FSFAT
	case hasAt(buf, 0, "XFSB"): // XFS superblock magic
		return core.FSXFS
	case hasAt(buf, 1024, "H+"), hasAt(buf, 1024, "HX"): // HFS+/HFSX
		return core.FSHFS
	case hasAt(buf, 1080, "\x53\xef"): // ext s_magic 0xEF53 (LE) at 1024+56
		return core.FSExt
	case hasAt(buf, 1024, "BD"): // classic HFS, or an HFS wrapper around HFS+ (PAR-26)
		return core.FSHFS
	case isFAT(buf, 6*512) && !hasAt(buf, 510, "\x55\xaa"):
		// FAT32 keeps a backup boot sector at sector 6; libfat falls back
		// to it when sector 0 is gone (PAR-26).
		return core.FSFAT
	}
	return core.FSUnknown
}

func isFAT(buf []byte, at int) bool {
	return hasAt(buf, at+82, "FAT32   ") || hasAt(buf, at+54, "FAT12   ") ||
		hasAt(buf, at+54, "FAT16   ") || hasAt(buf, at+54, "FAT     ")
}

// isLVM2 looks for an LVM2 physical-volume label in the first four sectors.
func isLVM2(buf []byte) bool {
	for s := 0; s < 4; s++ {
		if hasAt(buf, s*512, "LABELONE") && hasAt(buf, s*512+24, "LVM2 001") {
			return true
		}
	}
	return false
}

func hasAt(buf []byte, off int, sig string) bool {
	if len(buf) < off+len(sig) {
		return false
	}
	return string(buf[off:off+len(sig)]) == sig
}

// OpenAuto detects the filesystem on p.Reader and opens it, returning
// ErrUnknownFS when nothing an adapter supports is recognised.
func OpenAuto(p core.OpenParams) (core.Filesystem, error) {
	d, err := DetectErr(p.Reader)
	if err != nil {
		return nil, err
	}
	if d.Type == core.FSUnknown {
		return nil, ErrUnknownFS
	}
	return Open(d.Type, p)
}
