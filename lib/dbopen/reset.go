package dbopen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/fsx"
)

// ErrResetRefused is a reset refused because of what the path is.
var ErrResetRefused = errors.New("refusing to reset")

// ownedEntries are the entries of a data root that DUES creates. A reset
// deletes these and nothing else (ING-16).
var ownedEntries = []string{
	cnst.KVDBDIR, cnst.BLOBSDIR, cnst.GRAPHDIR, cnst.FTSDIR, cnst.UploadsDir, dbmeta.ManifestName,
}

// CheckResettable reports why the data root must not be reset: it holds no
// DUES database, or it is a filesystem root, the home directory or the
// working directory, or contains one of them. root must be absolute.
func CheckResettable(root string) error {
	if filepath.Dir(root) == root {
		return fmt.Errorf("%w %s: it is a filesystem root", ErrResetRefused, root)
	}
	places := map[string]func() (string, error){
		"your home directory":   os.UserHomeDir,
		"the working directory": os.Getwd,
	}
	for what, get := range places {
		dir, err := get()
		if err != nil {
			continue
		}
		if contains(root, dir) {
			return fmt.Errorf("%w %s: it is or contains %s (%s)", ErrResetRefused, root, what, dir)
		}
	}
	if !dbmeta.HasDatabase(root) {
		return fmt.Errorf("%w %s: it holds no DUES database", ErrResetRefused, root)
	}
	return nil
}

// Reset deletes the DUES database whose data root path names, after
// CheckResettable. It holds the root lock throughout, so it fails with
// ErrInUse while any DUES process has the database open, and it deletes only
// the entries DUES owns: anything else in the root is left, and so is the
// root itself unless that leaves it empty. It returns the root and the
// entries it removed.
func Reset(path string) (root string, removed []string, err error) {
	root, err = ResolveRoot(path)
	if err != nil {
		return "", nil, err
	}
	if err := CheckResettable(root); err != nil {
		return root, nil, err
	}
	lockPath := filepath.Join(root, LockName)
	lock, err := fsx.TryLock(lockPath, false)
	if errors.Is(err, fsx.ErrLocked) {
		return root, nil, fmt.Errorf("%w (%s)", ErrInUse, root)
	}
	if err != nil {
		return root, nil, err
	}

	removed, err = removeOwned(root, ownedEntries)
	if uerr := lock.Unlock(); err == nil {
		err = uerr
	}
	if err != nil {
		return root, removed, err
	}
	if rerr := os.Remove(lockPath); rerr == nil {
		removed = append(removed, LockName)
	}
	// Only an empty root goes: os.Remove refuses a directory with anything in it.
	_ = os.Remove(root)
	return root, removed, nil
}

// Reset empties the open database for a caller that keeps using it (the
// TUI): badger's contents, blobs, graph, full-text index and uploads. The
// manifest, layout and key stay, and the handle remains usable.
func (h *Handle) Reset() error {
	if h.ReadOnly {
		return errors.New("cannot reset a database opened read-only")
	}
	if err := h.DB.DropAll(); err != nil {
		return err
	}
	// DropAll removed the in-database copy of the manifest; write it again.
	if err := h.checkMarker(); err != nil {
		return err
	}
	if _, err := removeOwned(h.Root, []string{cnst.BLOBSDIR, cnst.GRAPHDIR, cnst.FTSDIR, cnst.UploadsDir}); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(h.Root, cnst.BLOBSDIR), cnst.DirPerm)
}

// removeOwned deletes the named entries of root, and the DUES temporary files
// at its top level.
func removeOwned(root string, names []string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		owned := fsx.IsTempName(e.Name())
		for _, n := range names {
			owned = owned || samePathName(e.Name(), n)
		}
		if !owned {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}

// contains reports whether dir is root or inside it.
func contains(root, dir string) bool {
	rel, err := filepath.Rel(canonical(root), canonical(dir))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// canonical resolves symlinks where it can and folds case on Windows.
func canonical(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return filepath.Clean(p)
}

func samePathName(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
