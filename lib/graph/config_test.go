package graph

import (
	"context"
	"path/filepath"
	"testing"

	"indicer/internal/diskspace"
	"indicer/lib/graph/schema"
	"indicer/lib/util"
)

// autoCompacting configures root's graph with cfg until the test ends.
func autoCompacting(t *testing.T, root string, cfg Config) {
	t.Helper()
	remove, err := Configure(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
}

// segmentCount is how many log segments compactions have retired in root's
// graph. A new store has one, its empty first image's.
func segmentCount(t *testing.T, root string) int {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(util.GraphPath(root), segmentPrefix+"*"+segmentSuffix))
	if err != nil {
		t.Fatal(err)
	}
	return len(segs)
}

// imageNodes is how many nodes the marker records in root's image.
func imageNodes(t *testing.T, root string) int {
	t.Helper()
	m, found, err := schema.ReadMarker(util.GraphPath(root))
	if err != nil || !found || m.Image == nil {
		t.Fatalf("marker: found %v, image %v, %v", found, m.Image, err)
	}
	return m.Image.Nodes
}

// writeAndRelease opens root's graph, writes n nodes and releases it.
func writeAndRelease(t *testing.T, root string, n int) {
	t.Helper()
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	addNodes(t, h, "a", n)
	if err := h.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// TestReleaseCompactsWhenDue: the last Release of a writable handle on a root
// configured for it compacts a log that has reached the threshold. The image
// holds the nodes, the marker records it, and the log is kept as a segment.
func TestReleaseCompactsWhenDue(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	autoCompacting(t, root, Config{AutoCompact: true, CompactAt: Threshold{Bytes: 1}})
	writeAndRelease(t, root, 3)
	if got := imageNodes(t, root); got != 3 {
		t.Fatalf("image holds %d nodes, want 3", got)
	}
	if got := segmentCount(t, root); got != 2 {
		t.Fatalf("%d segments, want the first image's and the compacted log", got)
	}
}

// TestReleaseLeavesTheStoreAlone: a Release compacts nothing when the root is
// not configured, is configured not to, or the log is under the threshold.
func TestReleaseLeavesTheStoreAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *Config
	}{
		{"not configured", nil},
		{"never", &Config{CompactAt: Threshold{Bytes: 1}}},
		{"small log", &Config{AutoCompact: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanRegistry(t)
			root := t.TempDir()
			if tc.cfg != nil {
				autoCompacting(t, root, *tc.cfg)
			}
			writeAndRelease(t, root, 3)
			if got := imageNodes(t, root); got != 0 {
				t.Fatalf("image holds %d nodes, want the empty first image", got)
			}
			if got := segmentCount(t, root); got != 1 {
				t.Fatalf("%d segments, want 1", got)
			}
		})
	}
}

// TestOnlyTheLastReleaseCompacts: a Release that leaves a reference compacts
// nothing; the last one does.
func TestOnlyTheLastReleaseCompacts(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	autoCompacting(t, root, Config{AutoCompact: true, CompactAt: Threshold{Bytes: 1}})
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	addNodes(t, first, "a", 2)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if got := segmentCount(t, root); got != 1 {
		t.Fatalf("%d segments after the first Release, want 1", got)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if got := segmentCount(t, root); got != 2 {
		t.Fatalf("%d segments after the last Release, want 2", got)
	}
}

// TestReadOnlyReleaseNeverCompacts: a read-only handle's Release compacts
// nothing, whatever the root is configured to do.
func TestReadOnlyReleaseNeverCompacts(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	writeAndRelease(t, root, 3)
	autoCompacting(t, root, Config{AutoCompact: true, CompactAt: Threshold{Bytes: 1}})
	h, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if got := segmentCount(t, root); got != 1 {
		t.Fatalf("%d segments, want 1", got)
	}
}

// TestCancelledReleaseSkipsCompaction: once the command's context is
// cancelled, Release starts no compaction.
func TestCancelledReleaseSkipsCompaction(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	autoCompacting(t, root, Config{AutoCompact: true, CompactAt: Threshold{Bytes: 1}, Ctx: ctx})
	writeAndRelease(t, root, 3)
	if got := segmentCount(t, root); got != 1 {
		t.Fatalf("%d segments, want 1", got)
	}
}

// TestRefusedCompactionDoesNotFailRelease: a compaction the volume refuses
// is a warning. Release succeeds, and the store opens with what was written.
func TestRefusedCompactionDoesNotFailRelease(t *testing.T) {
	cleanRegistry(t)
	prev := checkSpace
	checkSpace = func(path string, need uint64) error { return &diskspace.LowSpaceError{Path: path, Need: need} }
	t.Cleanup(func() { checkSpace = prev })

	root := t.TempDir()
	autoCompacting(t, root, Config{AutoCompact: true, CompactAt: Threshold{Bytes: 1}})
	writeAndRelease(t, root, 3)
	if got := segmentCount(t, root); got != 1 {
		t.Fatalf("%d segments, want 1", got)
	}
	h, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if got := nodeCount(t, h); got != 3 {
		t.Fatalf("%d nodes after the refused compaction, want 3", got)
	}
}

// TestConfigureStacks: a later Configure of a root takes over until it is
// removed, and removing one twice is harmless.
func TestConfigureStacks(t *testing.T) {
	root := t.TempDir()
	outer, err := Configure(root, Config{AutoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := Configure(filepath.Join(root, "."), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg, ok := Configured(root); !ok || cfg.AutoCompact {
		t.Fatalf("configured %+v, %v; want the inner configuration", cfg, ok)
	}
	inner()
	inner()
	if cfg, ok := Configured(root); !ok || !cfg.AutoCompact {
		t.Fatalf("configured %+v, %v; want the outer configuration", cfg, ok)
	}
	outer()
	if _, ok := Configured(root); ok {
		t.Fatal("root still configured after both were removed")
	}
}
