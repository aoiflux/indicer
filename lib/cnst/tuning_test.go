package cnst

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoMutableConfigGlobals: package cnst declares no variable but its
// errors. Layout, tuning and encoders belong to each open database (package
// engine) or travel in a Tuning; a process-wide setting would let one
// database's settings reach another's (I3).
func TestNoMutableConfigGlobals(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, spec := range g.Specs {
				v := spec.(*ast.ValueSpec)
				for i, id := range v.Names {
					if !strings.HasPrefix(id.Name, "Err") || i >= len(v.Values) || !isErrorsNew(v.Values[i]) {
						t.Errorf("%s: package variable %s; pass settings in a cnst.Tuning or keep them per database (engine.For)", fset.Position(id.Pos()), id.Name)
					}
				}
			}
		}
	}
}

// isErrorsNew reports whether e is a call errors.New(...).
func isErrorsNew(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "errors" && sel.Sel.Name == "New"
}

// TestPipelineOverrides: set workers and queues replace the machine's, and
// a queue is never shorter than the workers it feeds.
func TestPipelineOverrides(t *testing.T) {
	t.Parallel()
	for _, container := range []bool{false, true} {
		w, q := DefaultStoreTuning(runtime.NumCPU(), false, container)
		if gw, gq := (Tuning{}).StorePipeline(container); gw != w || gq != q {
			t.Errorf("container %v: default store pipeline %d/%d, want %d/%d", container, gw, gq, w, q)
		}
		if gw, gq := (Tuning{StoreWorkers: 9, StoreQueue: 2}).StorePipeline(container); gw != 9 || gq != 9 {
			t.Errorf("container %v: store pipeline %d/%d, want 9/9", container, gw, gq)
		}
	}
	if w, q := (Tuning{RestoreWorkers: 3, RestoreQueue: 30}).RestorePipeline(); w != 3 || q != 30 {
		t.Errorf("restore pipeline %d/%d, want 3/30", w, q)
	}
}

// TestRestoreWriteBuffer: the default is 256 MiB, 32 MiB in low-resource
// mode, and a set size is capped at 256 MiB.
func TestRestoreWriteBuffer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		t    Tuning
		want int
	}{
		{Tuning{}, 256 * 1024 * 1024},
		{Tuning{LowResource: true}, 32 * 1024 * 1024},
		{Tuning{RestoreBufferMB: 5}, 5 * 1024 * 1024},
		{Tuning{RestoreBufferMB: 300}, 256 * 1024 * 1024},
	} {
		if got := c.t.RestoreWriteBuffer(); got != c.want {
			t.Errorf("%+v: %d bytes, want %d", c.t, got, c.want)
		}
	}
}
