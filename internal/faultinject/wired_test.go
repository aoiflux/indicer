package faultinject

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestEveryPointIsWired: each catalogued point is hit by production code and
// named by a test outside this package, so a point cannot be added, or its
// call removed, without the crash tests noticing.
func TestEveryPointIsWired(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	repo := filepath.Join(dir, "..", "..")

	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, "points.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			for _, n := range s.(*ast.ValueSpec).Names {
				if n.Name != "SelfTest" {
					names = append(names, n.Name)
				}
			}
		}
	}
	if len(names) != len(known)-1 {
		t.Fatalf("parsed %d point constants, the catalogue has %d", len(names), len(known)-1)
	}

	hit := regexp.MustCompile(`faultinject\.Hit\(faultinject\.(\w+)\)`)
	named := regexp.MustCompile(`faultinject\.(\w+)`)
	hits, tests := map[string]bool{}, map[string]bool{}
	err = filepath.WalkDir(repo, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == dir || strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		set, re := hits, hit
		if strings.HasSuffix(p, "_test.go") {
			set, re = tests, named
		}
		for _, m := range re.FindAllSubmatch(b, -1) {
			set[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if !hits[n] {
			t.Errorf("point %s is never hit", n)
		}
		if !tests[n] {
			t.Errorf("point %s is named by no test", n)
		}
	}
}
