package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"indicer/internal/ui"
	"os"
	"strings"
	"sync/atomic"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbopen"
	duesgraph "indicer/lib/graph"
	"indicer/lib/microartefact"
	"indicer/lib/near"
	"indicer/lib/search"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/tui"

	"github.com/dgraph-io/badger/v4"
)

// The micro-artefact screen shows the files with the most artefacts, and at
// most this many artefacts of each (TUI-5).
const (
	tuiMicroFiles   = 500
	tuiMicroPerFile = 20
)

// runTUI runs the TUI; tests replace it.
var runTUI = tui.Run

// leftOpen is given the database TUICmd leaves open; tests close it.
var leftOpen = func(*dbopen.Handle) {}

// TUICmd runs the interactive TUI on a writable open of the database.
func TUICmd(ctx context.Context, opts DBOptions) (err error) {
	// The TUI shows its own status; nothing below reports to a terminal
	// display.
	ctx = progress.Without(ctx)
	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		return err
	}
	if err := util.EnsureBlobPath(h.Root); err != nil {
		closeDB(h, &err)
		return err
	}
	a := &tuiActions{h: h}

	// The TUI owns the terminal: anything else printed would draw over it.
	// Output is silenced once, for the whole session, before any job runs;
	// silencing it per action raced when actions ran side by side.
	restore := console.Quiet()
	cfg := tui.Config{Info: a.info(), Actions: a.actions()}
	if m := ui.Current(); m.Out.TTY {
		cfg.Profile = m.Out.Profile
	}
	err = runTUI(ctx, cfg)
	if errors.Is(err, tui.ErrJobsRunning) {
		// A job did not stop. Closing the database under it could damage
		// it, so it is left for badger to recover at its next open (TUI-3).
		// Output stays silenced: the job may still print.
		leftOpen(h)
		return err
	}
	restore()
	if a.stored.Load() {
		compactValueLog(ctx, h, "TUI")
	}
	closeDB(h, &err)
	return err
}

// tuiActions are the TUI's actions on one open database.
type tuiActions struct {
	h *dbopen.Handle
	// stored is set once a store has run, so the value log is compacted at
	// the end, as after `dues store`.
	stored atomic.Bool
}

func (a *tuiActions) info() tui.Info {
	l := a.h.Manifest.Layout
	return tui.Info{Root: a.h.Root, ChunkKiB: l.ChunkKiB, ChunkHash: l.ChunkHash, Encrypted: a.h.Manifest.Encrypted()}
}

func (a *tuiActions) actions() tui.Actions {
	return tui.Actions{
		Store:   a.store,
		Search:  a.search,
		Restore: a.restore,
		NearIn:  a.nearIn,
		NearOut: a.nearOut,
		Reset:   a.reset,
		List:    a.list,
		Stats:   a.stats,
		Micro:   a.micro,
	}
}

// outcome turns a command's report and error into what the TUI shows: a
// warning (exit 2 on the command line) is a finished action that needs the
// examiner's attention, not a failure.
func outcome(report string, err error) (tui.Outcome, error) {
	var out tui.Outcome
	for l := range strings.SplitSeq(report, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out.Lines = append(out.Lines, l)
		}
	}
	var warning *lifecycle.WarningError
	if errors.As(err, &warning) {
		out.Warning, err = true, nil
	}
	return out, err
}

// store stores a file or folder, as `dues store` does with default options.
func (a *tuiActions) store(ctx context.Context, path string, o tui.StoreOptions) (tui.Outcome, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return tui.Outcome{}, err
	}
	var report bytes.Buffer
	sopts := StoreOptions{NoIndex: o.NoIndex, Decode: true, Quiet: true}
	a.stored.Store(true)
	if fi.IsDir() {
		err = storeFolder(ctx, &report, a.h.DB, path, sopts)
	} else {
		err = storeFile(ctx, &report, a.h.DB, path, sopts)
	}
	return outcome(report.String(), err)
}

func (a *tuiActions) search(ctx context.Context, query string) (tui.Outcome, error) {
	// The TUI replaces the report of its previous search, as it always has.
	o, err := search.SearchReport(ctx, a.h.DB, search.Request{Query: query, RankAlpha: search.DefaultRankAlpha, Force: true})
	if err != nil {
		return tui.Outcome{}, err
	}
	lines, problems := searchSummary(o)
	out := tui.Outcome{Lines: append(lines, o.Notes...)}
	for _, p := range problems {
		out.Lines = append(out.Lines, "Not searched: "+p)
	}
	if w := searchIncomplete(o); w != "" {
		out.Lines = append(out.Lines, "Warning: "+w)
		out.Warning = true
	}
	return out, nil
}

