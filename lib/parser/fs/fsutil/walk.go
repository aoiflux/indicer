package fsutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"indicer/lib/parser/core"
)

// ErrEntryBudget ends a walk that reached Limits.MaxEntries. The nodes
// already visited stand; the partition is partial.
var ErrEntryBudget = errors.New("fsutil: walk entry budget reached")

// truncMark ends a name cut to Limits.MaxName.
const truncMark = "%TRUNC"

// EscapeName makes a raw filesystem name safe as one path component. A name
// needs escaping when it holds '/', NUL or another control character
// (including C1 and DEL), invalid UTF-8, or is "." or "..". Such a name is
// percent-encoded byte by byte, '%' included, and sanitized is true; every
// other name is returned unchanged, so ordinary names (even ones with '%')
// keep their paths.
func EscapeName(name string) (escaped string, sanitized bool) {
	if name == "." || name == ".." {
		return strings.Repeat("%2E", len(name)), true
	}
	if !needsEscape(name) {
		return name, false
	}
	var b strings.Builder
	for i := 0; i < len(name); {
		r, size := utf8.DecodeRuneInString(name[i:])
		bad := (r == utf8.RuneError && size == 1) || r == '/' || r == '%' || isControl(r)
		if bad {
			for _, c := range []byte(name[i : i+size]) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		} else {
			b.WriteString(name[i : i+size])
		}
		i += size
	}
	return b.String(), true
}

func needsEscape(name string) bool {
	if !utf8.ValidString(name) {
		return true
	}
	for _, r := range name {
		if r == '/' || isControl(r) {
			return true
		}
	}
	return false
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

// Join returns parent + "/" + the escaped name, cutting the name to maxName
// bytes (on a rune boundary, marked with truncMark) when maxName > 0.
// sanitized reports whether the name was escaped or cut.
func Join(parent, name string, maxName int) (path string, sanitized bool) {
	esc, sanitized := EscapeName(name)
	if maxName > 0 && len(esc) > maxName {
		cut := maxName
		for cut > 0 && !utf8.RuneStart(esc[cut]) {
			cut--
		}
		esc = esc[:cut] + truncMark
		sanitized = true
	}
	return strings.TrimSuffix(parent, "/") + "/" + esc, sanitized
}

// TreeWalker walks a directory tree in preorder without recursion, for
// filesystems whose adapter lists one directory at a time (NTFS, ext, XFS,
// HFS+). Memory grows with the names on the current path, not with depth
// squared: each directory on the stack keeps its own listing and a link to its
// parent's path.
//
// A directory is listed once: a second visit (a hard-linked directory, a
// corrupt pointer back up the tree) is reported as a cycle and not entered.
// A directory below Limits.MaxDepth is reported, not entered. A listing that
// fails is reported and skipped; a listing that fails part way keeps what it
// returned. The walk stops at Limits.MaxEntries with ErrEntryBudget.
type TreeWalker struct {
	// List returns a directory's children. Returning children with an error
	// means the listing is partial. Children carry ID, Name and Kind; the
	// walker sets Path and Parent.
	List func(dir core.Node) ([]core.Node, error)
	// Descend reports whether to enter a child directory. The default enters
	// live directories.
	Descend func(core.Node) bool
	// Key identifies a directory for cycle detection. The default is its ID.
	Key    func(core.Node) uint64
	Sink   core.Sink
	Limits core.Limits
}

type pathNode struct {
	parent *pathNode
	name   string // escaped component
}

func (p *pathNode) String() string {
	if p == nil || p.parent == nil {
		return "/"
	}
	var parts []string
	for n := p; n.parent != nil; n = n.parent {
		parts = append(parts, n.name)
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		b.WriteByte('/')
		b.WriteString(parts[i])
	}
	return b.String()
}

type frame struct {
	dir   core.Node
	kids  []core.Node
	i     int
	path  *pathNode
	depth int
}

func (w *TreeWalker) report(i core.Issue) {
	if w.Sink != nil {
		w.Sink(i)
	}
}

// Walk lists root and every directory below it, calling fn for each node
// (root itself is not passed to fn). An unreadable root is an error.
func (w *TreeWalker) Walk(ctx context.Context, root core.Node, fn func(core.Node) error) error {
	lim := w.Limits.WithDefaults()
	descend := w.Descend
	if descend == nil {
		descend = func(n core.Node) bool { return n.Kind == core.KindDir && !n.IsDeleted }
	}
	key := w.Key
	if key == nil {
		key = func(n core.Node) uint64 { return n.ID }
	}

	kids, err := w.List(root)
	if err != nil && len(kids) == 0 {
		return fmt.Errorf("list root directory: %w", err)
	}
	if err != nil {
		w.report(core.Issue{Code: core.IssuePartialDir, Path: "/", Inode: root.ID, Detail: err.Error()})
	}
	visited := map[uint64]struct{}{key(root): {}}
	rootPath := &pathNode{}
	stack := []*frame{{dir: root, kids: kids, path: rootPath}}
	var emitted int64

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		top := stack[len(stack)-1]
		if top.i >= len(top.kids) {
			stack[len(stack)-1] = nil
			stack = stack[:len(stack)-1]
			continue
		}
		n := top.kids[top.i]
		top.kids[top.i] = core.Node{} // release the listing as it is consumed
		top.i++
		if n.Name == "" {
			continue
		}

		esc, san := Join("", n.Name, lim.MaxName)
		comp := &pathNode{parent: top.path, name: esc[1:]}
		n.Path = comp.String()
		n.Parent = top.dir.ID
		if san {
			n.Prov |= core.ProvNameSanitized
		}

		if emitted >= lim.MaxEntries {
			w.report(core.Issue{Code: core.IssueEntryBudget, Path: n.Path, Detail: fmt.Sprintf("stopped after %d entries", emitted)})
			return ErrEntryBudget
		}
		emitted++
		if err := fn(n); err != nil {
			return err
		}

		if !descend(n) {
			continue
		}
		depth := top.depth + 1
		k := key(n)
		if _, seen := visited[k]; seen {
			w.report(core.Issue{Code: core.IssueCycle, Path: n.Path, Inode: n.ID, Detail: "directory already visited"})
			continue
		}
		if depth >= lim.MaxDepth {
			w.report(core.Issue{Code: core.IssueDepthCap, Path: n.Path, Inode: n.ID, Detail: fmt.Sprintf("depth %d", depth)})
			continue
		}
		visited[k] = struct{}{}
		kids, err := w.List(n)
		switch {
		case err != nil && len(kids) == 0:
			w.report(core.Issue{Code: core.IssueUnreadableDir, Path: n.Path, Inode: n.ID, Detail: err.Error()})
			continue
		case err != nil:
			w.report(core.Issue{Code: core.IssuePartialDir, Path: n.Path, Inode: n.ID, Detail: err.Error()})
		}
		stack = append(stack, &frame{dir: n, kids: kids, path: comp, depth: depth})
	}
	return nil
}

