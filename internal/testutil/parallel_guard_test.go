package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The module's import path, which its packages' paths start with.
const modulePath = "indicer"

// serialPackages own process-wide state their tests change, or measure the
// whole process (leakcheck counts every goroutine), so none of their tests
// runs in parallel.
var serialPackages = map[string]bool{
	"indicer/internal/ui":                 true,
	"indicer/internal/console":            true,
	"indicer/internal/crash":              true,
	"indicer/internal/lifecycle":          true,
	"indicer/internal/testutil/leakcheck": true,
	"indicer/lib/logging":                 true,
}

// processCalls change or measure what the whole process shares, by import
// path and name; "*" stands for every function of the package.
var processCalls = map[string]map[string]bool{
	"os":                           {"Setenv": true, "Unsetenv": true, "Clearenv": true, "Chdir": true},
	"runtime":                      {"GC": true, "ReadMemStats": true, "GOMAXPROCS": true, "NumGoroutine": true, "LockOSThread": true},
	"runtime/debug":                {"SetGCPercent": true, "SetMemoryLimit": true, "FreeOSMemory": true, "SetMaxThreads": true, "SetMaxStack": true, "SetPanicOnFault": true, "SetTraceback": true},
	"runtime/metrics":              {"*": true},
	"runtime/trace":                {"*": true},
	"os/signal":                    {"*": true},
	"testing":                      {"AllocsPerRun": true},
	"indicer/internal/faultinject": {"*": true},
	"indicer/internal/testutil":    {"AllocBelow": true, "PeakHeap": true},
}

// testMethods are the methods of testing.T a parallel test cannot call.
var testMethods = map[string]bool{"Setenv": true, "Chdir": true}

// mutators change the value they are called on: called on a package-level
// variable, they write it.
var mutators = map[string]bool{
	"Store": true, "Swap": true, "CompareAndSwap": true, "Add": true, "Set": true,
	"Reset": true, "Register": true, "Unregister": true, "Write": true, "WriteString": true,
}

// sharedSafely are functions that write package state in a way callers at
// once cannot see: registries and caches by key under a lock or a
// sync.Once, and fault hits, which only fault tests read, and those call
// faultinject themselves.
var sharedSafely = map[string]bool{
	"indicer/internal/diskspace.GuardFor":          true,
	"indicer/internal/faultinject.Hit":             true,
	"indicer/internal/lifecycle.OnForceExit":       true,
	"indicer/internal/safe.recovered":              true,
	"indicer/internal/testutil.BuildBinary":        true,
	"indicer/internal/testutil/fsfixture.Open":     true,
	"indicer/internal/testutil/testdb.keepEncoder": true,
	"indicer/internal/ui.buildSGR":                 true,
	"indicer/lib/blobcodec.Register":               true,
	"indicer/lib/dbio.EvidenceChunkSize":           true,
	"indicer/lib/dbio.RegisterKVDir":               true,
	"indicer/lib/dbmeta.RegisterManifest":          true,
	"indicer/lib/engine.Register":                  true,
	"indicer/lib/fio.initWriteBackend":             true,
	"indicer/lib/fts.acquire":                      true,
	"indicer/lib/graph.Configure":                  true,
	"indicer/lib/graph.OpenWith":                   true,
	"indicer/lib/store.joinChunkWriter":            true,
	"indicer/lib/zstdx.Acquire":                    true,
	"indicer/pb.file_dues_proto_init":              true,
}

// TestParallelTestsShareNoProcessState: a test that calls t.Parallel runs
// beside the other parallel tests of its package, so neither it nor any
// function of its test files it reaches may change what the process shares:
// a package variable (a seam a test replaces, os.Stdout), the environment or
// working directory, fault injection, the runtime's settings, or the
// console, log or display a command sets up, through a function of the
// module that writes them. Nor may it measure the process as a whole: the
// heap, goroutines, a trace. Such a test stays serial, and Go runs it before
// the parallel ones start.
func TestParallelTestsShareNoProcessState(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	m, err := loadModule(root)
	if err != nil {
		t.Fatal(err)
	}
	bad := m.unsafeParallelTests()
	if len(bad) > 0 {
		t.Fatalf("these tests run in parallel but touch process-wide state; drop their t.Parallel, or pass the state in:\n\t%s",
			strings.Join(bad, "\n\t"))
	}
}

