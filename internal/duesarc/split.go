// Package duesarc holds what DUES archives share: the output written whole
// or as split parts with a sidecar, an archive or a split set read in place,
// the error that marks damage to an archive, and the check that turns an
// entry name into a safe local path. The compression product's archives use
// it, and so do database packages.
package duesarc

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

	"indicer/internal/faultinject"
	"indicer/lib/fsx"
)

const (
	// SidecarExt names the file that describes a split archive's parts:
	// <archive>.split.json beside <archive>.001, <archive>.002, ...
	SidecarExt     = ".split.json"
	sidecarVersion = 1

	partSuffixFmt = ".%03d"
	filePerm      = 0o644
)

// Part is one part of a split archive.
type Part struct {
	Name   string `json:"name"` // base name, beside the sidecar
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Sidecar describes a split archive, so a reader can tell a complete,
// intact set from a damaged one or a mix of two.
type Sidecar struct {
	Version int    `json:"version"`
	Archive string `json:"archive"` // base name of the whole archive
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Parts   []Part `json:"parts"`
}

// PartPath is the path of a split archive's part number i.
func PartPath(archivePath string, i int) string {
	return fmt.Sprintf("%s"+partSuffixFmt, archivePath, i)
}

// Sink is where archive writers write. Tests replace it to inject write
// failures.
var Sink = func(f *os.File) io.Writer { return f }

// WriteOutput writes an archive through fill. With partSize 0 it replaces
// outputPath atomically; otherwise it writes the archive as parts of
// partSize bytes and a sidecar. Either way, whatever an earlier archive of
// the same name left (an unsplit file, parts, a sidecar) is replaced, and a
// failure leaves it as it was.
func WriteOutput(ctx context.Context, outputPath string, partSize int64, fill func(w io.Writer) error) ([]string, error) {
	if partSize <= 0 {
		err := fsx.WriteAtomic(outputPath, filePerm, fsx.Options{RemoveStale: true}, func(f *os.File) error {
			if err := fill(Sink(f)); err != nil {
				return err
			}
			return faultinject.Hit(faultinject.QwikpakArchiveBeforeClose)
		})
		if err != nil {
			return nil, err
		}
		removeSplitSet(outputPath, 1)
		return []string{outputPath}, nil
	}

	fsx.RemoveStaleTemps(outputPath)
	tmp, err := os.CreateTemp(filepath.Dir(outputPath), fsx.TempPattern(outputPath))
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = fsx.Remove(tmpPath) }()
	err = fill(Sink(tmp))
	if err == nil {
		err = faultinject.Hit(faultinject.QwikpakArchiveBeforeClose)
	}
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
	sc := Sidecar{Version: sidecarVersion, Archive: filepath.Base(archivePath), Size: info.Size()}
	var paths []string
	for i := 1; ; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := min(partSize, info.Size()-sc.partsSize())
		if n <= 0 && i > 1 {
			break
		}
		p := PartPath(archivePath, i)
		sum := sha256.New()
		if err := fsx.WriteAtomic(p, filePerm, fsx.Options{RemoveStale: true}, func(f *os.File) error {
			_, err := io.CopyN(io.MultiWriter(Sink(f), sum), r, n)
			return err
		}); err != nil {
			return nil, err
		}
		sc.Parts = append(sc.Parts, Part{Name: filepath.Base(p), Size: n, SHA256: hex.EncodeToString(sum.Sum(nil))})
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
	if err := fsx.WriteFileAtomic(archivePath+SidecarExt, data, filePerm); err != nil {
		return nil, err
	}
	removeParts(archivePath, len(paths)+1)
	_ = fsx.Remove(archivePath)
	return paths, nil
}

func (sc Sidecar) partsSize() int64 {
	var n int64
	for _, p := range sc.Parts {
		n += p.Size
	}
	return n
}

// removeSplitSet removes a split set's sidecar and its parts from number
// from on.
func removeSplitSet(archivePath string, from int) {
	_ = fsx.Remove(archivePath + SidecarExt)
	removeParts(archivePath, from)
}

// removeParts removes archivePath.NNN for NNN from on, up to the first gap.
func removeParts(archivePath string, from int) {
	for i := from; i < 1000; i++ {
		p := PartPath(archivePath, i)
		if _, err := os.Lstat(p); err != nil {
			return
		}
		_ = fsx.Remove(p)
	}
}

var partSuffix = regexp.MustCompile(`^(.*)\.(\d{3})$`)

