package image

import (
	"io"

	"github.com/aoiflux/libvhdi"

	"indicer/lib/parser/core"
)

// vhdImage adapts a libvhdi virtual disk (VHD/VHDX) to core.Image.
type vhdImage struct {
	d *libvhdi.Disk
}

func (im *vhdImage) ReadAt(p []byte, off int64) (int, error) { return im.d.ReadAt(p, off) }
func (im *vhdImage) Size() int64                             { return int64(im.d.Size()) }
func (im *vhdImage) Format() core.ImageFormat                { return core.FormatVHD }
func (im *vhdImage) Close() error                            { return im.d.Close() }

// OpenVHDFile opens a VHD or VHDX image by path, auto-detecting the format and
// resolving any differencing parent chain from the image's own directory.
func OpenVHDFile(path string) (core.Image, error) {
	d, err := libvhdi.OpenFile(path)
	if err != nil {
		return nil, err
	}
	return &vhdImage{d: d}, nil
}

// OpenVHD wraps an already-open io.ReaderAt VHD/VHDX source (size derived from
// the reader). Differencing parent chains are not auto-resolved on this path —
// use OpenVHDFile when parents must be resolved.
func OpenVHD(r io.ReaderAt) (core.Image, error) {
	d, err := libvhdi.Open(r, nil)
	if err != nil {
		return nil, err
	}
	return &vhdImage{d: d}, nil
}
