package compression_product

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"indicer/lib/fsx"
)

const (
	// splitSidecarExt names the file that describes a split archive's parts:
	// <archive>.split.json beside <archive>.001, <archive>.002, ...
	splitSidecarExt     = ".split.json"
	splitSidecarVersion = 1
)

// splitPart is one part of a split archive.
type splitPart struct {
	Name   string `json:"name"` // base name, beside the sidecar
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// splitSidecar describes a split archive, so a reader can tell a complete,
// intact set from a damaged one or a mix of two.
type splitSidecar struct {
	Version int         `json:"version"`
	Archive string      `json:"archive"` // base name of the whole archive
	Size    int64       `json:"size"`
	SHA256  string      `json:"sha256"`
	Parts   []splitPart `json:"parts"`
}

func partPath(archivePath string, i int) string {
	return fmt.Sprintf("%s"+archiveSplitSuffixFmt, archivePath, i)
}

// archiveSink is where archive writers write. Tests replace it to inject
// write failures.
var archiveSink = func(f *os.File) io.Writer { return f }

// writeArchiveOutput writes an archive through fill. With partSize 0 it
// replaces outputPath atomically; otherwise it writes the archive as parts
// of partSize bytes and a sidecar. Either way, whatever an earlier archive
// of the same name left (an unsplit file, parts, a sidecar) is replaced, and
// a failure leaves it as it was.
func writeArchiveOutput(ctx context.Context, outputPath string, partSize int64, fill func(w io.Writer) error) ([]string, error) {
	if partSize <= 0 {
		err := fsx.WriteAtomic(outputPath, defaultFilePerm, fsx.Options{}, func(f *os.File) error {
			return fill(archiveSink(f))
		})
		if err != nil {
			return nil, err
		}
		removeSplitSet(outputPath, 1)
		return []string{outputPath}, nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(outputPath), "."+filepath.Base(outputPath)+".dues-tmp-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = fsx.Remove(tmpPath) }()
	err = fill(archiveSink(tmp))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	return writeSplit(ctx, tmpPath, outputPath, partSize)
}

// writeSplit cuts the archive at src into parts beside archivePath. Each part
// is written atomically; the sidecar is written last, so until it is, the
// old sidecar (if any) still describes the old parts and a reader refuses
// the mix. Then stale parts past the new last one, and an unsplit archive of
// the same name, are removed.
func writeSplit(ctx context.Context, src, archivePath string, partSize int64) ([]string, error) {
	in, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return nil, err
	}
	whole := sha256.New()
	r := io.TeeReader(in, whole)
	sc := splitSidecar{Version: splitSidecarVersion, Archive: filepath.Base(archivePath), Size: info.Size()}
	var paths []string
	for i := 1; ; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := min(partSize, info.Size()-sc.partsSize())
		if n <= 0 && i > 1 {
			break
		}
		p := partPath(archivePath, i)
		sum := sha256.New()
		if err := fsx.WriteAtomic(p, defaultFilePerm, fsx.Options{}, func(f *os.File) error {
			_, err := io.CopyN(io.MultiWriter(archiveSink(f), sum), r, n)
			return err
		}); err != nil {
			return nil, err
		}
		sc.Parts = append(sc.Parts, splitPart{Name: filepath.Base(p), Size: n, SHA256: hex.EncodeToString(sum.Sum(nil))})
		paths = append(paths, p)
		if sc.partsSize() >= info.Size() {
			break
		}
	}
	sc.SHA256 = hex.EncodeToString(whole.Sum(nil))
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := fsx.WriteFileAtomic(archivePath+splitSidecarExt, data, defaultFilePerm); err != nil {
		return nil, err
	}
	removeParts(archivePath, len(paths)+1)
	_ = fsx.Remove(archivePath)
	return paths, nil
}

func (sc splitSidecar) partsSize() int64 {
	var n int64
	for _, p := range sc.Parts {
		n += p.Size
	}
	return n
}

// removeSplitSet removes a split set's sidecar and its parts from number
// from on.
func removeSplitSet(archivePath string, from int) {
	_ = fsx.Remove(archivePath + splitSidecarExt)
	removeParts(archivePath, from)
}

// removeParts removes archivePath.NNN for NNN from on, up to the first gap.
func removeParts(archivePath string, from int) {
	for i := from; i < 1000; i++ {
		p := partPath(archivePath, i)
		if _, err := os.Lstat(p); err != nil {
			return
		}
		_ = fsx.Remove(p)
	}
}

var partSuffix = regexp.MustCompile(`^(.*)\.(\d{3})$`)

