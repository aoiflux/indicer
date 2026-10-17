package fsutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"indicer/lib/parser/core"
)

func TestEscapeName(t *testing.T) {
	cases := []struct {
		in, want  string
		sanitized bool
	}{
		{"report.txt", "report.txt", false},
		{"100%.txt", "100%.txt", false}, // % alone needs no escape
		{"résumé.doc", "résumé.doc", false},
		{"a/b", "a%2Fb", true},
		{"50%/x", "50%25%2Fx", true}, // once escaping, % is escaped too
		{"nul\x00here", "nul%00here", true},
		{"esc\x1b[31m", "esc%1B[31m", true},
		{"del\x7f", "del%7F", true},
		{"c1\u0085", "c1%C2%85", true},
		{"bad\xffutf8", "bad%FFutf8", true},
		{".", "%2E", true},
		{"..", "%2E%2E", true},
		{"...", "...", false},
	}
	for _, tc := range cases {
		got, san := EscapeName(tc.in)
		if got != tc.want || san != tc.sanitized {
			t.Errorf("EscapeName(%q) = %q, %v; want %q, %v", tc.in, got, san, tc.want, tc.sanitized)
		}
	}
}

func TestJoinCapsNames(t *testing.T) {
	long := strings.Repeat("é", 100) // 200 bytes
	p, san := Join("/dir", long, 51)
	if !san {
		t.Fatal("a capped name must be flagged")
	}
	name := strings.TrimPrefix(p, "/dir/")
	if len(name) > 51+len(truncMark) || !strings.HasSuffix(name, truncMark) {
		t.Fatalf("capped name %q (%d bytes)", name, len(name))
	}
	if strings.ContainsRune(strings.TrimSuffix(name, truncMark), '�') {
		t.Fatalf("cut inside a rune: %q", name)
	}
	if p, _ := Join("", "x", 10); p != "/x" {
		t.Fatalf("Join at root = %q", p)
	}
}

// tree is a fake directory structure: dir ID -> children.
type tree map[uint64][]core.Node

func (tr tree) list(dir core.Node) ([]core.Node, error) {
	kids, ok := tr[dir.ID]
	if !ok {
		return nil, fmt.Errorf("unreadable %d", dir.ID)
	}
	return kids, nil
}

func dir(id uint64, name string) core.Node { return core.Node{ID: id, Name: name, Kind: core.KindDir} }
func file(id uint64, name string) core.Node {
	return core.Node{ID: id, Name: name, Kind: core.KindFile}
}

type issues []core.Issue

func (is *issues) sink(i core.Issue) { *is = append(*is, i) }
func (is issues) count(c core.IssueCode) (n int) {
	for _, i := range is {
		if i.Code == c {
			n++
		}
	}
	return n
}

func walkAll(t *testing.T, w *TreeWalker, root core.Node) ([]string, error) {
	t.Helper()
	var paths []string
	err := w.Walk(context.Background(), root, func(n core.Node) error {
		paths = append(paths, n.Path)
		return nil
	})
	return paths, err
}

func TestTreeWalkerPreorder(t *testing.T) {
	tr := tree{
		1: {dir(2, "a"), file(3, "z.txt")},
		2: {file(4, "b.txt"), dir(5, "c")},
		5: {file(6, "d.txt")},
	}
	var is issues
	w := &TreeWalker{List: tr.list, Sink: is.sink}
	paths, err := walkAll(t, w, dir(1, "/"))
	if err != nil {
		t.Fatal(err)
	}
	want := "/a /a/b.txt /a/c /a/c/d.txt /z.txt"
	if got := strings.Join(paths, " "); got != want {
		t.Fatalf("order %q, want %q", got, want)
	}
	if len(is) != 0 {
		t.Fatalf("issues on a clean tree: %v", is)
	}
}

func TestTreeWalkerParents(t *testing.T) {
	tr := tree{1: {dir(2, "a")}, 2: {file(3, "b")}}
	parents := map[string]uint64{}
	w := &TreeWalker{List: tr.list}
	_ = w.Walk(context.Background(), dir(1, "/"), func(n core.Node) error {
		parents[n.Path] = n.Parent
		return nil
	})
	if parents["/a"] != 1 || parents["/a/b"] != 2 {
		t.Fatalf("parents %v", parents)
	}
}

