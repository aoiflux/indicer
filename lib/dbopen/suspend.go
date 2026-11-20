package dbopen

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"indicer/internal/diskspace"
	"indicer/lib/cnst"
	"indicer/lib/fts"
	"indicer/lib/graph"
	"indicer/lib/graph/signer"
	"indicer/lib/logging"

	"go.uber.org/zap"
)

// Suspended is a writable database closed for a while with its root still
// locked exclusively, so no DUES process opens it and its files stay as they
// are for Snapshot. The handle keeps the manifest and the keys, so Resume
// needs no password.
type Suspended struct {
	h *Handle
	// unclean is set when badger did not close cleanly: its last writes may
	// be only in its memtable logs, which a copy would not replay.
	unclean bool
}

// ResumeOptions say how Resume reopens the database.
type ResumeOptions struct {
	// Recover finishes or fails the ingests left incomplete, as
	// Request.Recover does.
	Recover bool
	// KeepUploads keeps the part files of uploads, which a writable open
	// removes: a server receives uploads while its database is suspended.
	KeepUploads bool
}

var errNotSuspended = errors.New("the database is no longer suspended")

// Suspend closes the database and keeps its root locked until Resume or
// Release. The graph and the full-text index, which an ingest session holds,
// must be closed first: their files would change under a copy. h stays the
// handle Resume returns; Close on it ends the suspension, as Release.
//
// The database is closed even when closing badger fails. The error is then
// returned with the Suspended: Snapshot refuses a database that did not
// close cleanly, and Resume recovers it.
func (h *Handle) Suspend() (*Suspended, error) {
	if h == nil || h.DB == nil {
		return nil, errors.New("the database is not open")
	}
	// The ingest session names the graph and the index by the badger
	// directory, which a root itself named kvdb does not spell.
	kvdb := filepath.Join(h.Root, cnst.KVDBDIR)
	switch {
	case h.ReadOnly:
		return nil, fmt.Errorf("database %s is open read-only; only a writable open is suspended", h.Root)
	case graph.IsOpen(kvdb):
		return nil, fmt.Errorf("database %s still has its graph open; close it before suspending", h.Root)
	case fts.IsOpen(kvdb):
		return nil, fmt.Errorf("database %s still has its full-text index open; close it before suspending", h.Root)
	}
	db := h.DB
	h.DB = nil
	s := &Suspended{h: h}
	h.suspension = s
	unregister(db)
	if err := db.Close(); err != nil {
		s.unclean = true
		logging.GetLogger().Warn("database suspended without closing cleanly", zap.String("root", h.Root), zap.Error(err))
		return s, fmt.Errorf("closing database %s: %w", h.Root, err)
	}
	logging.GetLogger().Info("database suspended", zap.String("root", h.Root))
	return s, nil
}

func (s *Suspended) held() error {
	if s == nil || s.h == nil || s.h.suspension != s {
		return errNotSuspended
	}
	return nil
}

// Snapshot is the suspended root to copy. The suspension holds the lock, so
// releasing the snapshot does nothing. A database that did not close cleanly
// is refused: its last writes are not in its files yet.
func (s *Suspended) Snapshot() (*Snapshot, error) {
	if err := s.held(); err != nil {
		return nil, err
	}
	h := s.h
	if s.unclean || hasMemtableLogs(filepath.Join(h.Root, cnst.KVDBDIR)) {
		return nil, fmt.Errorf("%w: %s did not close cleanly; resuming it recovers it", ErrNeedsRecovery, h.Root)
	}
	return &Snapshot{Root: h.Root, Manifest: h.Manifest}, nil
}

// Actor is the actor the handle was opened to sign as (Request.Actor), or
// "" for the database's own.
func (s *Suspended) Actor() string {
	if s.held() != nil || s.h.actor == nil {
		return ""
	}
	return s.h.actor.Name
}

// TakeSigner resolves the actor the handle signs with, making the database
// actor's key when it has none, as a writable open does. The caller closes
// the provider.
func (s *Suspended) TakeSigner() (signer.Provider, error) {
	if err := s.held(); err != nil {
		return nil, err
	}
	h := s.h
	h.signMu.Lock()
	defer h.signMu.Unlock()
	return resolveSigner(h.Root, h.actor, h.actorKey, true)
}

// Resume reopens the database with the kept keys and returns its handle, the
// one suspended. When it fails, the database stays suspended: Resume may be
// tried again, or Release called.
func (s *Suspended) Resume(ctx context.Context, opts ResumeOptions) (*Handle, error) {
	if err := s.held(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h := s.h
	if err := diskspace.Check(h.Root, 0); err != nil {
		return nil, err
	}
	if err := h.openBadger(h.Manifest, h.keys); err != nil {
		return nil, err
	}
	err := h.register(h.keys)
	if err == nil {
		h.sweepTemps(opts.KeepUploads)
		if opts.Recover {
			err = h.recoverIngests(ctx)
		}
	}
	if err != nil {
		db := h.DB
		h.DB = nil
		unregister(db)
		return nil, errors.Join(err, db.Close())
	}
	h.suspension = nil
	logging.GetLogger().Info("database resumed", zap.String("root", h.Root), zap.Bool("kept_uploads", opts.KeepUploads))
	return h, nil
}

// Release closes the suspended database for good: it zeroes the keys and
// releases the root lock. After Resume, or a second time, it does nothing.
func (s *Suspended) Release() error {
	if s.held() != nil {
		return nil
	}
	return s.h.Close()
}
