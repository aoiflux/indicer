package image

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoiflux/libvhdi"
	"github.com/aoiflux/libvhdi/types"
)

func writeFile(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func resolveName(t *testing.T, req libvhdi.ParentRequest) (string, error) {
	t.Helper()
	src, err := siblingResolver{}.ResolveParent(req)
	if err != nil {
		return "", err
	}
	if src.Closer != nil {
		defer src.Closer.Close()
	}
	return src.Name, nil
}

// TestSiblingResolverStaysInChildDir checks that a parent is found only by
// base name in the child's own directory, never through a directory the
// evidence names.
func TestSiblingResolverStaysInChildDir(t *testing.T) {
	t.Parallel()
	childDir, otherDir := t.TempDir(), t.TempDir()
	elsewhere := filepath.Join(otherDir, "parent.vhdx")
	writeFile(t, elsewhere, "not this one")
	req := libvhdi.ParentRequest{
		ChildDir:       childDir,
		ParentFilename: elsewhere,
		Locators: []types.ParentLocatorEntry{
			{Key: "relative_path", Value: `..\` + filepath.Base(otherDir) + `\parent.vhdx`},
			{Key: "absolute_win32_path", Value: elsewhere},
		},
	}
	if name, err := resolveName(t, req); err == nil {
		t.Fatalf("resolved %s although no sibling exists", name)
	}

	sibling := filepath.Join(childDir, "parent.vhdx")
	writeFile(t, sibling, "sibling")
	name, err := resolveName(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if name != sibling {
		t.Fatalf("resolved %s, want %s", name, sibling)
	}
}

func TestSiblingResolverMatchesCaseInsensitively(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := filepath.Join(dir, "Parent.VHDX")
	writeFile(t, want, "x")
	name, err := resolveName(t, libvhdi.ParentRequest{ChildDir: dir, ParentFilename: `C:\vms\parent.vhdx`})
	if err != nil {
		t.Fatal(err)
	}
	if name != want {
		t.Fatalf("resolved %s, want %s", name, want)
	}
}

func TestSiblingResolverRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x"), "x")
	if err := os.Mkdir(filepath.Join(dir, "sub.vhdx"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", `..\`, "C:", "x\x00y", "x:stream", "sub.vhdx"} {
		if got, err := resolveName(t, libvhdi.ParentRequest{ChildDir: dir, ParentFilename: name}); err == nil {
			t.Errorf("name %q resolved to %s", name, got)
		}
	}
	if got, err := resolveName(t, libvhdi.ParentRequest{ParentFilename: "x"}); err == nil {
		t.Errorf("request without a child directory resolved to %s", got)
	}
}

// Corpus tests use the small Hyper-V chain drive.vhdx <- drive_CBE…avhdx and
// the archlight leaf whose grandparent is missing.
const (
	corpusChild      = "drive_CBE30548-2F9B-453E-8D84-30FCC5F25B31.avhdx"
	corpusBrokenLeaf = "archlight_850835C4-2B45-44C9-B110-0264C9E6BED4.avhdx"
)

func TestOpenVHDFileResolvesSiblingChain(t *testing.T) {
	t.Parallel()
	dir := corpusVHD.Require(t)
	im, err := OpenVHDFile(filepath.Join(dir, corpusChild))
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close()
	if depth := im.(*vhdImage).d.ChainDepth(); depth != 2 {
		t.Fatalf("chain depth %d, want 2", depth)
	}
	buf := make([]byte, 1<<20)
	if _, err := im.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}

// TestOpenVHDFileFailsClosed checks that a differencing disk whose chain
// cannot be completed from its own directory is refused, even when the
// absolute parent path recorded in the image exists on this machine.
func TestOpenVHDFileFailsClosed(t *testing.T) {
	t.Parallel()
	dir := corpusVHD.Require(t)

	t.Run("missing ancestor", func(t *testing.T) {
		im, err := OpenVHDFile(filepath.Join(dir, corpusBrokenLeaf))
		if err == nil {
			im.Close()
			t.Fatal("opened a differencing disk with a missing ancestor")
		}
	})

	t.Run("child moved without parent", func(t *testing.T) {
		moved := filepath.Join(t.TempDir(), corpusChild)
		copyFile(t, filepath.Join(dir, corpusChild), moved)
		im, err := OpenVHDFile(moved)
		if err == nil {
			im.Close()
			t.Fatal("parent was resolved outside the child's directory")
		}
	})

	t.Run("reader without path", func(t *testing.T) {
		f, err := os.Open(filepath.Join(dir, corpusChild))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		im, err := OpenVHD(f)
		if err == nil {
			im.Close()
			t.Fatal("opened a differencing disk from a bare reader")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "parent") {
			t.Fatalf("error does not name the parent: %v", err)
		}
	})
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
