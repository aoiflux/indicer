package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// HashFile hashes all of f with h, from its start, and leaves f at its
// start.
func HashFile(f *os.File, h hash.Hash) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	_, err := f.Seek(0, io.SeekStart)
	return h.Sum(nil), err
}

// HashRange hashes size bytes of f from offset start with h. A file
// shorter than start+size is an error.
func HashRange(f *os.File, h hash.Hash, start, size int64) ([]byte, error) {
	if _, err := io.CopyN(h, io.NewSectionReader(f, start, size), size); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// RandomBytes returns n pseudo-random bytes from seed. The same seed always
// gives the same bytes, on every platform and Go release.
func RandomBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		v := r.Uint64()
		for j := range 8 {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	for i := n &^ 7; i < n; i++ {
		b[i] = byte(r.Uint64())
	}
	return b
}

// WriteRandomFile writes n deterministic pseudo-random bytes to a new file
// under dir and returns its path.
func WriteRandomFile(tb testing.TB, dir, name string, seed uint64, n int) string {
	tb.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(p, RandomBytes(seed, n), 0o644); err != nil {
		tb.Fatal(err)
	}
	return p
}

// FileSHA256 returns the hex SHA-256 of a file's content.
func FileSHA256(tb testing.TB, path string) string {
	tb.Helper()
	f, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		tb.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// BytesSHA256 returns the hex SHA-256 of b.
func BytesSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TreeHash digests a directory tree: every regular file's slash path,
// size and content, in sorted order. Two trees hash equal exactly when they
// hold the same files with the same bytes. Directories and modes are ignored.
func TreeHash(tb testing.TB, root string) string {
	tb.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, rel := range paths {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			tb.Fatal(err)
		}
		sum := sha256.Sum256(b)
		_, _ = io.WriteString(h, rel+"\x00"+hex.EncodeToString(sum[:])+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FilesMatching returns the slash paths under root whose base name matches
// the glob pattern.
func FilesMatching(tb testing.TB, root, pattern string) []string {
	tb.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		ok, merr := filepath.Match(pattern, d.Name())
		if merr != nil {
			return merr
		}
		if ok && p != root {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return out
}

// AssertNoFilesMatching fails the test if anything under root has a base name
// matching pattern; use it to prove temp files were cleaned up.
func AssertNoFilesMatching(tb testing.TB, root, pattern string) {
	tb.Helper()
	if left := FilesMatching(tb, root, pattern); len(left) > 0 {
		tb.Fatalf("files matching %q left under %s: %s", pattern, root, strings.Join(left, ", "))
	}
}
