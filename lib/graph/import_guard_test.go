package graph

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// openCall matches a call that opens a graphene store directly. Both the root
// package and the disk backend expose one, and either creates an independent
// view of a directory.
var openCall = regexp.MustCompile(`\b(graphene|graphenedb|graphdb|disk)\.(Open|OpenWithOptions)\s*\(`)

// TestNoDirectStoreOpensOutsideThisPackage keeps the single-owner rule from
// eroding one call site at a time.
//
// The rule is deliberately narrow. Importing graphene's store package for its
// types is fine and widespread — what must not happen outside lib/graph is
// *opening* a store, because graphene has no locking and a second open is a
// second independent view of the same files. See the package doc.
//
// A grep is enough here: the failure mode is someone adding a call, not someone
// disguising one, and a test that a reviewer can understand in ten seconds is
// worth more than an AST walk that they will not read.
func TestNoDirectStoreOpensOutsideThisPackage(t *testing.T) {
	t.Parallel()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	guarded, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	var offenders []string

	err = filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "dist", "node_modules", "log", "data", "tdata":
				return filepath.SkipDir
			}
			// lib/graph owns the store; everything under it is exempt.
			if abs, aerr := filepath.Abs(path); aerr == nil && abs == guarded {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}

		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if openCall.MatchString(line) {
				rel, _ := filepath.Rel(repoRoot, path)
				offenders = append(offenders,
					filepath.ToSlash(rel)+":"+itoa(i+1)+": "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("graphene stores are opened outside lib/graph:\n  %s\n\n"+
			"graphene has no locking: a second open is a second independent view of the "+
			"same directory, with its own ID sequence, its own delta overlay and its own "+
			"WAL writer. Use graph.Open (which reference-counts one handle per directory) "+
			"or borrow an existing handle with OpenOn.",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
