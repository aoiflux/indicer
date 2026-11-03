package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	duesgraph "indicer/lib/graph"
	"indicer/lib/util"
	"indicer/tui"

	"github.com/dgraph-io/badger/v4"
)

// tuiActionsOn opens dbPath as TUICmd does and returns its actions.
func tuiActionsOn(t *testing.T, dbPath string) (*tuiActions, tui.Actions) {
	t.Helper()
	h, err := Common(t.Context(), optsFor(dbPath, matrixChunkKB, nil), dbopen.ReadWrite, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	a := &tuiActions{h: h}
	return a, a.actions()
}

func writeFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTUIActions (TUI-2): the TUI's actions report what the commands report,
// list with a cap and the total, and never replace a restore target without
// being told to.
func TestTUIActions(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	t.Chdir(t.TempDir()) // search and near write their reports here
	dir := t.TempDir()
	first := writeFile(t, filepath.Join(dir, "first.bin"), bytes.Repeat([]byte("needle in first "), 4096))
	second := writeFile(t, filepath.Join(dir, "second.bin"), bytes.Repeat([]byte("other content "), 4096))
	again := writeFile(t, filepath.Join(dir, "again.bin"), bytes.Repeat([]byte("needle in first "), 4096))

	a, acts := tuiActionsOn(t, filepath.Join(t.TempDir(), "db"))
	ctx := t.Context()
	if info := a.info(); info.ChunkKiB != matrixChunkKB || info.ChunkHash == "" || info.Encrypted {
		t.Fatalf("info: %+v", info)
	}

	out, err := acts.Store(ctx, first, tui.StoreOptions{})
	if err != nil || out.Warning || !strings.Contains(strings.Join(out.Lines, "\n"), "Stored as evidence ") {
		t.Fatalf("store: %v %+v", err, out)
	}
	if !a.stored.Load() {
		t.Fatal("a store did not mark the value log for compaction")
	}
	if _, err := acts.Store(ctx, second, tui.StoreOptions{NoIndex: true}); err != nil {
		t.Fatal(err)
	}
	out, err = acts.Store(ctx, again, tui.StoreOptions{})
	if err != nil || !strings.Contains(strings.Join(out.Lines, "\n"), "Already stored as evidence") {
		t.Fatalf("store of stored content: %v %+v", err, out)
	}
	if _, err := acts.Store(ctx, filepath.Join(dir, "missing"), tui.StoreOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store of a missing file: %v", err)
	}

	all, err := acts.List(ctx, 5000)
	if err != nil || all.Total != 2 || len(all.Rows) != 2 {
		t.Fatalf("list: %v %+v", err, all)
	}
	capped, err := acts.List(ctx, 1)
	if err != nil || capped.Total != 2 || len(capped.Rows) != 1 || capped.Rows[0] != all.Rows[0] {
		t.Fatalf("list capped at 1: %v %+v", err, capped)
	}
	row := all.Rows[0]
	if row.Name != "first.bin" || row.Size != 16*4096 || row.Format == "" || row.Index == "" {
		t.Fatalf("row: %+v", row)
	}

	target := writeFile(t, filepath.Join(dir, "target.bin"), []byte("keep me"))
	if _, err := acts.Restore(ctx, row.ID, target, false); !errors.Is(err, tui.ErrTargetExists) {
		t.Fatalf("restore over a file: %v, want %v", err, tui.ErrTargetExists)
	}
	if got, _ := os.ReadFile(target); string(got) != "keep me" {
		t.Fatalf("the target was changed: %q", got)
	}
	if _, err := acts.Restore(ctx, "AAAA", target, true); err == nil {
		t.Fatal("restore of an unknown ID succeeded")
	}
	if got, _ := os.ReadFile(target); string(got) != "keep me" {
		t.Fatalf("a failed restore changed the target: %q", got)
	}
	out, err = acts.Restore(ctx, row.ID, target, true)
	if err != nil || out.Warning || len(out.Lines) < 2 || !strings.Contains(out.Lines[1], "match") {
		t.Fatalf("restore: %v %+v", err, out)
	}
	if got, want := mustRead(t, target), mustRead(t, first); !bytes.Equal(got, want) {
		t.Fatal("the restored file differs")
	}

	out, err = acts.Search(ctx, "needle")
	if err != nil || len(out.Lines) < 2 || !strings.Contains(out.Lines[0], "objects hold a match. Report: ") || !strings.HasPrefix(out.Lines[1], "Searched 2 evidence") {
		t.Fatalf("search: %v %+v", err, out)
	}
	if _, err := acts.NearIn(ctx, row.ID, tui.NearOptions{}); err == nil || !strings.Contains(err.Error(), "repair --fix --revrel") {
		t.Fatalf("near without reverse relations: %v", err)
	}
	stats, err := acts.Stats(ctx)
	if err != nil || stats.CompletedFiles != 2 {
		t.Fatalf("stats: %v %+v", err, stats)
	}
	micro, err := acts.Micro(ctx)
	if err != nil || micro.Files != 0 || micro.Tree != nil {
		t.Fatalf("micro without a graph: %v %+v", err, micro)
	}

	if err := acts.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if empty, err := acts.List(ctx, 10); err != nil || empty.Total != 0 {
		t.Fatalf("list after reset: %v %+v", err, empty)
	}
	if _, err := acts.Store(ctx, first, tui.StoreOptions{}); err != nil {
		t.Fatalf("store after reset: %v", err)
	}
}

// TestTUIStoreOptions: a store from the TUI writes what its options ask.
// The TUI's NeAR then finds what the TUI stored, deep NeAR matches similar
// chunks of a file outside the database, Advanced deep runs phase 2, and
// the full-text index and the graph are built. The TUI used to store with
// none of them, so its own NeAR could not search what it had stored.
func TestTUIStoreOptions(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	t.Chdir(t.TempDir()) // near writes its report here
	cs := matrixChunkKB << 10
	content := testutil.RandomBytes(71, 8*cs)
	shares := append(append([]byte(nil), content[:6*cs]...), testutil.RandomBytes(72, 2*cs)...)
	similar := append([]byte(nil), content...)
	for i := 0; i < len(similar); i += cs {
		similar[i+100] ^= 0xff
		similar[i+3000] ^= 0xff
	}
	dir := t.TempDir()
	first := writeFile(t, filepath.Join(dir, "first.bin"), content)
	second := writeFile(t, filepath.Join(dir, "second.bin"), shares)
	outside := writeFile(t, filepath.Join(t.TempDir(), "similar.bin"), similar)
	_, image := fixtureNamed(t, "fat/fat16")

	a, acts := tuiActionsOn(t, filepath.Join(t.TempDir(), "db"))
	ctx := t.Context()
	for _, s := range []struct {
		path string
		opts tui.StoreOptions
	}{
		{first, tui.StoreOptions{RevRel: true, Simhash: true}},
		{second, tui.StoreOptions{RevRel: true, Simhash: true}},
		{image, tui.StoreOptions{RevRel: true, FTS: true, Enrich: true}},
	} {
		if out, err := acts.Store(ctx, s.path, s.opts); err != nil || out.Warning {
			t.Fatalf("store %s: %v %+v", s.path, err, out)
		}
	}
	rows, err := acts.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var firstID string
	for _, r := range rows.Rows {
		if r.Name == "first.bin" {
			firstID = r.ID
		}
	}

	near := func(out tui.Outcome, err error) string {
		t.Helper()
		if err != nil || out.Warning || len(out.Lines) == 0 {
			t.Fatalf("near: %v %+v", err, out)
		}
		return out.Lines[0]
	}
	if got := near(acts.NearIn(ctx, firstID, tui.NearOptions{})); !strings.HasPrefix(got, "Matched 6 of 8 distinct chunks") {
		t.Errorf("near in: %s", got)
	}
	if got := near(acts.NearOut(ctx, outside, tui.NearOptions{})); !strings.HasPrefix(got, "Matched 0 of 8 distinct chunks") {
		t.Errorf("near out without deep: %s", got)
	}
	if got := near(acts.NearOut(ctx, outside, tui.NearOptions{Deep: true})); !strings.HasPrefix(got, "Matched 8 of 8 distinct chunks") {
		t.Errorf("deep near out: %s", got)
	}
	if n := countKeys(t, a.h.DB, cnst.FileSimhashNamespace); n != 0 {
		t.Fatalf("%d full-file signatures cached before phase 2 ran", n)
	}
	near(acts.NearIn(ctx, firstID, tui.NearOptions{Advanced: true}))
	if n := countKeys(t, a.h.DB, cnst.FileSimhashNamespace); n == 0 {
		t.Error("Advanced deep cached no full-file signature: phase 2 did not run")
	}

	root := dbio.KVDir(a.h.DB)
	if _, err := os.Stat(util.FTSPath(root)); err != nil {
		t.Errorf("no full-text index: %v", err)
	}
	if !duesgraph.Exists(root) {
		t.Error("no graph after a store with enrichment")
	}
	if _, err := acts.Micro(ctx); err != nil {
		t.Errorf("micro view: %v", err)
	}
}

func countKeys(t *testing.T, db *badger.DB, prefix string) int {
	t.Helper()
	n := 0
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix, opts.PrefetchValues = []byte(prefix), false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestGraphGate: a store that enriches waits for the graph views already
// reading, and no view starts while it runs; a view that ends lets it go. A
// read-only handle open in the process would make its writable open fail.
func TestGraphGate(t *testing.T) {
	var g graphGate
	reading, release := make(chan struct{}), make(chan struct{})
	viewed := make(chan error, 1)
	go func() {
		viewed <- g.view(func() error {
			close(reading)
			<-release
			return nil
		})
	}()
	<-reading

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.enrich(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled store waiting for a view: %v", err)
	}
	started := make(chan func(), 1)
	go func() {
		done, err := g.enrich(t.Context())
		if err != nil {
			t.Error(err)
		}
		started <- done
	}()
	select {
	case <-started:
		t.Fatal("a store enriched while a view read the graph")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-viewed; err != nil {
		t.Fatal(err)
	}
	done := <-started
	if err := g.view(func() error { t.Error("a view read the graph while a store enriched"); return nil }); !errors.Is(err, errGraphBusy) {
		t.Fatalf("a view while a store enriches: %v", err)
	}
	done()
	if err := g.view(func() error { return nil }); err != nil {
		t.Fatalf("a view after the store: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTUIOutcome: a warning is a finished action that needs attention, not
// a failure; blank lines are dropped.
func TestTUIOutcome(t *testing.T) {
	out, err := outcome("\nStored as evidence A\n\n  Index partial  \n", &lifecycle.WarningError{Err: errors.New("index partial")})
	if err != nil || !out.Warning || len(out.Lines) != 2 || out.Lines[1] != "Index partial" {
		t.Fatalf("warning: %v %+v", err, out)
	}
	failed := errors.New("disk full")
	out, err = outcome("Storing a\n", failed)
	if !errors.Is(err, failed) || out.Warning || len(out.Lines) != 1 {
		t.Fatalf("failure: %v %+v", err, out)
	}
}

// replaceRunTUI replaces the TUI for one test.
func replaceRunTUI(t *testing.T, fn func(context.Context, tui.Config) error) {
	t.Helper()
	saved := runTUI
	runTUI = fn
	t.Cleanup(func() { runTUI = saved })
}

// TestTUICmdClosesTheDatabase: output is silenced while the TUI runs and
// restored after; the database is closed, so it opens again. The TUI gets
// --theme.
func TestTUICmdClosesTheDatabase(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	t.Cleanup(console.Reset)
	t.Cleanup(ui.SetForTest(ui.Mode{Theme: ui.ThemeLight}))
	dbPath := filepath.Join(t.TempDir(), "db")
	src := writeFile(t, filepath.Join(t.TempDir(), "a.bin"), bytes.Repeat([]byte("a"), 10000))
	replaceRunTUI(t, func(ctx context.Context, cfg tui.Config) error {
		if console.Out() != io.Discard || console.Err() != io.Discard {
			t.Error("output is not silenced while the TUI runs")
		}
		if cfg.Theme != ui.ThemeLight {
			t.Errorf("the TUI got theme %v, want light", cfg.Theme)
		}
		if _, err := cfg.Actions.Store(ctx, src, tui.StoreOptions{}); err != nil {
			t.Error(err)
		}
		return nil
	})
	if err := TUICmd(t.Context(), optsFor(dbPath, matrixChunkKB, nil)); err != nil {
		t.Fatal(err)
	}
	if console.Out() == io.Discard {
		t.Fatal("output is still silenced after the TUI")
	}
	h, err := openTestDB(dbPath, nil)
	if err != nil {
		t.Fatalf("open after the TUI: %v", err)
	}
	h.Close()
}

// TestTUICmdLeavesTheDatabaseToAStuckJob (TUI-3): when a job did not stop,
// TUICmd returns the error and does not close the database under it.
func TestTUICmdLeavesTheDatabaseToAStuckJob(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	t.Cleanup(console.Reset)
	dbPath := filepath.Join(t.TempDir(), "db")
	var left *dbopen.Handle
	saved := leftOpen
	leftOpen = func(h *dbopen.Handle) { left = h }
	t.Cleanup(func() {
		leftOpen = saved
		if left != nil {
			left.Close()
		}
	})
	replaceRunTUI(t, func(context.Context, tui.Config) error {
		return fmt.Errorf("%w (store a)", tui.ErrJobsRunning)
	})
	err := TUICmd(t.Context(), optsFor(dbPath, matrixChunkKB, nil))
	if !errors.Is(err, tui.ErrJobsRunning) {
		t.Fatalf("TUICmd: %v, want %v", err, tui.ErrJobsRunning)
	}
	if left == nil || left.DB.IsClosed() {
		t.Fatal("the database was closed under a running job")
	}
	if h, err := openTestDB(dbPath, nil); !errors.Is(err, dbopen.ErrInUse) {
		if err == nil {
			h.Close()
		}
		t.Fatalf("a second open: %v, want %v", err, dbopen.ErrInUse)
	}
}
