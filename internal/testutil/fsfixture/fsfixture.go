// Package fsfixture loads the filesystem fixture images in
// lib/parser/testdata/fixtures and their ground-truth manifests.
//
// Images are stored zstd-compressed; Open decompresses them once per test
// binary into a cache directory and hands out the path. The manifests record
// what is on each volume (paths, sizes, content digests, expected properties)
// and nothing about where it is, so a test compares the parser against the
// truth rather than against the parser's own earlier output.
//
// lib/parser/testdata/gen/gen_fixtures.sh regenerates both.
package fsfixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Stream is a named data stream of a file (an NTFS alternate data stream).
type Stream struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// File is one entry the volume holds. Kind is "file", "dir" or "symlink".
type File struct {
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Size    int64    `json:"size"`
	SHA256  string   `json:"sha256"`
	Target  string   `json:"target"`
	Extents int      `json:"extents"`
	Flags   []string `json:"flags"`
	Links   []string `json:"links"`
	Streams []Stream `json:"streams"`

	// Deleted entries only.
	Deleted     bool   `json:"deleted"`
	Recoverable string `json:"recoverable"` // full, partial or none
}

// HasFlag reports whether the ground truth marks the file with flag
// (fragmented, sparse, unwritten, inline).
func (f File) HasFlag(flag string) bool { return slices.Contains(f.Flags, flag) }

// Partition is one filesystem inside the image, at Offset bytes.
type Partition struct {
	Offset  int64  `json:"offset"`
	FS      string `json:"fs"`
	Files   []File `json:"files"`
	Deleted []File `json:"deleted"`
}

// Manifest is the ground truth for one fixture image.
type Manifest struct {
	Schema     int         `json:"schema"`
	Image      string      `json:"image"`
	Segments   []string    `json:"segments"`
	Format     string      `json:"format"` // raw, ewf or vhd
	Size       int64       `json:"size"`   // decoded disk size
	SHA256     string      `json:"sha256"` // decoded disk digest
	Generator  string      `json:"generator"`
	Partitions []Partition `json:"partitions"`

	// Name is "<group>/<base>", e.g. "ntfs/ntfs_fsagen"; set by All.
	Name string `json:"-"`
}

// Dir returns lib/parser/testdata/fixtures.
func Dir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return filepath.Join("testdata", "fixtures")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "lib", "parser", "testdata", "fixtures")
}

// All loads every manifest, sorted by name.
func All(tb testing.TB) []Manifest {
	tb.Helper()
	paths, err := filepath.Glob(filepath.Join(Dir(), "*", "*.manifest.json"))
	if err != nil {
		tb.Fatal(err)
	}
	if len(paths) == 0 {
		tb.Fatalf("no fixture manifests under %s", Dir())
	}
	sort.Strings(paths)
	var out []Manifest
	for _, p := range paths {
		m, err := load(p)
		if err != nil {
			tb.Fatalf("fixture %s: %v", p, err)
		}
		out = append(out, m)
	}
	return out
}

func load(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, err
	}
	if m.Schema != 1 {
		return Manifest{}, fmt.Errorf("schema %d, want 1", m.Schema)
	}
	if len(m.Segments) == 0 {
		m.Segments = []string{m.Image}
	}
	group := filepath.Base(filepath.Dir(path))
	m.Name = group + "/" + strings.TrimSuffix(filepath.Base(path), ".manifest.json")
	return m, nil
}

var (
	cacheMu   sync.Mutex
	cacheDir  string
	extracted = map[string]string{}
)

// Open decompresses the fixture's image (every segment, for a split set)
// into a per-process cache and returns the path of the first segment. The
// files must be treated as read-only; they are shared between tests.
func Open(tb testing.TB, m Manifest) string {
	tb.Helper()
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if p, ok := extracted[m.Name]; ok {
		return p
	}
	if cacheDir == "" {
		d, err := os.MkdirTemp("", "dues-parser-fixtures-")
		if err != nil {
			tb.Fatal(err)
		}
		cacheDir = d
	}
	group := filepath.Dir(filepath.FromSlash(m.Name))
	dst := filepath.Join(cacheDir, filepath.FromSlash(m.Name))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		tb.Fatal(err)
	}
	for _, seg := range m.Segments {
		src := filepath.Join(Dir(), group, seg+".zst")
		if err := decompress(src, filepath.Join(dst, seg)); err != nil {
			tb.Fatalf("fixture %s: %v", m.Name, err)
		}
	}
	p := filepath.Join(dst, m.Segments[0])
	extracted[m.Name] = p
	return p
}

// Cleanup removes the decompressed cache. Call it from TestMain after the
// tests ran; leaving it costs only temp space.
func Cleanup() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheDir != "" {
		_ = os.RemoveAll(cacheDir)
	}
	cacheDir = ""
	extracted = map[string]string{}
}

// maxImage bounds a decompressed fixture so a corrupted archive cannot fill
// the disk; the largest fixture is 64 MiB.
const maxImage = 256 << 20

func decompress(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := zstd.NewReader(in)
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(zr, maxImage+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxImage {
		err = errors.New("decompressed image exceeds the fixture size limit")
	}
	return err
}