// TestParallelGuardSeesSeams: the guard finds each way a test can reach
// shared state, so a parallel test that does is reported.
func TestParallelGuardSeesSeams(t *testing.T) {
	t.Parallel()
	const prod = `package p
import (
	"sync/atomic"
	"github.com/alecthomas/kingpin/v2"
)
var seam = func() {}
var current atomic.Pointer[int]
var registry = map[string]int{}
var table = map[string]int{"a": 1}
func Set(v *int) { current.Store(v) }
func Wrap() { Set(nil) }
func Read() *int { return current.Load() }
func Add(k string) { registry[k]++ }
func Flags() { kingpin.EnableFileExpansion = false }
`
	cases := map[string]string{
		"assigns":       `func TestX(t *testing.T) { t.Parallel(); seam = nil }`,
		"helper":        `func TestX(t *testing.T) { t.Parallel(); fake() }; func fake() { seam = nil }`,
		"method helper": `type h struct{}; func (h) put() { seam = nil }; func TestX(t *testing.T) { t.Parallel(); h{}.put() }`,
		"setter":        `func TestX(t *testing.T) { t.Parallel(); Set(nil) }`,
		"via setter":    `func TestX(t *testing.T) { t.Parallel(); Wrap() }`,
		"stdout":        `func TestX(t *testing.T) { t.Parallel(); os.Stdout = nil }`,
		"setenv":        `func TestX(t *testing.T) { t.Parallel(); t.Setenv("A", "b") }`,
		"gc":            `func TestX(t *testing.T) { t.Parallel(); runtime.GC() }`,
		"faults":        `func TestX(t *testing.T) { t.Parallel(); faultinject.Set("x", "y") }`,
		"as value":      `func TestX(t *testing.T) { t.Parallel(); t.Cleanup(Wrap) }`,
		"delete":        `var reg = map[string]int{}; func TestX(t *testing.T) { t.Parallel(); delete(reg, "a") }`,
		"other package": `func TestX(t *testing.T) { t.Parallel(); Flags() }`,
		"counts":        `func TestX(t *testing.T) { t.Parallel(); _ = len(registry) }`,
	}
	for name, src := range cases {
		m := parseModule(t, prod, testFile(src))
		if bad := m.unsafeParallelTests(); len(bad) != 1 {
			t.Errorf("%s: guard reported %v, want TestX", name, bad)
		}
	}
	safe := []string{
		`func TestX(t *testing.T) { t.Parallel(); _ = Read(); local := 1; local++ }`,
		`func TestX(t *testing.T) { seam = nil }`,
		`func TestX(t *testing.T) { t.Parallel(); seam := 1; seam++ }`,
		`func TestX(t *testing.T) { t.Parallel(); _ = len(table) }`,
	}
	for _, src := range safe {
		m := parseModule(t, prod, testFile(src))
		if bad := m.unsafeParallelTests(); len(bad) != 0 {
			t.Errorf("%s: guard reported %v, want nothing", src, bad)
		}
	}
	measures := parsePackage(t, modulePath+"/internal/testutil", "package testutil\nfunc PeakHeap(fn func()) uint64 { return 0 }\n",
		"package testutil\nimport \"testing\"\nfunc TestX(t *testing.T) { t.Parallel(); _ = PeakHeap(nil) }\n")
	if bad := measures.unsafeParallelTests(); len(bad) != 1 {
		t.Errorf("own measuring helper: guard reported %v, want TestX", bad)
	}
	for path, want := range map[string]string{
		"os":                               "os",
		"github.com/alecthomas/kingpin/v2": "kingpin",
		"gopkg.in/yaml.v3":                 "yaml",
		"github.com/mattn/go-runewidth":    "runewidth",
		"indicer/lib/v2store":              "v2store",
	} {
		if got := importName(path); got != want {
			t.Errorf("importName(%q) = %q, want %q", path, got, want)
		}
	}
}

func testFile(body string) string {
	return "package p\nimport (\n\t\"os\"\n\t\"runtime\"\n\t\"testing\"\n\t\"indicer/internal/faultinject\"\n)\nvar _ = os.Stdout\nvar _ = runtime.GC\nvar _ = faultinject.Set\n" + body + "\n"
}

// parseModule loads a module of one package p from its sources.
func parseModule(t *testing.T, prod, test string) *module {
	t.Helper()
	return parsePackage(t, modulePath+"/p", prod, test)
}

