package legacyfixture

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"indicer/internal/testutil"
)

// TestFixturesExtract checks every archive named by golden.json unpacks into
// a badger directory.
func TestFixturesExtract(t *testing.T) {
	for _, f := range Load(t) {
		t.Run(f.Mode, func(t *testing.T) {
			db := Extract(t, f)
			for _, want := range []string{"kvdb/MANIFEST", "kvdb/KEYREGISTRY"} {
				if _, err := os.Stat(filepath.Join(db, filepath.FromSlash(want))); err != nil {
					t.Errorf("%s missing: %v", want, err)
				}
			}
		})
	}
}

func TestExtractRejectsUnsafeEntries(t *testing.T) {
	cases := []struct {
		name string
		hdr  tar.Header
	}{
		{"parent", tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}},
		{"absolute", tar.Header{Name: "/evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}},
		{"nested parent", tar.Header{Name: "data/../../evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644}},
		{"symlink", tar.Header{Name: "data/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "bad.tar.zst")
			writeArchive(t, archive, tc.hdr)
			dst := filepath.Join(dir, "out")
			if err := ExtractTo(archive, dst); err == nil {
				t.Fatal("ExtractTo accepted an unsafe entry")
			}
			if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
				t.Fatal("an entry was written outside the destination")
			}
		})
	}
}

func writeArchive(t *testing.T, path string, hdr tar.Header) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Size > 0 {
		if _, err := tw.Write(bytes.Repeat([]byte("x"), int(hdr.Size))); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []io.Closer{tw, zw, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLegacyRestore is the back-compat gate: the current dues binary must
// restore every object in every legacy database to its true content.
//
// It runs the binary rather than calling packages, so it holds across
// internal rewrites. An object the writing release itself could not restore
// is a known defect: while it still fails the subtest is skipped with the
// defect ID, and once fixed it passes. An object that restored correctly at
// generation time and fails now is a regression.
func TestLegacyRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the dues binary")
	}
	bin := testutil.BuildDues(t)
	for _, f := range Load(t) {
		t.Run(f.Mode, func(t *testing.T) {
			db := Extract(t, f)
			work := t.TempDir()
			for i, r := range f.Restores {
				want, ok := f.Expected(r)
				if !ok {
					t.Errorf("%s %s: golden has no expected content", r.Kind, r.Name)
					continue
				}
				t.Run(r.Kind+":"+r.Name, func(t *testing.T) {
					out := filepath.Join(work, "r"+strconv.Itoa(i)+".bin")
					args := append([]string{"restore", r.Hash, "-f", out, "--dbpath", db}, f.ReadFlags(t)...)
					cmdOut, runErr := runIn(t, work, bin, args...)
					got, size, hashErr := fileSHA256(out)
					correct := runErr == nil && hashErr == nil && got == want.SHA256 && size == want.Size
					if correct {
						return
					}
					wasCorrect := r.Status == "ok" && r.SHA256 != nil && *r.SHA256 == want.SHA256
					detail := "run: " + errString(runErr) + "; output: " + lastLine(cmdOut) + "; sha256 " + got
					if !wasCorrect {
						t.Skipf("known defect %s: %s", knownDefect(f, r), detail)
					}
					t.Fatalf("regression: restored correctly when generated, now: %s", detail)
				})
			}
		})
	}
}

// knownDefect names the bug inventory entry behind a restore the writing
// release got wrong.
func knownDefect(f Fixture, r Restore) string {
	switch {
	case strings.HasPrefix(f.Mode, "container") && r.Kind == "evidence":
		return "STO-1 (second session reuses container names with offsets from 0)"
	case f.Mode == "e01":
		return "ING-7 (E01 indexed over the raw container)"
	}
	return "unclassified"
}

func runIn(t *testing.T, dir, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, testutil.IsolatedArgs(t, args)...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func fileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func lastLine(s string) string {
	var last string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l[strings.LastIndex(l, "\r")+1:])
		if l != "" {
			last = l
		}
	}
	return last
}

func errString(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
