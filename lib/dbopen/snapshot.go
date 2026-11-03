package dbopen

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/fsx"
)

// Snapshot is a data root held still while its files are copied: the root
// lock is held shared, so no DUES process writes the root until Release.
// Read-only opens may run beside it.
type Snapshot struct {
	Root     string
	Manifest dbmeta.Manifest
	// Notices are one-line messages for the user, as Handle's.
	Notices []string

	lock *fsx.Lock
}

// TakeSnapshot holds the data root path names for copying. It refuses a
// root a DUES writer holds (ErrInUse), one with no database
// (ErrNoDatabase), a legacy database without a manifest, and a database
// that was not closed cleanly (ErrNeedsRecovery), whose files would be
// copied with their last writes not yet applied. It opens nothing else:
// copying the files needs no badger.
func TakeSnapshot(ctx context.Context, path string) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := ResolveRoot(path)
	if err != nil {
		return nil, err
	}
	if !dbmeta.HasDatabase(root) || !hasRecords(root) {
		return nil, fmt.Errorf("%w at %s", ErrNoDatabase, root)
	}
	h := &Handle{Root: root, ReadOnly: true}
	if err := h.takeLock(); err != nil {
		return nil, err
	}
	s := &Snapshot{Root: root, Notices: h.Notices, lock: h.lock}
	if s.Manifest, err = dbmeta.Load(root); err != nil {
		_ = s.Release()
		if errors.Is(err, dbmeta.ErrNoManifest) {
			return nil, fmt.Errorf("database %s was made before manifests existed; open it once with a command that writes, such as `dues repair --fix`, to give it one", root)
		}
		return nil, fmt.Errorf("reading the manifest of %s: %w", root, err)
	}
	if hasMemtableLogs(filepath.Join(root, cnst.KVDBDIR)) {
		_ = s.Release()
		return nil, fmt.Errorf("%w: %s was not closed cleanly; run `dues repair --fix` with write access", ErrNeedsRecovery, root)
	}
	return s, nil
}

// Release lets writers back in. Calling it again does nothing.
func (s *Snapshot) Release() error {
	if s == nil || s.lock == nil {
		return nil
	}
	l := s.lock
	s.lock = nil
	return l.Unlock()
}
