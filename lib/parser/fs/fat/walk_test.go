package fat

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/aoiflux/libfat"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
)

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	for _, m := range fixture.All(t) {
		if m.Name == name {
			b, err := os.ReadFile(fixture.Open(t, m))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("no fixture %s", name)
	return nil
}

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

// entries lists the live records of a FAT image by path.
func entries(t *testing.T, data []byte) (*libfat.Volume, map[string]libfat.DirEntry) {
	t.Helper()
	vol, err := libfat.Open(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]libfat.DirEntry{}
	if err := vol.Walk(context.Background(), func(path string, _ uint32, e libfat.DirEntry) error {
		out[path] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return vol, out
}

// TestWalkReportsSkippedDirectories: every directory libfat reports but does
// not descend into is an issue on that directory (FAT-U1), and the walk goes
// on past it.
func TestWalkReportsSkippedDirectories(t *testing.T) {
	t.Parallel()
	data := fixtureBytes(t, "fat/fat32_fsagen")
	size := int64(len(data))
	vol, ents := entries(t, data)

	t.Run("depth limit", func(t *testing.T) {
		w := walk(t, bytes.NewReader(data), size, core.Limits{MaxDepth: 2})
		if !w.issue(core.IssueDepthCap, "/deep/one") {
			t.Fatalf("no depth cap on /deep/one; issues %v", w.issues)
		}
		if _, ok := w.nodes["/deep/one/two"]; ok {
			t.Fatal("the walk went past its depth limit")
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		one := ents["/deep/one"]
		off, err := vol.ClusterToOffset(one.FirstCluster)
		if err != nil {
			t.Fatal(err)
		}
		r := failRange{r: bytes.NewReader(data), lo: off, hi: off + int64(vol.BytesPerCluster())}
		w := walk(t, r, size, core.Limits{})
		if !w.issue(core.IssueUnreadableDir, "/deep/one") {
			t.Fatalf("no unreadable_dir on /deep/one; issues %v", w.issues)
		}
		if _, ok := w.nodes["/docs"]; !ok {
			t.Fatal("the walk stopped at the unreadable directory")
		}
	})

	t.Run("cycle", func(t *testing.T) {
		img := bytes.Clone(data)
		deep, two := ents["/deep"], ents["/deep/one/two"]
		// Short entry fields: first cluster high word at 20, low word at 26.
		ent := img[two.EntryAbsoluteOffset : two.EntryAbsoluteOffset+32]
		binary.LittleEndian.PutUint16(ent[20:], uint16(deep.FirstCluster>>16))
		binary.LittleEndian.PutUint16(ent[26:], uint16(deep.FirstCluster))
		w := walk(t, bytes.NewReader(img), size, core.Limits{})
		if !w.issue(core.IssueCycle, "/deep/one/two") {
			t.Fatalf("no cycle on /deep/one/two; issues %v", w.issues)
		}
	})
}

// TestWalkKeepsLiveDirectoryAfterDeletedOne: a deleted directory record that
// names the first cluster of a live directory listed after it must not hide
// the live directory's subtree. That is what removing one directory and
// creating another leaves on disk; libfat v0.3.3 lost the subtree, v0.3.5
// does not.
func TestWalkKeepsLiveDirectoryAfterDeletedOne(t *testing.T) {
	t.Parallel()
	data := fixtureBytes(t, "fat/fat32_fsagen")
	_, ents := entries(t, data)
	deep := ents["/deep"]
	// A live file in the root listed before /deep becomes a deleted
	// directory record naming /deep's cluster.
	var victim libfat.DirEntry
	for p, e := range ents {
		if !e.IsDirectory && !e.Virtual && e.EntryAbsoluteOffset > 0 && e.EntryAbsoluteOffset < deep.EntryAbsoluteOffset &&
			len(p) > 1 && !bytes.ContainsRune([]byte(p[1:]), '/') {
			if victim.EntryAbsoluteOffset == 0 || e.EntryAbsoluteOffset < victim.EntryAbsoluteOffset {
				victim = e
			}
		}
	}
	if victim.EntryAbsoluteOffset == 0 {
		t.Fatal("fixture has no root file listed before /deep")
	}
	img := bytes.Clone(data)
	ent := img[victim.EntryAbsoluteOffset : victim.EntryAbsoluteOffset+32]
	ent[0] = 0xE5
	ent[11] = 0x10 // directory
	binary.LittleEndian.PutUint16(ent[20:], uint16(deep.FirstCluster>>16))
	binary.LittleEndian.PutUint16(ent[26:], uint16(deep.FirstCluster))
	binary.LittleEndian.PutUint32(ent[28:], 0)

	w := walk(t, bytes.NewReader(img), int64(len(img)), core.Limits{})
	for _, p := range []string{"/deep/one", "/deep/one/two", "/deep/one/two/three"} {
		if _, ok := w.nodes[p]; !ok {
			t.Errorf("%s is missing", p)
		}
	}
	for _, i := range w.issues {
		if i.Code == core.IssueCycle {
			t.Errorf("a deleted record's reused cluster reported as a cycle: %v", i)
		}
	}
}
