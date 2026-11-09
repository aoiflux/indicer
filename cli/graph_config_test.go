package cli

import (
	"context"
	"path/filepath"
	"testing"

	"indicer/internal/console"
	"indicer/internal/testutil"
	"indicer/lib/dbopen"
	"indicer/lib/graph"
	"indicer/lib/util"
	"indicer/tui"
)

// graphSegments is how many log segments compactions have retired in the
// graph of the data root at root. A new store has one, its empty first
// image's.
func graphSegments(t *testing.T, root string) int {
	t.Helper()
	segs, err := filepath.Glob(filepath.Join(util.GraphPath(root), "graphene.*.wal"))
	if err != nil {
		t.Fatal(err)
	}
	return len(segs)
}

// TestStoreFolderCompactsTheGraphOnce: a folder store that leaves the graph's
// log past the threshold compacts it once, when its session releases the
// graph, not once per evidence; with AutoCompact off it compacts nothing.
// Either way the root's configuration goes with the database.
func TestStoreFolderCompactsTheGraphOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		auto     bool
		segments int
	}{
		{"auto", true, 2},
		{"never", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			in := filepath.Join(dir, "in")
			for i, name := range []string{"a.bin", "b.bin", "sub/c.bin"} {
				testutil.WriteRandomFile(t, in, name, uint64(51+i), 5000)
			}
			root := filepath.Join(dir, "db")
			opts := optsFor(root, matrixChunkKB, nil)
			opts.Graph = graph.Config{AutoCompact: tc.auto, CompactAt: graph.Threshold{Bytes: 1}}
			if err := StoreData(t.Context(), opts, in, false, false, false, true); err != nil {
				t.Fatal(err)
			}
			if got := graphSegments(t, root); got != tc.segments {
				t.Fatalf("%d segments, want %d", got, tc.segments)
			}
			if cfg, ok := graph.Configured(root); ok {
				t.Fatalf("the graph is still configured after the store: %+v", cfg)
			}
		})
	}
}

// TestCommonConfiguresTheGraph: a writable open configures the root's graph
// with the command's context until the database closes; a read-only open
// leaves it unconfigured.
func TestCommonConfiguresTheGraph(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "db")
	opts := optsFor(root, matrixChunkKB, nil)
	opts.Graph = graph.Config{AutoCompact: true}

	ctx := t.Context()
	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, ok := graph.Configured(root); !ok || !cfg.AutoCompact || cfg.Ctx != ctx {
		t.Errorf("configured %+v, %v; want auto compaction with the command's context", cfg, ok)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Configured(root); ok {
		t.Fatal("the graph is still configured after the database closed")
	}

	h, err = Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if cfg, ok := graph.Configured(root); ok {
		t.Fatalf("a read-only open configured the graph: %+v", cfg)
	}
}

// TestTUINeverCompactsTheGraph: the TUI leaves the root's graph unconfigured,
// whatever --graph-compact says, so its stores never end in a compaction.
func TestTUINeverCompactsTheGraph(t *testing.T) {
	t.Cleanup(console.Reset)
	root := filepath.Join(t.TempDir(), "db")
	replaceRunTUI(t, func(context.Context, tui.Config) error {
		if cfg, ok := graph.Configured(root); ok {
			t.Errorf("the TUI's graph is configured: %+v", cfg)
		}
		return nil
	})
	opts := optsFor(root, matrixChunkKB, nil)
	opts.Graph = graph.Config{AutoCompact: true}
	if err := TUICmd(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
}