// restore restores to path. An existing file is replaced only once the user
// agreed (TUI-2): without replace, it is tui.ErrTargetExists.
func (a *tuiActions) restore(ctx context.Context, id, path string, replace bool) (tui.Outcome, error) {
	if fi, err := os.Lstat(path); err == nil && !fi.IsDir() && !replace {
		return tui.Outcome{}, tui.ErrTargetExists
	}
	report, err := restoreToPath(ctx, a.h.DB, id, path, RestoreOptions{Force: replace})
	if err != nil {
		return tui.Outcome{}, err
	}
	out := tui.Outcome{Lines: []string{"Restored to " + path, describeVerification(report)}}
	if w := restoreWarning(report); w != "" {
		out.Lines = append(out.Lines, "Warning: "+w)
		out.Warning = true
	}
	return out, nil
}

func (a *tuiActions) nearIn(ctx context.Context, id string, deep bool) (tui.Outcome, error) {
	o, err := near.In(ctx, a.h.DB, id, near.Options{Deep: deep, Force: true})
	return nearOutcome(o, err)
}

func (a *tuiActions) nearOut(ctx context.Context, path string) (tui.Outcome, error) {
	o, err := near.Out(ctx, a.h.DB, path, near.Options{Force: true})
	return nearOutcome(o, err)
}

func nearOutcome(o near.Outcome, err error) (tui.Outcome, error) {
	if err != nil {
		return tui.Outcome{}, err
	}
	out := tui.Outcome{Lines: append([]string{nearSummary(o)}, o.Notes...)}
	if w := nearIncomplete(o); w != "" {
		out.Lines = append(out.Lines, "Warning: "+w)
		out.Warning = true
	}
	return out, nil
}

// reset empties the database the TUI holds open: the entries `dues reset`
// deletes, the graph and full-text index included (ING-16).
func (a *tuiActions) reset(context.Context) error {
	return a.h.Reset()
}

// list lists at most limit stored evidence, in the order they were stored.
func (a *tuiActions) list(ctx context.Context, limit int) (tui.EvidenceList, error) {
	var out tui.EvidenceList
	err := a.h.DB.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			data, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			var e structs.EvidenceFile
			if err := dbio.DecodeRecord(data, &e); err != nil {
				return fmt.Errorf("evidence record %q: %w", it.Item().Key(), err)
			}
			if !e.Completed || e.IngestState == structs.IngestStateDuplicate {
				continue
			}
			out.Total++
			if len(out.Rows) < limit {
				out.Rows = append(out.Rows, evidenceRow(dbio.EvidenceRawID(it.Item().KeyCopy(nil)), e))
			}
		}
		return nil
	})
	return out, err
}

func evidenceRow(id []byte, e structs.EvidenceFile) tui.EvidenceRow {
	row := tui.EvidenceRow{ID: base64.StdEncoding.EncodeToString(id), Name: e.Name, Size: e.Size, Format: "legacy"}
	if e.Source != nil && e.Source.Format != "" {
		row.Format = e.Source.Format
	}
	if e.Index != nil {
		row.Index = string(e.Index.State)
	}
	return row
}

func (a *tuiActions) stats(ctx context.Context) (*store.DBStats, error) {
	return store.GatherStats(ctx, a.h.DB)
}

// micro reads the files with the most micro-artefacts from the graph, opened
// read-only; a database without a graph has none.
func (a *tuiActions) micro(ctx context.Context) (view tui.MicroView, err error) {
	repo, err := microartefact.OpenGrapheneRepositoryReadOnly(a.h.Root)
	if errors.Is(err, duesgraph.ErrNoGraph) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer func() {
		if cerr := repo.Close(); err == nil {
			err = cerr
		}
	}()
	top, err := repo.TopFiles(ctx, tuiMicroFiles, tuiMicroPerFile)
	if err != nil {
		return view, err
	}
	return tui.MicroView{Tree: top.Tree, Files: top.Files, Artefacts: top.Artefacts, Shown: top.Shown, PerFile: tuiMicroPerFile}, nil
}
