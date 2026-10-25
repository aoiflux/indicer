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

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbopen"
	"indicer/tui"
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
	if _, err := acts.NearIn(ctx, row.ID, false); err == nil || !strings.Contains(err.Error(), "repair --fix --revrel") {
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
// restored after; the database is closed, so it opens again.
func TestTUICmdClosesTheDatabase(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	t.Cleanup(console.Reset)
	dbPath := filepath.Join(t.TempDir(), "db")
	src := writeFile(t, filepath.Join(t.TempDir(), "a.bin"), bytes.Repeat([]byte("a"), 10000))
	replaceRunTUI(t, func(ctx context.Context, cfg tui.Config) error {
		if console.Out() != io.Discard || console.Err() != io.Discard {
			t.Error("output is not silenced while the TUI runs")
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
