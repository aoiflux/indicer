package cli

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestOnlyMainImportsTheCLI keeps the libraries below the command line. A
// library that imports cli takes on its flags and its printing, and cli
// imports nearly everything else, so the next import back is a cycle. The
// server and the compression product did, for Common; they now open the
// database with dbopen.Open, as Common does.
func TestOnlyMainImportsTheCLI(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(root, "cli")
	var offenders []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".codegraph", "testdata", "dist", "node_modules":
				return filepath.SkipDir
			}
			if path == self {
				return filepath.SkipDir
			}
			return nil
		}
		// The dues command, package main at the root, is what cli is for.
		if filepath.Dir(path) == root || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "indicer/cli" {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("only the dues command may import indicer/cli; these do: %v", offenders)
	}
}