// parsePackage loads a module of one package at path from its sources.
func parsePackage(t *testing.T, path, prod, test string) *module {
	t.Helper()
	m := newModule()
	for name, src := range map[string]string{"p.go": prod, "p_test.go": test} {
		f, err := parser.ParseFile(m.fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		m.add(path, name, f)
	}
	m.findWriters()
	return m
}

// module is the parsed source of every package under the module root.
type module struct {
	fset *token.FileSet
	pkgs map[string]*goPackage
	// topLevel holds the specs of package-level variables.
	topLevel map[*ast.ValueSpec]bool
}

// goPackage is one directory's files.
type goPackage struct {
	path  string
	files []*ast.File // production files
	tests []*ast.File // _test files, both the package and its _test package
	names map[*ast.File]string
	vars  map[string]bool // package-level variables of the production files
	// writers are the package's functions that write process state, and
	// why.
	writers map[string]string
	// mutated are the package variables its functions and methods write
	// after init.
	mutated map[string]bool
}

func newModule() *module {
	return &module{fset: token.NewFileSet(), pkgs: map[string]*goPackage{}, topLevel: map[*ast.ValueSpec]bool{}}
}

func loadModule(root string) (*module, error) {
	m := newModule()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(m.fset, path, nil, 0)
		if err != nil {
			return err
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		ip := modulePath
		if dir != "." {
			ip += "/" + filepath.ToSlash(dir)
		}
		m.add(ip, path, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.findWriters()
	return m, nil
}

func (m *module) add(ip, name string, f *ast.File) {
	p := m.pkgs[ip]
	if p == nil {
		p = &goPackage{path: ip, names: map[*ast.File]string{}, vars: map[string]bool{}, writers: map[string]string{}, mutated: map[string]bool{}}
		m.pkgs[ip] = p
	}
	p.names[f] = name
	if strings.HasSuffix(name, "_test.go") {
		p.tests = append(p.tests, f)
		m.packageVars(f, nil)
		return
	}
	p.files = append(p.files, f)
	m.packageVars(f, p.vars)
}

// packageVars records f's package-level variables, into vars when not nil.
func (m *module) packageVars(f *ast.File, vars map[string]bool) {
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, s := range g.Specs {
			vs := s.(*ast.ValueSpec)
			m.topLevel[vs] = true
			for _, n := range vs.Names {
				if vars != nil && n.Name != "_" {
					vars[n.Name] = true
				}
			}
		}
	}
}

// findWriters finds the package-level functions of the module that write a
// package variable, or call a function of the module that does.
func (m *module) findWriters() {
	calls := map[string][]string{}
	for ip, p := range m.pkgs {
		for _, f := range p.files {
			imports := importNames(f)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil || fd.Name.Name == "init" {
					continue
				}
				m.eachWrite(fd.Body, p.vars, imports, false, func(_, name string) bool {
					p.mutated[name] = true
					return true
				})
				if fd.Recv != nil {
					continue
				}
				key := ip + "." + fd.Name.Name
				if sharedSafely[key] {
					continue
				}
				if why := m.writes(fd.Body, p.vars, imports, true); why != "" {
					p.writers[fd.Name.Name] = why
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						if id, ok := x.X.(*ast.Ident); ok && id.Obj == nil {
							if path, ok := imports[id.Name]; ok {
								calls[key] = append(calls[key], path+"."+x.Sel.Name)
							}
						}
					case *ast.Ident:
						if x.Obj == nil || x.Obj.Kind == ast.Fun {
							calls[key] = append(calls[key], ip+"."+x.Name)
						}
					}
					return true
				})
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for key, cs := range calls {
			p, name := m.split(key)
			if _, ok := p.writers[name]; ok {
				continue
			}
			for _, c := range cs {
				if q, callee := m.split(c); q != nil && c != key {
					if why, ok := q.writers[callee]; ok {
						p.writers[name] = "through " + c + ": " + why
						changed = true
						break
					}
				}
			}
		}
	}
}

// split splits "path.Name" into its package, nil outside the module, and
// name.
func (m *module) split(key string) (*goPackage, string) {
	i := strings.LastIndex(key, ".")
	return m.pkgs[key[:i]], key[i+1:]
}

// unsafeParallelTests lists the tests that call t.Parallel and reach process
// state, with why.
func (m *module) unsafeParallelTests() []string {
	var bad []string
	for ip, p := range m.pkgs {
		clauses := map[string][]*ast.File{}
		for _, f := range p.tests {
			clauses[f.Name.Name] = append(clauses[f.Name.Name], f)
		}
		for clause, files := range clauses {
			var own *goPackage
			vars := map[string]bool{}
			if !strings.HasSuffix(clause, "_test") {
				own = p
				for v := range p.vars {
					vars[v] = true
				}
			}
			for _, f := range files {
				m.packageVars(f, vars)
			}
			for _, test := range (testFuncs{m: m, own: own, vars: vars, files: files}).parallel() {
				name := p.names[test.file]
				if serialPackages[ip] {
					bad = append(bad, name+": "+test.decl.Name.Name+": its package owns process state")
				} else if why := test.why; why != "" {
					bad = append(bad, name+": "+test.decl.Name.Name+": "+why)
				}
			}
		}
	}
	slices.Sort(bad)
	return bad
}

// testFuncs are the functions of one test package's files.
type testFuncs struct {
	m     *module
	own   *goPackage // the package under test, nil for a _test package
	vars  map[string]bool
	files []*ast.File
}

type parallelTest struct {
	decl *ast.FuncDecl
	file *ast.File
	why  string
}

// parallel returns the tests that call t.Parallel, each with why it touches
// process state, directly or through a function of the test files, or "".
func (tf testFuncs) parallel() []parallelTest {
	names := map[string]bool{}
	for _, f := range tf.files {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Body != nil {
				names[fd.Name.Name] = true
			}
		}
	}
	// Functions and methods are known by name alone: a method of one type
	// stands for those of every type with its name.
	direct := map[string]string{}
	reaches := map[string]map[string]bool{}
	var tests []parallelTest
	for _, f := range tf.files {
		imports := importNames(f)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			name := fd.Name.Name
			why := tf.m.writes(fd.Body, tf.vars, imports, false)
			if why == "" {
				why = tf.m.processCall(fd.Body, imports, tf.own)
			}
			if why == "" {
				why = tf.m.counts(fd.Body, tf.own)
			}
			if direct[name] == "" {
				direct[name] = why
			}
			if reaches[name] == nil {
				reaches[name] = map[string]bool{}
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if names[x.Name] && (x.Obj == nil || x.Obj.Kind == ast.Fun) {
						reaches[name][x.Name] = true
					}
				case *ast.SelectorExpr:
					if names[x.Sel.Name] {
						reaches[name][x.Sel.Name] = true
					}
				}
				return true
			})
			if fd.Recv == nil && isTest(fd) && callsParallel(fd) {
				tests = append(tests, parallelTest{decl: fd, file: f})
			}
		}
	}
	var reason func(name string, seen map[string]bool) string
	reason = func(name string, seen map[string]bool) string {
		if seen[name] {
			return ""
		}
		seen[name] = true
		if why := direct[name]; why != "" {
			return why
		}
		for c := range reaches[name] {
			if why := reason(c, seen); why != "" {
				return c + ": " + why
			}
		}
		return ""
	}
	for i := range tests {
		tests[i].why = reason(tests[i].decl.Name.Name, map[string]bool{})
	}
	return tests
}

