package evidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/structs"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fsfixture.Cleanup()
	os.Exit(code)
}

func fixture(t *testing.T, name string) (fsfixture.Manifest, string) {
	t.Helper()
	for _, m := range fsfixture.All(t) {
		if m.Name == name {
			return m, fsfixture.Open(t, m)
		}
	}
	t.Fatalf("no fixture %s", name)
	return fsfixture.Manifest{}, ""
}

func digest(t *testing.T, r io.ReaderAt, size int64) string {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(r, 0, size)); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// A container is opened as the disk it holds: its size, its bytes and its
// members are the decoded device's (D5, ING-7).
func TestOpenDecodesContainers(t *testing.T) {
	for _, c := range []struct {
		fixture, format string
		members         int
	}{
		{"ewf/fat12_multiseg", structs.SourceEWF, 4},
		{"vhd/fat16_dynamic_vhd", structs.SourceVHD, 1},
		{"vhd/fat16_vhdx", structs.SourceVHD, 1},
		{"fat/fat12", structs.SourceRaw, 1},
	} {
		t.Run(c.fixture, func(t *testing.T) {
			m, path := fixture(t, c.fixture)
			src, err := Open(context.Background(), path, Options{Decode: true})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			if src.Size() != m.Size {
				t.Fatalf("size %d, want the decoded %d", src.Size(), m.Size)
			}
			if got := digest(t, src, src.Size()); got != m.SHA256 {
				t.Fatalf("content sha256 %s, want the decoded %s", got, m.SHA256)
			}
			info := src.Info()
			if info.Format != c.format || len(info.Members) != c.members {
				t.Fatalf("source %+v, want format %s with %d members", info, c.format, c.members)
			}
			for _, mem := range info.Members {
				if !filepath.IsAbs(mem.Path) || mem.Size <= 0 {
					t.Fatalf("member %+v: want an absolute path and its size", mem)
				}
			}
			if src.SkipIndex() != "" {
				t.Fatalf("decoded source skips indexing: %s", src.SkipIndex())
			}
			if c.format == structs.SourceEWF && (info.EWF == nil || len(info.EWF.StoredMD5) == 0) {
				t.Fatalf("EWF source records no stored digest: %+v", info.EWF)
			}
		})
	}
}

// Without decoding, a file is stored as its own bytes and a container in it
// is not indexed.
func TestOpenPlainKeepsFileBytes(t *testing.T) {
	_, path := fixture(t, "ewf/fat12_multiseg")
	src, err := Open(context.Background(), path, Options{Decode: false})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if src.Size() != int64(len(raw)) {
		t.Fatalf("size %d, want the file's %d", src.Size(), len(raw))
	}
	got := make([]byte, len(raw))
	if _, err := src.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("plain source does not read the file's bytes")
	}
	if src.Info().Format != structs.SourceRaw || src.SkipIndex() != SkipNotDecoded {
		t.Fatalf("format %s skip %q, want raw and %q", src.Info().Format, src.SkipIndex(), SkipNotDecoded)
	}
}

func TestOpenUnsupportedEmptyAndIrregular(t *testing.T) {
	dir := t.TempDir()
	qcow := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(qcow, append([]byte("QFI\xfb"), make([]byte, 4092)...), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := Open(context.Background(), qcow, Options{Decode: true})
	if err != nil {
		t.Fatal(err)
	}
	if src.Info().Format != structs.SourceOpaque || src.Info().Container != "qcow2" || src.SkipIndex() != SkipUnsupportedContainer || src.Size() != 4096 {
		t.Fatalf("qcow2: %+v skip %q size %d", src.Info(), src.SkipIndex(), src.Size())
	}
	src.Close()

	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	src, err = Open(context.Background(), empty, Options{Decode: true})
	if err != nil {
		t.Fatal(err)
	}
	if src.SkipIndex() != SkipEmpty || src.Size() != 0 {
		t.Fatalf("empty: skip %q size %d", src.SkipIndex(), src.Size())
	}
	src.Close()

	if _, err := Open(context.Background(), dir, Options{Decode: true}); err == nil {
		t.Fatal("a directory opened as evidence")
	}
}

func TestFingerprint(t *testing.T) {
	newHash := func() hash.Hash { return sha256.New() }
	fp := func(b []byte) string {
		t.Helper()
		s, err := Fingerprint(bytes.NewReader(b), int64(len(b)), newHash)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	small := bytes.Repeat([]byte("a"), 1000)
	other := append([]byte(nil), small...)
	other[500] = 'b'
	if fp(small) != fp(append([]byte(nil), small...)) {
		t.Fatal("equal content, different fingerprints")
	}
	// Up to the limit the fingerprint covers every byte.
	if fp(small) == fp(other) {
		t.Fatal("small sources differing in one byte share a fingerprint")
	}
	big := make([]byte, FingerprintFullLimit+3*fingerprintSample)
	for i := range big {
		big[i] = byte(i * 7)
	}
	mid := append([]byte(nil), big...)
	mid[len(mid)/2] ^= 1
	if fp(big) == fp(mid) {
		t.Fatal("a change in the middle sample left the fingerprint unchanged")
	}
	if fp(big) == fp(big[:len(big)-1]) {
		t.Fatal("sizes differ, fingerprints equal")
	}
}

func TestPlanGroupsSets(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	m, e01 := fixture(t, "ewf/fat12_multiseg")
	var setSize int64
	for _, seg := range m.Segments {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(e01), seg))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("case", seg), b)
		setSize += int64(len(b))
	}
	write("split/disk.001", []byte("one"))
	write("split/disk.002", []byte("two"))
	write("loose.bin", []byte("x"))
	link := filepath.Join(dir, "link.bin")
	haveLink := os.Symlink(filepath.Join(dir, "loose.bin"), link) == nil

	plan, err := Plan(context.Background(), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	sizes := map[string]int64{}
	for _, it := range plan.Items {
		rel, _ := filepath.Rel(dir, it.Path)
		got[filepath.ToSlash(rel)] = len(it.Members)
		sizes[filepath.ToSlash(rel)] = it.Size
	}
	want := map[string]int{
		"case/" + m.Segments[0]: len(m.Segments) - 1,
		"split/disk.001":        1,
		"loose.bin":             0,
	}
	if len(got) != len(want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("items %v, want %v", got, want)
		}
	}
	// A set weighs all its segments.
	wantSizes := map[string]int64{"case/" + m.Segments[0]: setSize, "split/disk.001": 6, "loose.bin": 1}
	for k, n := range wantSizes {
		if sizes[k] != n {
			t.Fatalf("sizes %v, want %v", sizes, wantSizes)
		}
	}
	if haveLink && (len(plan.Skipped) != 1 || plan.Skipped[0].Path != link) {
		t.Fatalf("skipped %+v, want the symlink", plan.Skipped)
	}

	// Without decoding every file is its own evidence.
	plain, err := Plan(context.Background(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(plain.Items); n != len(m.Segments)+3 {
		t.Fatalf("plain plan has %d items, want %d", n, len(m.Segments)+3)
	}
	for _, it := range plain.Items {
		if fi, err := os.Stat(it.Path); err != nil || it.Size != fi.Size() {
			t.Fatalf("plain item %s: size %d, file %v %v", it.Path, it.Size, fi, err)
		}
	}
}
