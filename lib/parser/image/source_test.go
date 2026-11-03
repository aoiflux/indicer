package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
	"indicer/lib/parser/core"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fsfixture.Cleanup()
	os.Exit(code)
}

// ewfSet copies the multi-segment EWF fixture into a fresh directory and
// returns the directory and the first segment's path.
func ewfSet(t *testing.T) (string, string) {
	t.Helper()
	var m fsfixture.Manifest
	for _, x := range fsfixture.All(t) {
		if x.Name == "ewf/fat12_multiseg" {
			m = x
		}
	}
	if m.Name == "" {
		t.Fatal("fixture ewf/fat12_multiseg missing")
	}
	src := filepath.Dir(fsfixture.Open(t, m))
	dir := t.TempDir()
	for _, seg := range m.Segments {
		b, err := os.ReadFile(filepath.Join(src, seg))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, seg), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, filepath.Join(dir, m.Segments[0])
}

func TestOpenSourceEWF(t *testing.T) {
	dir, first := ewfSet(t)
	// PAR-23: a sibling that only looks like a segment is not one.
	if err := os.WriteFile(filepath.Join(dir, "fat12_multiseg.Exe"), []byte("MZ not evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := OpenSource(context.Background(), first, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Image.Close()
	if src.Format != core.FormatEWF || len(src.Files) != 4 {
		t.Fatalf("format %s, files %v", src.Format, src.Files)
	}
	for _, f := range src.Files {
		if filepath.Ext(f) == ".Exe" {
			t.Fatalf("segment set took in %s", f)
		}
	}
	e := src.Meta.EWF
	if e == nil || len(e.SHA1) != 20 {
		t.Fatalf("EWF metadata %+v: the fixture stores a SHA-1", e)
	}

	// The digester verifies the stored digests over the decoded image.
	d := NewDigester(src.Meta)
	if _, err := io.Copy(d, io.NewSectionReader(src.Image, 0, src.Image.Size())); err != nil {
		t.Fatal(err)
	}
	res := d.Result()
	if !res.SHA1Checked || !res.SHA1Match || !res.OK() {
		t.Fatalf("digest result %+v", res)
	}
	d = NewDigester(src.Meta)
	_, _ = d.Write([]byte("not the image"))
	if res := d.Result(); res.SHA1Match || res.OK() {
		t.Fatalf("a wrong stream verified: %+v", res)
	}
}

func TestOpenSourceEWFMissingSegment(t *testing.T) {
	dir, first := ewfSet(t)
	if err := os.Remove(filepath.Join(dir, "fat12_multiseg.E02")); err != nil {
		t.Fatal(err)
	}
	_, err := OpenSource(context.Background(), first, Options{})
	var miss *MissingSegmentsError
	if !errors.As(err, &miss) || len(miss.Missing) != 1 {
		t.Fatalf("err %v, want the missing segment named", err)
	}
}

func TestOpenSourceSplitRaw(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	parts := [][]byte{data[:20000], data[20000:50000], data[50000:]}
	for i, p := range parts {
		if err := os.WriteFile(filepath.Join(dir, "disk.00"+string(rune('1'+i))), p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src, err := OpenSource(context.Background(), filepath.Join(dir, "disk.001"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if src.Format != core.FormatRaw || len(src.Files) != 3 || src.Image.Size() != int64(len(data)) {
		t.Fatalf("format %s files %v size %d", src.Format, src.Files, src.Image.Size())
	}
	got, _ := io.ReadAll(io.NewSectionReader(src.Image, 0, src.Image.Size()))
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("split image content differs")
	}
	src.Image.Close()

	if err := os.Remove(filepath.Join(dir, "disk.002")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSource(context.Background(), filepath.Join(dir, "disk.001"), Options{}); !errors.Is(err, ErrMissingSplitSegment) {
		t.Fatalf("err %v, want ErrMissingSplitSegment", err)
	}
}

func TestOpenSourceUnsupportedContainers(t *testing.T) {
	cases := map[string][]byte{
		"vmdk":  append([]byte("KDMV"), make([]byte, 1020)...),
		"qcow2": append([]byte("QFI\xfb"), make([]byte, 1020)...),
		"l01":   append([]byte("LVF\x09\x0d\x0a\xff\x00"), make([]byte, 1016)...),
	}
	for format, head := range cases {
		t.Run(format, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "evidence.bin")
			if err := os.WriteFile(p, head, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := OpenSource(context.Background(), p, Options{})
			var uc *UnsupportedContainerError
			if !errors.As(err, &uc) || uc.Format != format || !errors.Is(err, ErrUnsupportedContainer) {
				t.Fatalf("err %v, want unsupported %s", err, format)
			}
		})
	}
	t.Run("dmg", func(t *testing.T) {
		b := make([]byte, 4096)
		copy(b[len(b)-512:], "koly")
		p := filepath.Join(t.TempDir(), "evidence.dmg")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenSource(context.Background(), p, Options{}); !errors.Is(err, ErrUnsupportedContainer) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestOpenSourceRaw(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(p, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := OpenSource(context.Background(), p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Image.Close()
	if src.Format != core.FormatRaw || len(src.Files) != 1 || src.Meta.EWF != nil {
		t.Fatalf("%+v", src)
	}
}
