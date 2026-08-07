package image

import (
	"io"
	"os"

	"github.com/aoiflux/libewf"

	"indicer/lib/parser/core"
)

// ewfImage adapts a libewf.Reader (E01/Ex01/L01/Lx01) to core.Image.
type ewfImage struct {
	r      libewf.Reader
	closer io.Closer // underlying source(s); libewf.Reader.Close does not close them
}

func (im *ewfImage) ReadAt(p []byte, off int64) (int, error) { return im.r.ReadAt(p, off) }
func (im *ewfImage) Size() int64                             { return im.r.Size() }
func (im *ewfImage) Format() core.ImageFormat                { return core.FormatEWF }

func (im *ewfImage) Close() error {
	err := im.r.Close()
	if im.closer != nil {
		if cerr := im.closer.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// OpenEWF wraps an already-open io.ReaderAt EWF source. The caller retains
// ownership of r; Close only releases libewf's own resources.
func OpenEWF(r io.ReaderAt) (core.Image, error) {
	rd, err := libewf.Open(r)
	if err != nil {
		return nil, err
	}
	return &ewfImage{r: rd}, nil
}

// OpenEWFFile opens a single-segment EWF image (.E01/.Ex01/.L01/.Lx01) by path.
func OpenEWFFile(path string) (core.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	rd, err := libewf.Open(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &ewfImage{r: rd, closer: f}, nil
}

// OpenEWFSegments opens a multi-segment EWF image (.E01, .E02, … given in order)
// and presents it as one contiguous core.Image.
func OpenEWFSegments(paths []string) (core.Image, error) {
	files := make([]*os.File, 0, len(paths))
	sources := make([]io.ReaderAt, 0, len(paths))
	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			closeAll()
			return nil, err
		}
		files = append(files, f)
		sources = append(sources, f)
	}
	rd, err := libewf.OpenSegments(sources)
	if err != nil {
		closeAll()
		return nil, err
	}
	return &ewfImage{r: rd, closer: multiCloser(files)}, nil
}
