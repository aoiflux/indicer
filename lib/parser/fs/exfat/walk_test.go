package exfat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/aoiflux/libxfat"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
)

// failRange fails every read that touches [lo, hi).
type failRange struct {
	r      io.ReaderAt
	lo, hi int64
}

var errFailRange = errors.New("injected read failure")

func (f failRange) ReadAt(p []byte, off int64) (int, error) {
	if off < f.hi && off+int64(len(p)) > f.lo {
		return 0, errFailRange
	}
	return f.r.ReadAt(p, off)
}

type walked struct {
	nodes  map[string]core.Node
	issues []core.Issue
}

func (w walked) issue(code core.IssueCode, path string) bool {
	for _, i := range w.issues {
		if i.Code == code && i.Path == path {
			return true
		}
	}
	return false
}

func walk(t *testing.T, r io.ReaderAt, size int64, lim core.Limits) walked {
	t.Helper()
	w := walked{nodes: map[string]core.Node{}}
	fsys, err := Open(core.OpenParams{Reader: r, Size: size, ImageSize: size, Limits: lim,
		Sink: func(i core.Issue) { w.issues = append(w.issues, i) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.Walk(context.Background(), func(n core.Node) error {
		w.nodes[n.Path] = n
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestWalkReportsSkippedDirectories: every directory libxfat reports but
// does not descend into, or reads only part of, is an issue on that
// directory (XFAT-U1), and the walk goes on past it.
func TestWalkReportsSkippedDirectories(t *testing.T) {
	t.Parallel()
	var data []byte
	for _, m := range fixture.All(t) {
		if m.Name == "exfat/exfat" {
			b, err := os.ReadFile(fixture.Open(t, m))
			if err != nil {
				t.Fatal(err)
			}
			data = b
		}
	}
	if data == nil {
		t.Fatal("no exfat fixture")
	}
	size := int64(len(data))

	t.Run("depth limit", func(t *testing.T) {
		w := walk(t, bytes.NewReader(data), size, core.Limits{MaxDepth: 1})
		if !w.issue(core.IssueDepthCap, "/sub") {
			t.Fatalf("no depth cap on /sub; issues %v", w.issues)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		fsys, err := Open(core.OpenParams{Reader: bytes.NewReader(data), Size: size, ImageSize: size})
		if err != nil {
			t.Fatal(err)
		}
		ex := fsys.(*exfatFS).ex
		var sub libxfat.Entry
		var subKids int
		if err := ex.WalkWithOptions(context.Background(), libxfat.WalkOptions{}, func(path string, _ uint32, e libxfat.Entry) error {
			switch {
			case path == "/sub":
				sub = e
			case strings.HasPrefix(path, "/sub/"):
				subKids++
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if subKids == 0 {
			t.Fatal("fixture's /sub is empty")
		}
		off, err := ex.ClusterOffset(sub.FirstCluster())
		if err != nil {
			t.Fatal(err)
		}
		r := failRange{r: bytes.NewReader(data), lo: int64(off), hi: int64(off + ex.ClusterSize())}
		w := walk(t, r, size, core.Limits{})
		if !w.issue(core.IssueUnreadableDir, "/sub") && !w.issue(core.IssuePartialDir, "/sub") {
			t.Fatalf("/sub's lost entries not reported; issues %v", w.issues)
		}
	})
}
