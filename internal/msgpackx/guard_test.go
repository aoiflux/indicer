package msgpackx

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// directDecode matches decoding that bypasses this package.
var directDecode = regexp.MustCompile(`\bmsgpack\.(Unmarshal|NewDecoder|GetDecoder)\b`)

// guardExempt are the directories allowed to decode directly: this package,
// and test helpers and tools, which decode what they wrote.
var guardExempt = []string{
	"internal/msgpackx/",
	"internal/testutil/",
	"internal/tools/",
}

// TestDecodesGoThroughMsgpackx fails on any msgpack decode outside tests and
// this package: production code reads records it cannot assume are intact,
// and msgpack alone allocates whatever a damaged length asks for.
func TestDecodesGoThroughMsgpackx(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		for _, ex := range guardExempt {
			if strings.HasPrefix(rel, ex) {
				return nil
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		line := 0
		for sc.Scan() {
			line++
			code, _, _ := strings.Cut(sc.Text(), "//")
			if directDecode.MatchString(code) {
				offenders = append(offenders, rel+":"+strconv.Itoa(line)+": "+strings.TrimSpace(sc.Text()))
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("%d msgpack decodes outside internal/msgpackx; use msgpackx.Unmarshal:\n%s", len(offenders), strings.Join(offenders, "\n"))
	}
}
