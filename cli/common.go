package cli

import (
	"context"
	"errors"
	"fmt"
	"indicer/internal/progress"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/graph"
	"indicer/lib/ingest"
	"indicer/lib/logging"

	"go.uber.org/zap"
)

// DBOptions say which database a command opens and how: the data root (empty
// for ~/.dues), the credentials, and the layout the command line asked for.
type DBOptions struct {
	Path   string
	Creds  dbmeta.Credentials
	Layout dbopen.Requested
	// TempDir is --temp-dir; empty means osdirs.Temp.
	TempDir string
	// Tuning is how the storage engine runs on the databases the command
	// opens; nil takes cnst.DefaultTuning.
	Tuning *cnst.Tuning
	// Graph is how the command's graph handles end (--graph-compact): with
	// AutoCompact, a writable open has the last of them compact a log that
	// has grown past the threshold. A nil Ctx takes the command's.
	Graph graph.Config
	// Actor signs the graph commits of a writable open (--actor); nil is
	// the database's own actor.
	Actor *dbopen.Actor
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

// Request is the open opts describe. The caller sets its mode, and whether a
// writable open recovers the ingests a crash left incomplete.
func (o DBOptions) Request() dbopen.Request {
	return dbopen.Request{Path: o.Path, Creds: o.Creds, Layout: o.Layout, WrittenBy: "dues", TempDir: o.TempDir, Tuning: o.Tuning, Actor: o.Actor}
}

// Common opens the database for a command and prints the open's notices. A
// writable open also finishes or fails ingests a crash left incomplete,
// unless recover is false (repair inspects them first), and configures the
// root's graph with opts.Graph until the database closes. Read-only opens
// change nothing (CFG-11).
func Common(ctx context.Context, opts DBOptions, mode dbopen.Mode, recover bool) (*dbopen.Handle, error) {
	req := opts.Request()
	req.Mode, req.Recover = mode, recover
	h, err := dbopen.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	if mode != dbopen.ReadOnly && opts.Graph.AutoCompact {
		cfg := opts.Graph
		if cfg.Ctx == nil {
			cfg.Ctx = ctx
		}
		remove, err := graph.Configure(h.Root, cfg)
		if err != nil {
			return nil, errors.Join(err, h.Close())
		}
		h.AtClose(remove)
	}
	for _, n := range h.Notices {
		ui.Notef("%s", n)
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

// closeSession closes a command's ingest session, the full-text index and
// graph it opened, and adds a close error to *errp, as closeDB does.
func closeSession(s *ingest.Session, errp *error) {
	if cerr := s.Close(); cerr != nil {
		*errp = errors.Join(*errp, fmt.Errorf("closing the full-text index and graph: %w", cerr))
	}
}

// compactValueLog reclaims value-log space after a command that wrote. The
// data is already committed, so a failure is only logged (CFG-9), and shown
// as a warning on the compaction's progress phase. It can take up to half a
// minute, so it shows as a phase of its own.
func compactValueLog(ctx context.Context, h *dbopen.Handle, command string) {
	ph := progress.Start(ctx, "compact", progress.None, 0)
	defer ph.Done(nil)
	n, err := dbio.CompactValueLog(h.DB)
	if err != nil {
		logging.GetLogger().Warn(command+" VALUE_LOG_GC_ERROR", zap.Error(err))
		ph.Warn("the value log was not compacted: " + err.Error())
		return
	}
	logging.GetLogger().Info(command+" VALUE_LOG_GC", zap.Int("files_rewritten", n))
}

// encryptionNotice is the one-line D8 notice list and stats show for an
// unencrypted database.
func encryptionNotice(h *dbopen.Handle) {
	if !h.Manifest.Encrypted() {
		ui.Notef("database %s is not encrypted", h.Root)
	}
}
