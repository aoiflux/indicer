package image

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/types"

	"indicer/lib/parser/core"
)

// vhdImage adapts a libvhdi virtual disk (VHD/VHDX) to core.Image.
type vhdImage struct {
	d *libvhdi.Disk
	r io.ReaderAt // d, or d behind sectorAlignedReader for differencing VHDX
}

func newVHDImage(d *libvhdi.Disk) *vhdImage {
	im := &vhdImage{d: d, r: d}
	if d.IsDifferencing() && d.Format() == types.FileFormatVHDX {
		sector := int64(d.SectorSize())
		if sector <= 0 {
			sector = types.DefaultSectorSize
		}
		im.r = sectorAlignedReader{r: d, sector: sector}
	}
	return im
}

func (im *vhdImage) ReadAt(p []byte, off int64) (int, error) { return im.r.ReadAt(p, off) }
func (im *vhdImage) Size() int64                             { return int64(im.d.Size()) }
func (im *vhdImage) Format() core.ImageFormat                { return core.FormatVHD }
func (im *vhdImage) Close() error                            { return im.d.Close() }

// sectorAlignedReader makes every read start on a sector boundary.
//
// libvhdi v0.4.1 mis-serves a read that starts inside a sector of a VHDX
// PARTIALLY_PRESENT block: it returns bytes from the start of that sector, so
// an ext4 inode 256 bytes into a sector reads as its neighbour. Reads that
// start aligned are served correctly, including those ending mid-sector, and
// the differencing resolver keeps them aligned down the parent chain. So only
// the head of an unaligned read is fetched as a whole sector and copied out.
type sectorAlignedReader struct {
	r      io.ReaderAt
	sector int64
}

func (a sectorAlignedReader) ReadAt(p []byte, off int64) (int, error) {
	head := off % a.sector
	if head == 0 || len(p) == 0 {
		return a.r.ReadAt(p, off)
	}
	buf := make([]byte, a.sector)
	n, err := a.r.ReadAt(buf, off-head)
	if int64(n) <= head {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	got := copy(p, buf[head:n])
	if got == len(p) {
		return got, nil
	}
	if err != nil {
		return got, err
	}
	rest, err := a.r.ReadAt(p[got:], off+int64(got))
	return got + rest, err
}

// ChainDepth reports how many disks the opened chain holds, counting the
// image itself; 1 for a disk that is not differencing.
func (im *vhdImage) ChainDepth() int { return im.d.ChainDepth() }

// ChainComplete reports whether every parent in the chain was attached.
func (im *vhdImage) ChainComplete() bool { return im.d.ChainComplete() }

// maxVHDChainDepth bounds a differencing chain, counting the child.
const maxVHDChainDepth = 16

// OpenVHDFile opens a VHD or VHDX image by path, auto-detecting the format. A
// differencing disk's parents are looked up by base name in the image's own
// directory, and the open fails unless the whole chain resolves: reading a
// child without its parents would return zeros for every unchanged block.
func OpenVHDFile(path string) (core.Image, error) {
	d, err := libvhdi.OpenFileWith(path, &libvhdi.Options{
		ParentResolver:     siblingResolver{},
		RequireParentChain: true,
		MaxChainDepth:      maxVHDChainDepth,
	})
	if err != nil {
		return nil, err
	}
	return newVHDImage(d), nil
}

// OpenVHD wraps an already-open io.ReaderAt VHD/VHDX source (size derived from
// the reader). Parents are never resolved on this path, so a differencing disk
// is refused; use OpenVHDFile for those.
func OpenVHD(r io.ReaderAt) (core.Image, error) {
	d, err := libvhdi.Open(r, &libvhdi.Options{
		ParentResolver: libvhdi.NoParentResolution,
	})
	if err != nil {
		return nil, err
	}
	if d.NeedsParent() {
		d.Close()
		return nil, fmt.Errorf("%w: differencing disk %q opened without its path; open it by path to resolve the parent chain",
			libvhdi.ErrParentRequired, d.ParentFilename())
	}
	return newVHDImage(d), nil
}

// siblingResolver finds a differencing disk's parent in the child's own
// directory, by base name only. The paths a child records come from the
// evidence, so they are never opened as given: an absolute, UNC, volume or
// relative path contributes only its last element.
type siblingResolver struct{}

func (siblingResolver) ResolveParent(req libvhdi.ParentRequest) (libvhdi.ParentSource, error) {
	if req.ChildDir == "" {
		return libvhdi.ParentSource{}, fmt.Errorf("%w: %q: the child has no directory to search",
			libvhdi.ErrParentNotFound, req.ParentFilename)
	}
	entries, err := os.ReadDir(req.ChildDir)
	if err != nil {
		return libvhdi.ParentSource{}, fmt.Errorf("%w: %q: %v", libvhdi.ErrParentNotFound, req.ParentFilename, err)
	}
	for _, want := range parentBaseNames(req) {
		match := ""
		for _, e := range entries {
			if e.Name() == want {
				match = want
				break
			}
		}
		if match == "" {
			// A chain made on Windows may be examined on a case-sensitive
			// filesystem.
			for _, e := range entries {
				if strings.EqualFold(e.Name(), want) {
					match = e.Name()
					break
				}
			}
		}
		if match == "" {
			continue
		}
		if src, err := openSibling(req.ChildDir, match); err == nil {
			return src, nil
		}
	}
	return libvhdi.ParentSource{}, fmt.Errorf("%w: %q is not in %s",
		libvhdi.ErrParentNotFound, req.ParentFilename, req.ChildDir)
}

// parentBaseNames returns the distinct, safe base names among the parent
// filename and locator values, in order of preference.
func parentBaseNames(req libvhdi.ParentRequest) []string {
	var names []string
	seen := map[string]bool{}
	add := func(raw string) {
		base := path.Base(strings.ReplaceAll(raw, `\`, "/"))
		if raw == "" || base == "." || base == ".." || base == "/" ||
			strings.ContainsAny(base, ":\x00") || seen[base] {
			return
		}
		seen[base] = true
		names = append(names, base)
	}
	add(req.ParentFilename)
	for _, loc := range req.Locators {
		add(loc.Value)
	}
	return names
}

// openSibling opens name inside dir, refusing anything that is not a regular
// file or that resolves outside dir.
func openSibling(dir, name string) (libvhdi.ParentSource, error) {
	f, err := os.OpenInRoot(dir, name)
	if err != nil {
		return libvhdi.ParentSource{}, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return libvhdi.ParentSource{}, fmt.Errorf("%s is not a regular file", name)
	}
	return libvhdi.ParentSource{
		ReaderAt: f,
		Size:     info.Size(),
		Closer:   f,
		Name:     filepath.Join(dir, name),
		ModTime:  info.ModTime(),
	}, nil
}