// EscapePath escapes each component of a raw '/'-separated path, for paths a
// library assembled itself.
func EscapePath(raw string) string {
	var b strings.Builder
	for _, part := range strings.Split(raw, "/") {
		if part == "" {
			continue
		}
		esc, _ := EscapeName(part)
		b.WriteString("/" + esc)
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

// RawPaths escapes the paths a library's own preorder walk reports (libfat,
// libxfat), which join raw names with '/'. It keeps the escaped path of each
// directory on the current branch, so a child's path is its parent's escaped
// path plus its own escaped name, even when names contain '/'.
type RawPaths struct {
	MaxName int
	stack   []rawFrame
}

type rawFrame struct{ raw, esc string }

// Join returns the escaped path of the entry the library reported at raw
// with the given name. Call Enter after it for a directory, so its children
// find it.
func (r *RawPaths) Join(raw, name string) (path string, sanitized bool) {
	parentRaw := strings.TrimSuffix(raw, "/"+name)
	if parentRaw == raw {
		parentRaw = raw[:strings.LastIndex(raw, "/")+1]
		parentRaw = strings.TrimSuffix(parentRaw, "/")
	}
	for len(r.stack) > 0 && r.stack[len(r.stack)-1].raw != parentRaw {
		r.stack = r.stack[:len(r.stack)-1]
	}
	parent := ""
	if len(r.stack) > 0 {
		parent = r.stack[len(r.stack)-1].esc
	} else if parentRaw != "" {
		parent = strings.TrimSuffix(EscapePath(parentRaw), "/")
	}
	return Join(parent, name, r.MaxName)
}

// Enter records a directory just returned by Join.
func (r *RawPaths) Enter(raw, path string) {
	r.stack = append(r.stack, rawFrame{raw: raw, esc: path})
}
