package cli

import (
	"context"
	"errors"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/store"

	"github.com/fatih/color"
	"go.uber.org/zap"
)

// DBOptions say which database a command opens and how: the data root (empty
// for ~/.dues), the credentials, and the layout the command line asked for.
type DBOptions struct {
	Path   string
	Creds  dbmeta.Credentials
	Layout dbopen.Requested
}

// LegacyKeyOptions builds DBOptions from the old (chunk size, path, key)
// arguments, for callers that still hold a derived key. The key unlocks only
// databases written before manifests existed; a new database made this way
// is unencrypted.
func LegacyKeyOptions(chonkSize int, dbpath string, key []byte) DBOptions {
	creds := dbmeta.RawKey(key)
	if len(key) == 0 {
		creds = dbmeta.NoPassword()
	}
	return DBOptions{Path: dbpath, Creds: creds, Layout: dbopen.Requested{ChunkKiB: chonkSize}}
}

// Common opens the database for a command. A writable open also finishes or
// fails ingests a crash left incomplete, unless recover is false (repair
// inspects them first). Read-only opens change nothing (CFG-11).
func Common(ctx context.Context, opts DBOptions, mode dbopen.Mode, recover bool) (*dbopen.Handle, error) {
	h, err := dbopen.Open(ctx, dbopen.Request{
		Path:      opts.Path,
		Mode:      mode,
		Creds:     opts.Creds,
		Layout:    opts.Layout,
		WrittenBy: "dues",
	})
	if err != nil {
		return nil, err
	}
	for _, n := range h.Notices {
		fmt.Fprintln(console.Err(), "dues: note:", n)
	}

	// Recommend container mode for small chunk sizes
	if l := h.Manifest.Layout; h.Created && l.ChunkKiB < 128 && !cnst.CONTAINERMODE {
		color.Yellow("\n⚠️  RECOMMENDATION: You're using a chunk size of %dKB (< 128KB).", l.ChunkKiB)
		color.Yellow("   Consider using --container flag for better filesystem efficiency.")
		color.Yellow("   Container mode packs multiple small chunks into 1GB compressed files.\n")
		console.Println()
	}

	if h.ReadOnly || !recover {
		return h, nil
	}
	recoveryReport, err := store.RecoverIncompleteIngests(h.DB)
	if err != nil {
		return nil, errors.Join(err, h.Close())
	}
	if recoveryReport.MarkedFailed > 0 {
		logging.GetLogger().Warn("Common STARTUP_RECOVERY_MARKED_FAILED",
			zap.Int("marked_failed", recoveryReport.MarkedFailed),
			zap.Int("incomplete_found", recoveryReport.IncompleteFound),
		)
	}
	return h, nil
}

// closeDB closes h and adds a close error to *errp, so a command that
// otherwise succeeded still reports a database that did not close cleanly
// (CFG-8).
func closeDB(h *dbopen.Handle, errp *error) {
	if cerr := h.Close(); cerr != nil {
		*errp = errors.Join(*errp, fmt.Errorf("closing the database: %w", cerr))
	}
}

// compactValueLog reclaims value-log space after a command that wrote. The
// data is already committed, so a failure is only logged (CFG-9).
func compactValueLog(h *dbopen.Handle, command string) {
	n, err := dbio.CompactValueLog(h.DB)
	if err != nil {
		logging.GetLogger().Warn(command+" VALUE_LOG_GC_ERROR", zap.Error(err))
		return
	}
	logging.GetLogger().Info(command+" VALUE_LOG_GC", zap.Int("files_rewritten", n))
}

// encryptionNotice is the one-line D8 notice list and stats show for an
// unencrypted database.
func encryptionNotice(h *dbopen.Handle) {
	if !h.Manifest.Encrypted() {
		fmt.Fprintf(console.Err(), "dues: note: database %s is not encrypted\n", h.Root)
	}
}
