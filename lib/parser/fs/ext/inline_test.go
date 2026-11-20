package ext

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/aoiflux/libext"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
)

// readRecorder remembers the offset of the last read of exactly n bytes.
type readRecorder struct {
	r    *bytes.Reader
	n    int
	mu   sync.Mutex
	last int64
}

func (r *readRecorder) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == r.n {
		r.mu.Lock()
		r.last = off
		r.mu.Unlock()
	}
	return r.r.ReadAt(p, off)
}

// inodeOffset returns where inode ino's record lies in img.
func inodeOffset(t *testing.T, img []byte, ino uint32) int64 {
	t.Helper()
	probe, err := libext.OpenWithOptions(bytes.NewReader(img), libext.Options{ImageSize: uint64(len(img)), Permissive: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := &readRecorder{r: bytes.NewReader(img), n: int(probe.Superblock().InodeSize), last: -1}
	fs, err := libext.OpenWithOptions(rec, libext.Options{ImageSize: uint64(len(img)), Permissive: true})
	if err != nil {
		t.Fatal(err)
	}
	rec.last = -1
	if _, err := fs.ReadInode(ino); err != nil || rec.last < 0 {
		t.Fatalf("locating inode %d: %v", ino, err)
	}
	return rec.last
}

// TestForgedInlineSizeIsRefused is the PAR-35 regression: libext v0.4.1
// allocated an inline inode's claimed size before reading it, so one flipped
// bit in the size's high word asked for 192 GiB and the process died out of
// memory. libext v0.4.2 refuses the claim; every adapter path that reaches
// inline data must pass that refusal on.
func TestForgedInlineSizeIsRefused(t *testing.T) {
	t.Parallel()
	var m fixture.Manifest
	for _, f := range fixture.All(t) {
		if f.Name == "ext/ext4" {
			m = f
		}
	}
	clean, err := os.ReadFile(fixture.Open(t, m))
	if err != nil {
		t.Fatal(err)
	}
	open := func(img []byte) *extFS {
		t.Helper()
		f, err := Open(core.OpenParams{Reader: bytes.NewReader(img), Size: int64(len(img)), ImageSize: int64(len(img))})
		if err != nil {
			t.Fatal(err)
		}
		return f.(*extFS)
	}
	nodes := map[string]core.Node{}
	a := open(clean)
	if err := a.Walk(context.Background(), func(n core.Node) error {
		nodes[n.Path] = n
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	img := bytes.Clone(clean)
	var forged []string
	for _, p := range []string{"/inline.txt", "/sub/deeper", "/slowlink"} {
		n, ok := nodes[p]
		if !ok {
			t.Fatalf("fixture has no %s", p)
		}
		ino, err := a.fs.ReadInode(uint32(n.ID))
		if err != nil {
			t.Fatal(err)
		}
		if !ino.HasInline {
			continue
		}
		// libext reads a directory's size from the low word alone (the high
		// word is ext2's i_dir_acl), so a directory can claim up to 4 GiB.
		at := inodeOffset(t, clean, uint32(n.ID)) + libext.InodeOffSizeHi
		if n.Kind == core.KindDir {
			at = inodeOffset(t, clean, uint32(n.ID)) + libext.InodeOffSizeLo + 3
		}
		img[at] ^= 0x30
		forged = append(forged, p)
	}
	t.Logf("forged %v", forged)
	if len(forged) < 2 {
		t.Fatalf("only %v are inline in the fixture; want the file and the directory", forged)
	}

	b := open(img)
	for _, p := range forged {
		n := nodes[p]
		switch n.Kind {
		case core.KindDir:
			if _, err := b.list(n); !errors.Is(err, libext.ErrUnsupportedLayout) {
				t.Errorf("listing %s: %v, want ErrUnsupportedLayout", p, err)
			}
		case core.KindSymlink:
			info, err := b.Stat(n)
			if err != nil || info.Target != "" {
				t.Errorf("stat of %s: target %q, %v; want no target and no error", p, info.Target, err)
			}
		default:
			if _, err := b.Content(n); !errors.Is(err, libext.ErrUnsupportedLayout) {
				t.Errorf("content of %s: %v, want ErrUnsupportedLayout", p, err)
			}
			if _, err := readAll(b, n); !errors.Is(err, libext.ErrUnsupportedLayout) {
				t.Errorf("reading %s: %v, want ErrUnsupportedLayout", p, err)
			}
		}
	}
	// The whole walk survives too, and the intact files still read.
	if err := b.Walk(context.Background(), func(core.Node) error { return nil }); err != nil {
		t.Fatalf("walk of the forged image: %v", err)
	}
	if c, err := b.Content(nodes["/sub/a.bin"]); err != nil || c.Size != 5000 {
		t.Fatalf("an intact file: %+v, %v", c, err)
	}
}

// readAll reads a node through Open.
func readAll(a *extFS, n core.Node) ([]byte, error) {
	rc, err := a.Open(n)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
