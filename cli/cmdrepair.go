package cli

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/fio"
	"indicer/lib/fsx"
	"indicer/lib/ingest"
	"indicer/lib/logging"
	"indicer/lib/store"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

// RepairOptions selects what repair changes. With none set it only inspects.
type RepairOptions struct {
	Fix     bool // finish what interrupted ingests left: the recovery table (--fix)
	Upgrade bool // move a legacy block index into the database (--upgrade)
	Yes     bool // delete the moved block index files without asking (--yes)
	Verify  bool // read back and check every stored chunk (--verify)
	// ReindexFailed rebuilds the index of every evidence whose index pass
	// failed or was interrupted (--reindex-failed, with --fix).
	ReindexFailed bool
	// AddRevrel adds reverse relations, which near needs, to every stored
	// evidence stored without them (--revrel, with --fix).
	AddRevrel bool
}

// repairDocument is what repair prints on stdout once the database is open:
// the recovery report, a section for each step a flag ran, and the error the
// command ends with. The steps' own lines and the prompt go to stderr.
type repairDocument struct {
	store.RepairReport
	Upgrade *upgradeSection `json:"upgrade,omitempty"`
	// Quarantined is a pointer so that a quarantine that found nothing is an
	// empty list, and one that did not run is absent.
	Quarantined *[]quarantinedIndex `json:"quarantined,omitempty"`
	Revrel      *revrelSection      `json:"revrel,omitempty"`
	Reindex     *reindexSummary     `json:"reindex,omitempty"`
	Verify      *store.VerifyReport `json:"verify,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// upgradeSection is what --upgrade did.
type upgradeSection struct {
	store.BlockIndexUpgrade
	RecordedAsContainer bool `json:"recordedAsContainer"`
	// BlockFiles is none, kept or deleted; BlockFileCount, how many there were.
	BlockFiles     string `json:"blockFiles"`
	BlockFileCount int    `json:"blockFileCount"`
}

// quarantinedIndex is one evidence whose index --upgrade quarantined.
type quarantinedIndex struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Format string `json:"format"`
}

// revrelSection is what --revrel did.
type revrelSection struct {
	Evidence int   `json:"evidence"` // evidence that lacked reverse relations
	Added    int64 `json:"added"`    // reverse relations written
}

// addReverseRelations gives every stored evidence without reverse relations
// what near needs to find its chunks (SRC-12).
func addReverseRelations(ctx context.Context, h *dbopen.Handle) (*revrelSection, error) {
	evidence, added, err := store.AddMissingReverseRelations(ctx, h.DB)
	console.Errorf("Reverse relations: %d evidence lacked them; %d added.\n", evidence, added)
	return &revrelSection{Evidence: evidence, Added: added}, err
}

// quarantineContainerIndexes hides the indexes ingests before W6 built over
// the raw bytes of EWF and VHD containers (ING-7).
func quarantineContainerIndexes(ctx context.Context, h *dbopen.Handle) (_ *[]quarantinedIndex, err error) {
	q, err := store.QuarantineRawContainerIndexes(ctx, h.DB)
	if err != nil {
		return nil, err
	}
	rows := make([]quarantinedIndex, 0, len(q))
	for _, e := range q {
		rows = append(rows, quarantinedIndex{ID: base64.StdEncoding.EncodeToString(e.ID), Name: e.Name, Format: string(e.Format)})
	}
	if len(q) == 0 {
		console.Errorln("No index built over raw container bytes.")
		return &rows, nil
	}
	console.Errorf("Quarantined %d index(es) built over raw container bytes; store each evidence again to index its decoded disk:\n", len(q))
	// One session: the graph opens once for every quarantined index.
	sess := ingest.NewSession(h.DB)
	defer closeSession(sess, &err)
	for _, e := range q {
		console.Errorf("  %s (%s)\n", termsafe.Line(e.Name), e.Format)
		// The graph hides the index too, as list does: its partitions and
		// files go, the evidence node stays.
		if err := sess.DeleteGraphIndex(e.ID, true); err != nil {
			return &rows, fmt.Errorf("remove the quarantined index of %s from the graph: %w", termsafe.Line(e.Name), err)
		}
	}
	return &rows, nil
}

// VerifyError reports evidence that failed `repair --verify`.
type VerifyError struct {
	Problems, Verified int
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("%d of %d evidence failed verification", e.Problems, e.Problems+e.Verified)
}

func RepairData(ctx context.Context, opts DBOptions, ro RepairOptions) error {
	return repairData(ctx, opts, ro, os.Stdin, console.Interactive())
}

func repairData(ctx context.Context, opts DBOptions, ro RepairOptions, in io.Reader, interactive bool) (err error) {
	start := time.Now()
	logging.GetLogger().Info("RepairData START",
		zap.String("db_path", opts.Path),
		zap.Bool("fix", ro.Fix),
		zap.Bool("upgrade", ro.Upgrade),
	)

	// Without --fix or --upgrade repair only inspects. With them, repair
	// itself decides what to change, so the automatic startup recovery stays
	// off (CFG-11).
	mode := dbopen.ReadOnly
	if ro.ReindexFailed && !ro.Fix {
		return lifecycle.Usagef("--reindex-failed needs --fix")
	}
	if ro.AddRevrel && !ro.Fix {
		return lifecycle.Usagef("--revrel needs --fix")
	}
	if ro.Fix || ro.Upgrade {
		mode = dbopen.ReadWrite
	}
	h, err := Common(ctx, opts, mode, false)
	if err != nil {
		logging.GetLogger().Error("RepairData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	// Deferred before closeDB, so it runs after it: the document is printed
	// whichever step fails, and carries a failed close too.
	var doc repairDocument
	defer func() { err = printRepairDocument(&doc, err) }()
	defer closeDB(h, &err)

	logging.GetLogger().Info("RepairData REPAIR_EXECUTE", zap.Bool("fix", ro.Fix))
	doc.RepairReport, err = store.InspectEvidenceRepairs(ctx, h.DB, ro.Fix)
	if err != nil {
		logging.GetLogger().Error("RepairData REPAIR_ERROR", zap.Error(err), zap.Bool("fix", ro.Fix))
		return err
	}
	if ro.Upgrade {
		if doc.Upgrade, err = upgradeBlockIndex(ctx, h, ro.Yes, in, interactive); err != nil {
			logging.GetLogger().Error("RepairData UPGRADE_ERROR", zap.Error(err))
			return err
		}
		if doc.Quarantined, err = quarantineContainerIndexes(ctx, h); err != nil {
			logging.GetLogger().Error("RepairData QUARANTINE_ERROR", zap.Error(err))
			return err
		}
	}
	if ro.AddRevrel {
		if doc.Revrel, err = addReverseRelations(ctx, h); err != nil {
			logging.GetLogger().Error("RepairData REVREL_ERROR", zap.Error(err))
			return err
		}
	}
	if ro.ReindexFailed {
		sum, rerr := reindexEvidence(ctx, h.DB, ReindexOptions{Failed: true}, console.Err())
		doc.Reindex = &sum
		if err = rerr; err != nil {
			logging.GetLogger().Error("RepairData REINDEX_ERROR", zap.Error(err))
			return err
		}
	}
	if ro.Verify {
		if doc.Verify, err = verifyEvidence(ctx, h); err != nil {
			logging.GetLogger().Error("RepairData VERIFY_ERROR", zap.Error(err))
			return err
		}
	}

	if ro.Fix || ro.Upgrade {
		compactValueLog(ctx, h, "RepairData")
	}
	logging.GetLogger().Info("RepairData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Bool("fix", ro.Fix),
		zap.Bool("upgrade", ro.Upgrade),
	)
	return nil
}

// printRepairDocument prints doc on stdout with err, the error repair ends
// with, and returns err, or the print's own error when there was none.
func printRepairDocument(doc *repairDocument, err error) error {
	if err != nil {
		doc.Error = err.Error()
	}
	data, perr := json.MarshalIndent(doc, "", "  ")
	if perr == nil {
		_, perr = fmt.Fprintf(console.Out(), "%s\n", data)
	}
	if err != nil {
		return err
	}
	return perr
}

// verifyEvidence reads back every stored chunk of every completed or flushed
// evidence, prints a line for each evidence, and fails when any is damaged.
func verifyEvidence(ctx context.Context, h *dbopen.Handle) (*store.VerifyReport, error) {
	report, err := store.VerifyEvidence(ctx, h.DB)
	if report.Results == nil {
		report.Results = []store.VerifyResult{}
	}
	if err != nil {
		return &report, err
	}
	p := ui.For(console.Err())
	for _, r := range report.Results {
		if r.Problem == "" {
			console.Errorf("%s %s  %s (%d bytes)\n", p.Strong(ui.Success, "OK     "), p.Paint(ui.Evidence, r.ID), termsafe.Line(r.Name), r.Size)
		} else {
			console.Errorf("%s %s  %s (%d bytes): %s\n", p.Strong(ui.Error, "DAMAGED"), p.Paint(ui.Evidence, r.ID), termsafe.Line(r.Name), r.Size, termsafe.Line(r.Problem))
		}
	}
	console.Errorf("Verified %d evidence; %d damaged.\n", report.Verified, report.Problems)
	if report.Problems > 0 {
		return &report, &VerifyError{Problems: report.Problems, Verified: report.Verified}
	}
	return &report, nil
}

// upgradeBlockIndex moves the block index of a database written in the
// retired hierarchical mode into C||| (D9), records the database as
// container storage once every record has moved, and deletes the block
// files when the user confirms.
func upgradeBlockIndex(ctx context.Context, h *dbopen.Handle, yes bool, in io.Reader, interactive bool) (*upgradeSection, error) {
	files, err := fio.BlockIndexFiles(h.Root)
	if err != nil {
		return nil, err
	}
	s := &upgradeSection{BlockFiles: "none", BlockFileCount: len(files)}
	if len(files) > 0 {
		s.BlockFiles = "kept"
	}
	legacy := h.Manifest.Layout.Storage == dbmeta.StorageContainerBIDX
	if len(files) == 0 && !legacy {
		console.Errorln("No legacy block index to upgrade.")
		return s, nil
	}

	s.BlockIndexUpgrade, err = store.UpgradeBlockIndex(ctx, h.DB)
	if err != nil {
		return s, err
	}
	console.Errorf("Block index: %d files, %d records: %d moved into the database, %d already there, %d damaged.\n",
		s.Files, s.Records, s.Migrated, s.AlreadyStored, s.DamagedCount)
	if !s.Complete() {
		for _, d := range s.Damaged {
			console.Errorf("  damaged: %s\n", termsafe.Line(d))
		}
		return s, fmt.Errorf("%d block index records could not be read or verified; the block index is kept and still used", s.DamagedCount)
	}
	if legacy {
		if err := h.SetStorage(dbmeta.StorageContainer); err != nil {
			return s, err
		}
		s.RecordedAsContainer = true
		console.Errorln("The database is now recorded as container storage.")
	}
	if len(files) == 0 {
		return s, nil
	}

	if !yes {
		if !interactive {
			console.Errorf("Kept %d block index files; the database no longer needs them. Run again with --yes to delete them.\n", len(files))
			return s, nil
		}
		console.Errorf("Delete the %d block index files? Their records are in the database now. [y/N] ", len(files))
		line, _ := bufio.NewReader(in).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			console.Errorln("Kept the block index files.")
			return s, nil
		}
	}
	for i, f := range files {
		if err := fsx.Remove(f); err != nil {
			return s, fmt.Errorf("deleted %d of %d block index files: %w", i, len(files), err)
		}
	}
	_ = os.Remove(filepath.Dir(files[0])) // the blocks directory, once empty
	cnst.LegacyBlockIndex = false
	s.BlockFiles = "deleted"
	console.Errorf("Deleted %d block index files.\n", len(files))
	return s, nil
}
