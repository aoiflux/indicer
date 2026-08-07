// Package fs detects and opens filesystems, dispatching to the per-format
// adapters in its sub-packages (ntfs, xfs, exfat, fat, ext, hfs).
package fs

import (
	"errors"
	"io"

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

// OpenFunc mounts a filesystem from a partition-relative reader r. base is the
// partition's absolute offset within the image; size is the partition length.
type OpenFunc = func(r io.ReaderAt, base, size int64) (core.Filesystem, error)

// openers maps a detected filesystem type to its adapter constructor.
var openers = map[core.FSType]OpenFunc{
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

// Open mounts the given filesystem type from r.
func Open(fsType core.FSType, r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	open, ok := openers[fsType]
	if !ok {
		return nil, ErrUnknownFS
	}
	return open(r, base, size)
}

// OpenAuto detects the filesystem at the start of r and opens it, returning
// ErrUnknownFS if nothing is recognised.
func OpenAuto(r io.ReaderAt, base, size int64) (core.Filesystem, error) {
	fsType := Detect(r)
	if fsType == core.FSUnknown {
		return nil, ErrUnknownFS
	}
	return Open(fsType, r, base, size)
}

// Detect sniffs the filesystem type from r's boot sector / superblock magic. It
// returns core.FSUnknown when nothing matches. r is read at partition-relative
// offset 0.
func Detect(r io.ReaderAt) core.FSType {
	buf := make([]byte, 2048)
	n, _ := r.ReadAt(buf, 0)
	buf = buf[:n]

	switch {
	case hasAt(buf, 3, "NTFS    "): // boot-sector OEM id
		return core.FSNTFS
	case hasAt(buf, 3, "EXFAT   "):
		return core.FSExFAT
	case hasAt(buf, 82, "FAT32   "): // FAT32 BS_FilSysType
		return core.FSFAT
	case hasAt(buf, 54, "FAT12   "), hasAt(buf, 54, "FAT16   "), hasAt(buf, 54, "FAT     "):
		return core.FSFAT
	case hasAt(buf, 0, "XFSB"): // XFS superblock magic
		return core.FSXFS
	case len(buf) >= 1026 && buf[1024] == 'H' && (buf[1025] == '+' || buf[1025] == 'X'): // HFS+/HFSX
		return core.FSHFS
	case len(buf) >= 1082 && buf[1080] == 0x53 && buf[1081] == 0xEF: // ext s_magic 0xEF53 (LE) at 1024+56
		return core.FSExt
	}
	return core.FSUnknown
}

func hasAt(buf []byte, off int, sig string) bool {
	if len(buf) < off+len(sig) {
		return false
	}
	return string(buf[off:off+len(sig)]) == sig
}