// writes reports how body writes a package variable, or "". In production
// code a sync.Once's function is left out: it writes once, for everyone.
func (m *module) writes(body *ast.BlockStmt, vars map[string]bool, imports map[string]string, production bool) string {
	why := ""
	m.eachWrite(body, vars, imports, production, func(w, _ string) bool {
		why = w
		return false
	})
	return why
}

// eachWrite calls visit with how body writes a package variable, and the
// variable, for each write until visit returns false.
func (m *module) eachWrite(body *ast.BlockStmt, vars map[string]bool, imports map[string]string, production bool, visit func(why, name string) bool) {
	more := true
	found := func(why, name string) {
		if more {
			more = visit(why, name)
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if !more {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Obj == nil && (id.Name == "delete" || id.Name == "clear") && len(x.Args) > 0 {
				if w := m.written(x.Args[0], vars, imports); w != "" {
					found("calls "+id.Name+" on "+w, w)
				}
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if production && sel.Sel.Name == "Do" {
					return false
				}
				if mutators[sel.Sel.Name] {
					if w := m.written(sel.X, vars, imports); w != "" {
						found("calls "+w+"."+sel.Sel.Name, w)
					}
				}
			}
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				return true
			}
			for _, l := range x.Lhs {
				if w := m.written(l, vars, imports); w != "" {
					found("writes "+w, w)
				}
			}
		case *ast.IncDecStmt:
			if w := m.written(x.X, vars, imports); w != "" {
				found("writes "+w, w)
			}
		}
		return more
	})
}