func TestTreeWalkerCycle(t *testing.T) {
	// /a/loop names directory 1 (the root) again.
	tr := tree{1: {dir(2, "a")}, 2: {dir(1, "loop"), file(3, "f")}}
	var is issues
	w := &TreeWalker{List: tr.list, Sink: is.sink}
	paths, err := walkAll(t, w, dir(1, "/"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, " ") != "/a /a/loop /a/f" || is.count(core.IssueCycle) != 1 {
		t.Fatalf("paths %v issues %v", paths, is)
	}
}

func TestTreeWalkerUnreadableAndPartial(t *testing.T) {
	partial := errors.New("index block 7 unreadable")
	tr := tree{1: {dir(2, "gone"), dir(3, "part"), file(9, "after")}}
	list := func(d core.Node) ([]core.Node, error) {
		if d.ID == 3 {
			return []core.Node{file(4, "kept")}, partial
		}
		return tr.list(d)
	}
	var is issues
	w := &TreeWalker{List: list, Sink: is.sink}
	paths, err := walkAll(t, w, dir(1, "/"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, " ") != "/gone /part /part/kept /after" {
		t.Fatalf("paths %v", paths)
	}
	if is.count(core.IssueUnreadableDir) != 1 || is.count(core.IssuePartialDir) != 1 {
		t.Fatalf("issues %v", is)
	}
}

func TestTreeWalkerUnreadableRoot(t *testing.T) {
	w := &TreeWalker{List: tree{}.list}
	if _, err := walkAll(t, w, dir(1, "/")); err == nil {
		t.Fatal("an unreadable root must fail the walk")
	}
}

func TestTreeWalkerDeepTree(t *testing.T) {
	// 1,500 nested directories: no recursion, capped at MaxDepth.
	tr := tree{}
	for i := uint64(1); i <= 1500; i++ {
		tr[i] = []core.Node{dir(i+1, "d")}
	}
	var is issues
	w := &TreeWalker{List: tr.list, Sink: is.sink, Limits: core.Limits{MaxDepth: 1000}}
	paths, err := walkAll(t, w, dir(1, "/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1000 || is.count(core.IssueDepthCap) != 1 {
		t.Fatalf("%d nodes, issues %v", len(paths), is)
	}
}

func TestTreeWalkerEntryBudget(t *testing.T) {
	kids := make([]core.Node, 100)
	for i := range kids {
		kids[i] = file(uint64(i+10), fmt.Sprint(i))
	}
	var is issues
	w := &TreeWalker{List: tree{1: kids}.list, Sink: is.sink, Limits: core.Limits{MaxEntries: 40}}
	paths, err := walkAll(t, w, dir(1, "/"))
	if !errors.Is(err, ErrEntryBudget) || len(paths) != 40 || is.count(core.IssueEntryBudget) != 1 {
		t.Fatalf("err %v, %d nodes, issues %v", err, len(paths), is)
	}
}

func TestTreeWalkerSanitizes(t *testing.T) {
	var is issues
	w := &TreeWalker{List: tree{1: {file(2, "a/b")}}.list, Sink: is.sink}
	var got core.Node
	_ = w.Walk(context.Background(), dir(1, "/"), func(n core.Node) error { got = n; return nil })
	if got.Path != "/a%2Fb" || got.Name != "a/b" || !got.Prov.Has(core.ProvNameSanitized) {
		t.Fatalf("node %+v", got)
	}
}

func TestTreeWalkerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tr := tree{1: {file(2, "a"), file(3, "b")}}
	w := &TreeWalker{List: tr.list}
	err := w.Walk(ctx, dir(1, "/"), func(core.Node) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func TestPreorderTracker(t *testing.T) {
	var is issues
	probes := map[uint64]struct {
		n   int
		err error
	}{
		10: {0, nil},                       // genuinely empty
		11: {3, nil},                       // children the library skipped
		12: {0, errors.New("bad cluster")}, // unreadable
	}
	tk := &PreorderTracker{Sink: is.sink, MaxDepth: 3, Probe: func(ref any) (int, error) {
		p := probes[ref.(uint64)]
		return p.n, p.err
	}}
	visit := func(path string, dir bool, key uint64) { tk.Visit(path, dir, key, key) }
	visit("/empty", true, 10)
	visit("/skipped", true, 11)
	visit("/bad", true, 12)
	visit("/full", true, 13)
	visit("/full/x", false, 0)
	visit("/full/loop", true, 13) // the library revisits /full and stops
	visit("/a", true, 20)
	visit("/a/b", true, 21)
	visit("/a/b/c", true, 22) // depth 3: at the cap, not read
	tk.Flush()

	if is.count(core.IssueUnreadableDir) != 2 || is.count(core.IssueCycle) != 1 || is.count(core.IssueDepthCap) != 1 {
		t.Fatalf("issues %v", is)
	}
}

func TestRawPaths(t *testing.T) {
	r := &RawPaths{}
	type step struct {
		raw, name string
		dir       bool
		want      string
	}
	steps := []step{
		{"/a", "a", true, "/a"},
		{"/a/x/y", "x/y", false, "/a/x%2Fy"}, // a name holding '/'
		{"/a/b", "b", true, "/a/b"},
		{"/a/b/c", "c", false, "/a/b/c"},
		{"/z", "z", false, "/z"},
		{"/$OrphanFiles/f", "f", false, "/$OrphanFiles/f"}, // parent never seen
	}
	for _, s := range steps {
		got, _ := r.Join(s.raw, s.name)
		if got != s.want {
			t.Fatalf("Join(%q, %q) = %q, want %q", s.raw, s.name, got, s.want)
		}
		if s.dir {
			r.Enter(s.raw, got)
		}
	}
}

func FuzzEscapeName(f *testing.F) {
	f.Add("a/b")
	f.Add("..")
	f.Add("\xff\x00%")
	f.Fuzz(func(t *testing.T, name string) {
		esc, _ := EscapeName(name)
		if strings.ContainsAny(esc, "/\x00") || esc == "." || esc == ".." || !utf8.ValidString(esc) {
			t.Fatalf("EscapeName(%q) = %q", name, esc)
		}
		for _, r := range esc {
			if isControl(r) {
				t.Fatalf("EscapeName(%q) = %q keeps a control character", name, esc)
			}
		}
	})
}
