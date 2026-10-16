package dbio

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// setFileAllowed lists the production functions that may call SetFile, which
// replaces a record with the caller's copy. Evidence records must change
// through UpdateEvidence or CreateEvidence instead (I5, ING-8), so a new
// caller has to be added here deliberately.
var setFileAllowed = map[string]bool{
	"lib/store/store.go:storePartitionFile": true,
}

// TestEvidenceWritesUseUpdateEvidence fails on a SetFile call outside the
// allowed functions in non-test code.
func TestEvidenceWritesUseUpdateEvidence(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
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
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !callsSetFile(call.Fun) {
					return true
				}
				site := rel + ":" + fn.Name.Name
				if !setFileAllowed[site] {
					offenders = append(offenders, fset.Position(call.Pos()).String()+" in "+fn.Name.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range offenders {
		t.Errorf("%s: SetFile replaces the whole record; change evidence with UpdateEvidence or CreateEvidence", o)
	}
}

// callsSetFile reports whether fun names SetFile, qualified or not, with or
// without explicit type arguments.
func callsSetFile(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.IndexExpr:
		return callsSetFile(f.X)
	case *ast.IndexListExpr:
		return callsSetFile(f.X)
	case *ast.SelectorExpr:
		return f.Sel.Name == "SetFile"
	case *ast.Ident:
		return f.Name == "SetFile"
	}
	return false
}
