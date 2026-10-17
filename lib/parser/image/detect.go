package image

import (
	"context"
	"io"

	"indicer/lib/parser/core"
)

// Open opens a disk image by path, auto-detecting the container format
// (raw/dd and split raw, EWF segment sets, VHD/VHDX) and returning it as a
// core.Image. The returned Image owns whatever handles it opened and releases
// them on Close. OpenSource returns the same image with its provenance.
func Open(path string) (core.Image, error) {
	src, err := OpenSource(context.Background(), path, Options{})
	if err != nil {
		return nil, err
	}
	return src.Image, nil
}

// DetectFormat sniffs the container format of an image from its header and,
// for fixed VHD, its trailing footer. It returns FormatRaw when nothing matches.
func DetectFormat(r io.ReaderAt, size int64) core.ImageFormat {
	head := make([]byte, 8)
	if _, err := r.ReadAt(head, 0); err == nil {
		switch {
		case isEWFMagic(head):
			return core.FormatEWF
		case string(head) == "vhdxfile": // VHDX
			return core.FormatVHD
		case string(head) == "conectix": // dynamic/differencing VHD footer copy at offset 0
			return core.FormatVHD
		}
	}

	// Fixed VHD carries its 512-byte footer only at the end of the file.
	if size >= 512 {
		foot := make([]byte, 8)
		if _, err := r.ReadAt(foot, size-512); err == nil && string(foot) == "conectix" {
			return core.FormatVHD
		}
	}

	return core.FormatRaw
}

// isEWFMagic reports whether b begins with an EWF v1 (E01/L01) or v2 (Ex01/Lx01)
// signature.
func isEWFMagic(b []byte) bool {
	if len(b) >= 4 {
		switch string(b[:4]) {
		case "EVF2", "LEF2": // EWF v2
			return true
		}
	}
	if len(b) >= 4 {
		switch string(b[:3]) {
		case "EVF", "LVF": // EWF v1, followed by 0x09
			return b[3] == 0x09
		}
	}
	return false
}
