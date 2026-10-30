package cli

import (
	"bufio"
	"context"
	"fmt"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
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

// addReverseRelations gives every stored evidence without reverse relations
// what near needs to find its chunks (SRC-12).
func addReverseRelations(ctx context.Context, h *dbopen.Handle) error {
	evidence, added, err := store.AddMissingReverseRelations(ctx, h.DB)
	console.Printf("Reverse relations: %d evidence lacked them; %d added.\n", evidence, added)
	return err
}

// quarantineContainerIndexes hides the indexes ingests before W6 built over
// the raw bytes of EWF and VHD containers (ING-7).
func quarantineContainerIndexes(ctx context.Context, h *dbopen.Handle) error {
	q, err := store.QuarantineRawContainerIndexes(ctx, h.DB)
	if err != nil {
		return err
	}
	if len(q) == 0 {
		console.Println("No index built over raw container bytes.")
		return nil
	}
	console.Printf("Quarantined %d index(es) built over raw container bytes; store each evidence again to index its decoded disk:\n", len(q))
	for _, e := range q {
		console.Printf("  %s (%s)\n", termsafe.Line(e.Name), e.Format)
		// The graph hides the index too, as list does: its partitions and
		// files go, the evidence node stays.
		if err := ingest.DeleteGraphIndex(h.DB, e.ID, true); err != nil {
			return fmt.Errorf("remove the quarantined index of %s from the graph: %w", termsafe.Line(e.Name), err)
		}
	}
	return nil
}

// VerifyError reports evidence that failed `repair --verify`.
type VerifyError struct {
	Problems, Verified int
}

func (e *VerifyError) Error() string {
	return fmt.Sprintf("%d of %d evidence failed verification", e.Problems, e.Problems+e.Verified)
}

func RepairData(ctx context.Context, opts DBOptions, ro RepairOptions) error {
	return repairData(ctx, opts, ro, os.Stdin, isTerminal(os.Stdin))
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
	defer closeDB(h, &err)

	logging.GetLogger().Info("RepairData REPAIR_EXECUTE", zap.Bool("fix", ro.Fix))
	err = store.Repair(ctx, h.DB, ro.Fix)
	if err != nil {
		logging.GetLogger().Error("RepairData REPAIR_ERROR", zap.Error(err), zap.Bool("fix", ro.Fix))
		return err
	}
	if ro.Upgrade {
		if err = upgradeBlockIndex(ctx, h, ro.Yes, in, interactive); err != nil {
			logging.GetLogger().Error("RepairData UPGRADE_ERROR", zap.Error(err))
			return err
		}
		if err = quarantineContainerIndexes(ctx, h); err != nil {
			logging.GetLogger().Error("RepairData QUARANTINE_ERROR", zap.Error(err))
			return err
		}
	}
	if ro.AddRevrel {
		if err = addReverseRelations(ctx, h); err != nil {
			logging.GetLogger().Error("RepairData REVREL_ERROR", zap.Error(err))
			return err
		}
	}
	if ro.ReindexFailed {
		if err = reindexEvidence(ctx, h.DB, ReindexOptions{Failed: true}); err != nil {
			logging.GetLogger().Error("RepairData REINDEX_ERROR", zap.Error(err))
			return err
		}
	}
	if ro.Verify {
		if err = verifyEvidence(ctx, h); err != nil {
			logging.GetLogger().Error("RepairData VERIFY_ERROR", zap.Error(err))
			return err
		}
	}

	if ro.Fix || ro.Upgrade {
		compactValueLog(h, "RepairData")
	}
	logging.GetLogger().Info("RepairData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Bool("fix", ro.Fix),
		zap.Bool("upgrade", ro.Upgrade),
	)
	return nil
}

// verifyEvidence reads back every stored chunk of every completed or flushed
// evidence, prints a line for each evidence, and fails when any is damaged.
func verifyEvidence(ctx context.Context, h *dbopen.Handle) error {
	report, err := store.VerifyEvidence(ctx, h.DB)
	if err != nil {
		return err
	}
	for _, r := range report.Results {
		if r.Problem == "" {
			console.Printf("OK      %s  %s (%d bytes)\n", r.ID, termsafe.Line(r.Name), r.Size)
		} else {
			console.Printf("DAMAGED %s  %s (%d bytes): %s\n", r.ID, termsafe.Line(r.Name), r.Size, r.Problem)
		}
	}
	console.Printf("Verified %d evidence; %d damaged.\n", report.Verified, report.Problems)
	if report.Problems > 0 {
		return &VerifyError{Problems: report.Problems, Verified: report.Verified}
	}
	return nil
}

// upgradeBlockIndex moves the block index of a database written in the
// retired hierarchical mode into C||| (D9), records the database as
// container storage once every record has moved, and deletes the block
// files when the user confirms.
func upgradeBlockIndex(ctx context.Context, h *dbopen.Handle, yes bool, in io.Reader, interactive bool) error {
	files, err := fio.BlockIndexFiles(h.Root)
	if err != nil {
		return err
	}
	legacy := h.Manifest.Layout.Storage == dbmeta.StorageContainerBIDX
	if len(files) == 0 && !legacy {
		console.Println("No legacy block index to upgrade.")
		return nil
	}

	report, err := store.UpgradeBlockIndex(ctx, h.DB)
	if err != nil {
		return err
	}
	console.Printf("Block index: %d files, %d records: %d moved into the database, %d already there, %d damaged.\n",
		report.Files, report.Records, report.Migrated, report.AlreadyStored, report.DamagedCount)
	if !report.Complete() {
		for _, d := range report.Damaged {
			console.Printf("  damaged: %s\n", d)
		}
		return fmt.Errorf("%d block index records could not be read or verified; the block index is kept and still used", report.DamagedCount)
	}
	if legacy {
		if err := h.SetStorage(dbmeta.StorageContainer); err != nil {
			return err
		}
		console.Println("The database is now recorded as container storage.")
	}
	if len(files) == 0 {
		return nil
	}

	if !yes {
		if !interactive {
			console.Printf("Kept %d block index files; the database no longer needs them. Run again with --yes to delete them.\n", len(files))
			return nil
		}
		console.Printf("Delete the %d block index files? Their records are in the database now. [y/N] ", len(files))
		line, _ := bufio.NewReader(in).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			console.Println("Kept the block index files.")
			return nil
		}
	}
	for _, f := range files {
		if err := fsx.Remove(f); err != nil {
			return err
		}
	}
	_ = os.Remove(filepath.Dir(files[0])) // the blocks directory, once empty
	cnst.LegacyBlockIndex = false
	console.Printf("Deleted %d block index files.\n", len(files))
	return nil
}
