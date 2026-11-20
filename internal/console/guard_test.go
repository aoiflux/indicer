package console

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

// terminalWrite matches writes that bypass this package.
var terminalWrite = regexp.MustCompile(`\bfmt\.Print|\bos\.Stdout\b|\bos\.Stderr\b`)

// guardExempt are the directories allowed to touch the terminal directly:
// this package, test helpers and tools, and the log sink's last-resort
// stderr fallback.
var guardExempt = []string{
	"internal/console/",
	"internal/testutil/",
	"internal/tools/",
	"lib/logging/",
}

// TestNoTerminalWritesOutsideMain fails on any fmt.Print* or os.Stdout/Stderr
// outside package main: library code writes through this package so the TUI,
// the FFI library and the qwikpak CLI can redirect it.
func TestNoTerminalWritesOutsideMain(t *testing.T) {
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
			text := sc.Text()
			if line < 40 && strings.HasPrefix(text, "package ") {
				if strings.TrimSpace(strings.TrimPrefix(text, "package ")) == "main" {
					return nil
				}
			}
			code, _, _ := strings.Cut(text, "//")
			if terminalWrite.MatchString(code) {
				offenders = append(offenders, rel+":"+strconv.Itoa(line)+": "+strings.TrimSpace(text))
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("%d terminal writes outside package main; use internal/console:\n%s", len(offenders), strings.Join(offenders, "\n"))
	}
}
