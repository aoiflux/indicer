package leakcheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEveryTestPackageChecksForLeaks: every directory with tests has a
// TestMain that runs them through Run, so a new test package cannot skip the
// check.
func TestEveryTestPackageChecksForLeaks(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tested := map[string]bool{}
	checked := map[string]bool{}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir = filepath.ToSlash(dir)
		tested[dir] = true
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if runsThroughRun(f) {
			checked[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for dir := range tested {
		if !checked[dir] {
			missing = append(missing, dir)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Fatalf("these test packages need a TestMain that runs leakcheck.Run (or testutil.RunChildScenarios):\n\t%s\n\nfor example:\n\tfunc TestMain(m *testing.M) { os.Exit(leakcheck.Run(m)) }",
			strings.Join(missing, "\n\t"))
	}
}

// runsThroughRun reports whether f's TestMain calls leakcheck.Run or
// testutil.RunChildScenarios, which calls it.
func runsThroughRun(f *ast.File) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if x, ok := fun.X.(*ast.Ident); ok {
					found = found || (x.Name == "leakcheck" && fun.Sel.Name == "Run") ||
						(x.Name == "testutil" && fun.Sel.Name == "RunChildScenarios")
				}
			case *ast.Ident:
				// Inside leakcheck and testutil themselves.
				found = found || (f.Name.Name == "leakcheck" && fun.Name == "Run") ||
					(f.Name.Name == "testutil" && fun.Name == "RunChildScenarios")
			}
			return !found
		})
		return found
	}
	return false
}
