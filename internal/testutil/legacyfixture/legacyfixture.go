// Package legacyfixture serves the legacy database fixtures: DUES databases
// written by a past release and checked in, so tests can prove that later code
// still opens, lists and restores them.
//
// testdata holds one <mode>.tar.zst per storage mode and a golden.json that
// records, for each mode, the flags the database was written with, the inputs
// and their digests, the `dues list` output, and what restoring each object
// returned at generation time. scripts/fixtures/gen_legacy_dbs.sh regenerates
// them from a given commit.
//
// The package imports no DUES packages, so any test can use it without
// creating an import cycle.
package legacyfixture

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Input is one file that was stored into a fixture database.
type Input struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Restore records what restoring one listed object returned when the fixture
// was generated. Status is "ok" or "error".
type Restore struct {
	Kind   string  `json:"kind"` // evidence, partition or indexed
	Hash   string  `json:"hash"`
	Name   string  `json:"name"`
	Status string  `json:"status"`
	SHA256 *string `json:"sha256"`
	Size   *int64  `json:"size"`
	Error  *string `json:"error"`
}

// VolumeFile is the ground truth for one file inside the FAT volume input.
type VolumeFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Fixture describes one legacy database.
type Fixture struct {
	Mode        string                `json:"mode"`
	Commit      string                `json:"commit"`
	Flags       []string              `json:"flags"`
	Password    *string               `json:"password"`
	Archive     string                `json:"archive"`
	Inputs      []Input               `json:"inputs"`
	StoreErrors *string               `json:"store_errors"`
	StoreLogs   map[string][]string   `json:"store_logs"`
	VolumeFiles map[string]VolumeFile `json:"volume_files"`
	List        json.RawMessage       `json:"list"`
	ListError   *string               `json:"list_error"`
	Restores    []Restore             `json:"restores"`
}

// ReadFlags returns the global flags the current DUES needs to read this
// database: password, quick, container, hierarchical and chunk size.
// Store-only options such as --hash-algo are left out. The releases that
// wrote the fixtures took --password; the current one reads the password
// from a file, written in a test directory.
func (f Fixture) ReadFlags(tb testing.TB) []string {
	tb.Helper()
	var out []string
	for i := 0; i < len(f.Flags); i++ {
		switch f.Flags[i] {
		case "--password":
			if i+1 < len(f.Flags) {
				if pw := f.Flags[i+1]; pw != "" {
					file := filepath.Join(tb.TempDir(), "password")
					if err := os.WriteFile(file, []byte(pw+"\n"), 0o600); err != nil {
						tb.Fatal(err)
					}
					out = append(out, "--password-file", file)
				}
				i++
			}
		case "--chonksize":
			if i+1 < len(f.Flags) {
				out = append(out, f.Flags[i], f.Flags[i+1])
				i++
			}
		case "--quick", "--container", "--hierarchical":
			out = append(out, f.Flags[i])
		}
	}
	return out
}

// Expected returns the digest and size a correct restore of r must produce:
// the stored input for evidence, the whole input for a whole-device
// partition, and the volume ground truth for an indexed file.
func (f Fixture) Expected(r Restore) (VolumeFile, bool) {
	switch r.Kind {
	case "evidence":
		for _, in := range f.Inputs {
			if in.Name == r.Name {
				return VolumeFile{SHA256: in.SHA256, Size: in.Size}, true
			}
		}
	case "partition":
		// The fixture volumes have no partition table, so the one partition
		// is the whole device, named "<input>_p0".
		base := strings.TrimSuffix(r.Name, "_p0")
		for _, in := range f.Inputs {
			if in.Name == base {
				return VolumeFile{SHA256: in.SHA256, Size: in.Size}, true
			}
		}
	case "indexed":
		v, ok := f.VolumeFiles[r.Name]
		return v, ok
	}
	return VolumeFile{}, false
}

type golden struct {
	Schema   int       `json:"schema"`
	Fixtures []Fixture `json:"fixtures"`
}

// Dir returns the directory holding the fixture archives and golden.json.
func Dir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "testdata"
	}
	return filepath.Join(filepath.Dir(file), "testdata")
}

// Load returns every fixture described by golden.json.
func Load(tb testing.TB) []Fixture {
	tb.Helper()
	fixtures, err := LoadFrom(Dir())
	if err != nil {
		tb.Fatalf("legacyfixture: %v", err)
	}
	return fixtures
}

// LoadFrom reads golden.json from dir.
func LoadFrom(dir string) ([]Fixture, error) {
	b, err := os.ReadFile(filepath.Join(dir, "golden.json"))
	if err != nil {
		return nil, fmt.Errorf("read golden: %w", err)
	}
	var g golden
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("parse golden: %w", err)
	}
	if g.Schema != 1 {
		return nil, fmt.Errorf("golden schema %d, want 1", g.Schema)
	}
	if len(g.Fixtures) == 0 {
		return nil, errors.New("golden lists no fixtures")
	}
	return g.Fixtures, nil
}

// Extract unpacks the fixture's database into a fresh temporary directory and
// returns the database path to pass as --dbpath.
func Extract(tb testing.TB, f Fixture) string {
	tb.Helper()
	root := tb.TempDir()
	if err := ExtractTo(filepath.Join(Dir(), f.Archive), root); err != nil {
		tb.Fatalf("legacyfixture: extract %s: %v", f.Mode, err)
	}
	return filepath.Join(root, "data")
}

// maxExtractBytes bounds what one archive may expand to. The fixtures are a
// few megabytes; the bound only stops a corrupted archive from filling a disk.
const maxExtractBytes = 1 << 30

// ExtractTo unpacks a .tar.zst archive into dst. Entries must be regular files
// or directories with local, relative names.
func ExtractTo(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f, zstd.WithDecoderMaxMemory(maxExtractBytes))
	if err != nil {
		return err
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
		if name == "" || name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("entry %q escapes the destination", hdr.Name)
		}
		target := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if hdr.Size < 0 || total > maxExtractBytes {
				return fmt.Errorf("archive expands past %d bytes", int64(maxExtractBytes))
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeEntry(target, tr, hdr.Size); err != nil {
				return err
			}
		default:
			return fmt.Errorf("entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

func writeEntry(target string, r io.Reader, size int64) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(out, r, size); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
