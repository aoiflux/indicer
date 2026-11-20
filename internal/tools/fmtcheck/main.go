// Command fmtcheck reports Go files that gofmt would change, ignoring line
// endings.
//
// On Windows the working tree is checked out with CRLF line endings
// (core.autocrlf=true) while the repository stores LF, so plain `gofmt -l`
// lists every file. fmtcheck normalises CRLF to LF before formatting and
// reports only real formatting differences.
//
// Usage: go run ./internal/tools/fmtcheck [dir ...]   (default ".")
//
// Exit status is 0 when every file is formatted, 1 when some are not, and 2
// on an I/O or parse error.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(dirs []string) int {
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	status := 0
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != dir && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			ok, err := formatted(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fmtcheck: %s: %v\n", path, err)
				status = 2
				return nil
			}
			if !ok {
				fmt.Println(filepath.ToSlash(path))
				if status == 0 {
					status = 1
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "fmtcheck: %v\n", err)
			return 2
		}
	}
	return status
}

// skipDir reports directories gofmt checks do not cover: VCS and tool state,
// vendored code and test data.
func skipDir(name string) bool {
	switch name {
	case "vendor", "testdata", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func formatted(path string) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	out, err := format.Source(src)
	if err != nil {
		return false, err
	}
	return bytes.Equal(src, out), nil
}