// PartBase returns the name path would be a part of, when it ends in a
// part number (.NNN).
func PartBase(path string) (string, bool) {
	m := partSuffix.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// SplitBase returns the archive a split part or sidecar path names, and
// whether path is one. name.NNN is a part only when a sidecar for name
// exists, or when archiveName(name) says name is an archive's (a split
// archive written before sidecars existed); image.001 alone is a file, not
// a part.
func SplitBase(path string, archiveName func(string) bool) (string, bool) {
	if base, ok := CutSuffixFold(path, SidecarExt); ok {
		return base, true
	}
	base, ok := PartBase(path)
	if !ok {
		return "", false
	}
	if _, err := os.Stat(base + SidecarExt); err == nil {
		return base, true
	}
	if archiveName != nil && archiveName(base) {
		return base, true
	}
	return "", false
}

// CutSuffixFold is strings.CutSuffix ignoring ASCII case.
func CutSuffixFold(s, suffix string) (string, bool) {
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

// Input is an archive opened for reading: one file, or the parts of a split
// archive read in place.
type Input struct {
	path    string // the archive (for a split set, the name its parts share)
	parts   []string
	sidecar *Sidecar
	files   []*os.File
	offsets []int64 // offsets[i] is where files[i] starts
	size    int64
}

// OpenInput opens path: a file, a split archive's part or sidecar, or the
// name of a split archive whose sidecar exists. archiveName is as for
// SplitBase.
func OpenInput(path string, archiveName func(string) bool) (*Input, error) {
	base, split := SplitBase(path, archiveName)
	if !split {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			if _, serr := os.Stat(path + SidecarExt); serr == nil {
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
		return &Input{path: path, files: []*os.File{f}, offsets: []int64{0}, size: info.Size()}, nil
	}

	a := &Input{path: base}
	if data, err := os.ReadFile(base + SidecarExt); err == nil {
		var sc Sidecar
		if err := json.Unmarshal(data, &sc); err != nil || sc.Version != sidecarVersion || len(sc.Parts) == 0 {
			return nil, &DamagedError{Err: fmt.Errorf("split archive sidecar %s is not valid", base+SidecarExt)}
		}
		a.sidecar = &sc
		dir := filepath.Dir(base)
		for _, p := range sc.Parts {
			if p.Name != filepath.Base(p.Name) {
				return nil, &DamagedError{Err: fmt.Errorf("split archive sidecar names a part outside its directory: %q", p.Name)}
			}
			a.parts = append(a.parts, filepath.Join(dir, p.Name))
		}
	} else {
		for i := 1; i < 1000; i++ {
			p := PartPath(base, i)
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
				return nil, &DamagedError{Err: fmt.Errorf("split archive part %s is missing", filepath.Base(p))}
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
			return nil, &DamagedError{Err: fmt.Errorf("split archive part %s is %d bytes, its sidecar says %d", filepath.Base(p), info.Size(), a.sidecar.Parts[i].Size)}
		}
		a.files = append(a.files, f)
		a.offsets = append(a.offsets, a.size)
		a.size += info.Size()
	}
	return a, nil
}

// Path is the archive's path; for a split set, the name its parts share.
func (a *Input) Path() string { return a.path }

// Parts lists a split set's parts, or nothing for one file.
func (a *Input) Parts() []string { return a.parts }

// Size is the archive's size, all parts together.
func (a *Input) Size() int64 { return a.size }

// Split reports whether the archive is a split set.
func (a *Input) Split() bool { return a.parts != nil }

// Verified reports whether a sidecar describes the parts.
func (a *Input) Verified() bool { return a.sidecar != nil }

// Verify reads every part and checks it against the sidecar's SHA-256.
func (a *Input) Verify(ctx context.Context) error {
	if a.sidecar == nil {
		return nil
	}
	for i, f := range a.files {
		sum := sha256.New()
		if _, err := Copy(ctx, sum, io.NewSectionReader(f, 0, a.sidecar.Parts[i].Size), nil); err != nil {
			return err
		}
		if hex.EncodeToString(sum.Sum(nil)) != a.sidecar.Parts[i].SHA256 {
			return &DamagedError{Err: fmt.Errorf("split archive part %s does not match its sidecar", a.sidecar.Parts[i].Name)}
		}
	}
	return nil
}

// ReadAt reads across the parts as one file.
func (a *Input) ReadAt(p []byte, off int64) (int, error) {
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

// Close closes every part.
func (a *Input) Close() error {
	var first error
	for _, f := range a.files {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
	}
	a.files = nil
	return first
}
