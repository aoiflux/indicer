package image

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"indicer/lib/parser/core"
)

// Open opens a disk image by path, auto-detecting the container format
// (raw/dd, EWF, VHD/VHDX) and returning it as a core.Image. The returned Image
// owns whatever handles it opened and releases them on Close.
//
// For multi-segment sets (EWF .E01/.E02…, split raw .001/.002…) pass the segment
// paths to OpenEWFSegments / OpenRawSegments directly; Open handles the first (or
// only) segment given.
func Open(path string) (core.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	switch DetectFormat(f, info.Size()) {
	case core.FormatEWF:
		// EWF images are commonly split across .E01/.E02/… segments; the closing
		// "done" section lives in the last segment, so opening only the first
		// yields "incomplete segment set". Discover the sibling segments and open
		// the whole set.
		_ = f.Close()
		segs := ewfSegmentPaths(path)
		if len(segs) <= 1 {
			return OpenEWFFile(path)
		}
		return OpenEWFSegments(segs)

	case core.FormatVHD:
		// libvhdi.OpenFile reopens by path so it can resolve differencing parents
		// from the image's directory.
		_ = f.Close()
		return OpenVHDFile(path)

	default:
		return &rawImage{ra: f, size: info.Size(), closer: f}, nil
	}
}

// ewfSegmentPaths, given the path of one EWF segment, returns all segments of
// the set in order (.E01, .E02, …). It matches sibling files with the same stem
// and a same-length extension in the same EWF family letter (e/l), then sorts
// case-insensitively — which orders the fixed-width EWF extension sequence
// (E01<…<E99<EAA<…) correctly. If nothing else matches it returns just first.
func ewfSegmentPaths(first string) []string {
	base := filepath.Base(first)
	ext := filepath.Ext(base) // ".e01" / ".Ex01" / ".L01"
	if len(ext) < 3 {
		return []string{first}
	}
	stem := strings.TrimSuffix(base, ext)
	dir := filepath.Dir(first)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{first}
	}
	var segs []string
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		e := filepath.Ext(name)
		if len(e) != len(ext) {
			continue
		}
		if !strings.EqualFold(strings.TrimSuffix(name, e), stem) {
			continue
		}
		if !strings.EqualFold(e[:2], ext[:2]) { // same "." + family letter (e/l)
			continue
		}
		segs = append(segs, filepath.Join(dir, name))
	}
	if len(segs) == 0 {
		return []string{first}
	}
	sort.Slice(segs, func(i, j int) bool {
		return strings.ToLower(filepath.Base(segs[i])) < strings.ToLower(filepath.Base(segs[j]))
	})
	return segs
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