// splitBase returns the archive a split part or sidecar path names, and
// whether path is one. name.NNN is a part only when a sidecar for name
// exists, or when name has an archive extension (a split archive written
// before sidecars existed); image.001 alone is a file, not a part.
func splitBase(path string) (string, bool) {
	if base, ok := cutSuffixFold(path, splitSidecarExt); ok {
		return base, true
	}
	m := partSuffix.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	base := m[1]
	if _, err := os.Stat(base + splitSidecarExt); err == nil {
		return base, true
	}
	if detectArchiveFormatFromPath(base) != "" {
		return base, true
	}
	return "", false
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) >= len(suffix) && equalFold(s[len(s)-len(suffix):], suffix) {
		return s[:len(s)-len(suffix)], true
	}
	return "", false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// archiveInput is an archive opened for reading: one file, or the parts of
// a split archive read in place.
type archiveInput struct {
	path    string // the archive (for a split set, the name its parts share)
	parts   []string
	sidecar *splitSidecar
	files   []*os.File
	offsets []int64 // offsets[i] is where files[i] starts
	size    int64
}

// openArchiveInput opens path: a file, a split archive's part or sidecar, or
// the name of a split archive whose sidecar exists.
func openArchiveInput(path string) (*archiveInput, error) {
	base, split := splitBase(path)
	if !split {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			if _, serr := os.Stat(path + splitSidecarExt); serr == nil {
				base, split = path, true
			}
		}
	}
	if !split {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if info.IsDir() {
			_ = f.Close()
			return nil, fmt.Errorf("%s is a directory", path)
		}
		return &archiveInput{path: path, files: []*os.File{f}, offsets: []int64{0}, size: info.Size()}, nil
	}

	a := &archiveInput{path: base}
	if data, err := os.ReadFile(base + splitSidecarExt); err == nil {
		var sc splitSidecar
		if err := json.Unmarshal(data, &sc); err != nil || sc.Version != splitSidecarVersion || len(sc.Parts) == 0 {
			return nil, &damagedError{fmt.Errorf("split archive sidecar %s is not valid", base+splitSidecarExt)}
		}
		a.sidecar = &sc
		dir := filepath.Dir(base)
		for _, p := range sc.Parts {
			if p.Name != filepath.Base(p.Name) {
				return nil, &damagedError{fmt.Errorf("split archive sidecar names a part outside its directory: %q", p.Name)}
			}
			a.parts = append(a.parts, filepath.Join(dir, p.Name))
		}
	} else {
		for i := 1; i < 1000; i++ {
			p := partPath(base, i)
			if _, err := os.Stat(p); err != nil {
				break
			}
			a.parts = append(a.parts, p)
		}
		if len(a.parts) == 0 {
			return nil, fmt.Errorf("%s: no parts found", path)
		}
	}
	for i, p := range a.parts {
		f, err := os.Open(p)
		if err != nil {
			a.Close()
			if errors.Is(err, fs.ErrNotExist) {
				return nil, &damagedError{fmt.Errorf("split archive part %s is missing", filepath.Base(p))}
			}
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			a.Close()
			return nil, err
		}
		if a.sidecar != nil && info.Size() != a.sidecar.Parts[i].Size {
			_ = f.Close()
			a.Close()
			return nil, &damagedError{fmt.Errorf("split archive part %s is %d bytes, its sidecar says %d", filepath.Base(p), info.Size(), a.sidecar.Parts[i].Size)}
		}
		a.files = append(a.files, f)
		a.offsets = append(a.offsets, a.size)
		a.size += info.Size()
	}
	return a, nil
}

// split reports whether the archive is a split set.
func (a *archiveInput) split() bool { return a.parts != nil }

// verified reports whether a sidecar describes the parts.
func (a *archiveInput) verified() bool { return a.sidecar != nil }

// verify reads every part and checks it against the sidecar's SHA-256.
func (a *archiveInput) verify(ctx context.Context) error {
	if a.sidecar == nil {
		return nil
	}
	for i, f := range a.files {
		sum := sha256.New()
		if _, err := copyWithProgress(ctx, sum, io.NewSectionReader(f, 0, a.sidecar.Parts[i].Size), nil); err != nil {
			return err
		}
		if hex.EncodeToString(sum.Sum(nil)) != a.sidecar.Parts[i].SHA256 {
			return &damagedError{fmt.Errorf("split archive part %s does not match its sidecar", a.sidecar.Parts[i].Name)}
		}
	}
	return nil
}

// ReadAt reads across the parts as one file.
func (a *archiveInput) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	n := 0
	for len(p) > 0 {
		if off >= a.size {
			return n, io.EOF
		}
		i := sort.Search(len(a.offsets), func(i int) bool { return a.offsets[i] > off }) - 1
		end := a.size
		if i+1 < len(a.offsets) {
			end = a.offsets[i+1]
		}
		chunk := p[:min(int64(len(p)), end-off)]
		m, err := a.files[i].ReadAt(chunk, off-a.offsets[i])
		n += m
		off += int64(m)
		p = p[m:]
		if err != nil && !(err == io.EOF && m == len(chunk)) {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
	}
	return n, nil
}

func (a *archiveInput) Close() error {
	var first error
	for _, f := range a.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	a.files = nil
	return first
}