// counts reports which variable of the package under test, of those its
// code changes, body takes the length of, or "": a test that counts a
// registry counts what the tests beside it hold too.
func (m *module) counts(body *ast.BlockStmt, own *goPackage) string {
	if own == nil {
		return ""
	}
	why := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && why == "" && len(c.Args) == 1 {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Obj == nil && id.Name == "len" {
				if v, ok := c.Args[0].(*ast.Ident); ok && own.mutated[v.Name] && m.packageVar(v, own.vars) {
					why = "counts " + v.Name
				}
			}
		}
		return why == ""
	})
	return why
}

// written names the package-level variable that writing e writes, or "".
func (m *module) written(e ast.Expr, vars map[string]bool, imports map[string]string) string {
	for {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Obj == nil {
				if _, ok := imports[id.Name]; ok {
					return id.Name + "." + x.Sel.Name
				}
			}
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.Ident:
			if m.packageVar(x, vars) {
				return x.Name
			}
			return ""
		default:
			return ""
		}
	}
}

// packageVar reports whether id is a package-level variable: one of vars
// that its file leaves unresolved (declared in another file) or resolves to
// a package-level declaration.
func (m *module) packageVar(id *ast.Ident, vars map[string]bool) bool {
	if id.Name == "_" || !vars[id.Name] {
		return false
	}
	if id.Obj == nil {
		return true
	}
	vs, ok := id.Obj.Decl.(*ast.ValueSpec)
	return id.Obj.Kind == ast.Var && ok && m.topLevel[vs]
}

// processCall reports a call in body, or a use as a value, of a function
// that changes or measures the process, or of a writer of the module.
func (m *module) processCall(body *ast.BlockStmt, imports map[string]string, own *goPackage) string {
	why := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if why != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && testMethods[sel.Sel.Name] {
				if _, pkg := imports[identName(sel.X)]; !pkg {
					why = "calls " + sel.Sel.Name
				}
			}
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok || id.Obj != nil {
				return true
			}
			path, ok := imports[id.Name]
			if !ok {
				return true
			}
			if fs := processCalls[path]; fs["*"] || fs[x.Sel.Name] {
				why = "calls " + id.Name + "." + x.Sel.Name
			} else if p := m.pkgs[path]; p != nil && p.writers[x.Sel.Name] != "" {
				why = "calls " + id.Name + "." + x.Sel.Name + ", which " + p.writers[x.Sel.Name]
			}
			return false
		case *ast.Ident:
			if own == nil || (x.Obj != nil && x.Obj.Kind != ast.Fun) {
				return true
			}
			if processCalls[own.path][x.Name] {
				why = "calls " + x.Name
			} else if w := own.writers[x.Name]; w != "" {
				why = "calls " + x.Name + ", which " + w
			}
		}
		return why == ""
	})
	return why
}

func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok && id.Obj == nil {
		return id.Name
	}
	return ""
}

// importNames maps each name f imports a package under to its path.
func importNames(f *ast.File) map[string]string {
	names := map[string]string{}
	for _, s := range f.Imports {
		path, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			continue
		}
		name := importName(path)
		if s.Name != nil {
			name = s.Name.Name
		}
		names[name] = path
	}
	return names
}

// importName is the name a package imported from path goes by when the
// import gives none: its last element, less a major version ("kingpin/v2",
// "yaml.v3") and a "go-" prefix.
func importName(path string) string {
	elems := strings.Split(path, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && majorVersion(name) {
		name = elems[len(elems)-2]
	}
	if i := strings.Index(name, ".v"); i > 0 && majorVersion(name[i+1:]) {
		name = name[:i]
	}
	return strings.TrimPrefix(name, "go-")
}

func majorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isTest reports whether fd is a top-level test: TestXxx(t *testing.T).
func isTest(fd *ast.FuncDecl) bool {
	name := fd.Name.Name
	if !strings.HasPrefix(name, "Test") || name == "TestMain" || (len(name) > 4 && name[4] >= 'a' && name[4] <= 'z') {
		return false
	}
	ps := fd.Type.Params.List
	if len(ps) != 1 {
		return false
	}
	st, ok := ps[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := st.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}

// callsParallel reports whether fd's body calls Parallel as a statement of
// its own.
func callsParallel(fd *ast.FuncDecl) bool {
	for _, s := range fd.Body.List {
		e, ok := s.(*ast.ExprStmt)
		if !ok {
			continue
		}
		if c, ok := e.X.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" {
				return true
			}
		}
	}
	return false
}
