package dbpackage

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"indicer/internal/diskspace"
	"indicer/internal/duesarc"
	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/fsx"
	"indicer/lib/graph/schema"
	"indicer/lib/store"
)

const (
	// stageMarker names the directory an import writes in, beside its
	// target: .<target>.dues-import-<random>.
	stageMarker = ".dues-import-"
	// stageLockName is the lock an import holds in its directory, so a
	// sweep never removes one in use.
	stageLockName = ".dues-import.lock"
	// staleStageAge is how old an unlocked import directory must be before
	// an import of the same target removes it.
	staleStageAge = 10 * time.Minute
)

// ImportOptions say what to import and where.
type ImportOptions struct {
	// Package is the package's path, or a part or the sidecar of a split one.
	Package string
	// Path is the target data root, as for dbopen.Request. It must be
	// missing or empty.
	Path string
	// Creds, when set, open an encrypted database to check it once its
	// files are in place.
	Creds dbmeta.Credentials
	// TempDir is --temp-dir, for the read-only open that checks it.
	TempDir string
	// Tuning is how the storage engine runs on the database it opens; nil
	// takes cnst.DefaultTuning.
	Tuning *cnst.Tuning
}

// ImportResult is what an import made.
type ImportResult struct {
	Root     string
	Manifest duesarc.DatabaseManifest
	// Opened is set when the imported database was opened to check it.
	Opened bool
	// Notices are one-line messages for the user.
	Notices []string
}

// Import makes a new data root from a package. The package's files are
// written into a directory beside the target and checked against the
// manifest as they are written, the database is opened read-only to check
// it when it can be, and only then does the directory become the target. A
// failure, or an interruption, leaves no target and removes the directory.
func Import(ctx context.Context, opts ImportOptions) (res ImportResult, err error) {
	root, err := dbopen.ResolveRoot(opts.Path)
	if err != nil {
		return res, err
	}
	res.Root = root
	if err := checkTarget(root); err != nil {
		return res, err
	}
	p, err := Open(opts.Package)
	if err != nil {
		return res, err
	}
	defer p.Close()
	m := p.Manifest
	res.Manifest = m
	if err := checkReadable(m); err != nil {
		return res, err
	}
	if p.zip.In.Verified() {
		if err := p.zip.In.Verify(ctx); err != nil {
			return res, err
		}
	}
	entries, bad := p.entries()
	if bad.Count > 0 {
		return res, bad
	}

	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, cnst.DirPerm); err != nil {
		return res, err
	}
	if err := diskspace.Check(parent, uint64(m.Bytes())+uint64(len(m.Files))*4096); err != nil {
		return res, err
	}
	sweepStages(root)
	stage, lock, err := newStage(root)
	if err != nil {
		return res, err
	}
	defer func() {
		if lock != nil {
			_ = lock.Unlock()
		}
		if stage != "" {
			_ = fsx.RetryBusy(func() error { return os.RemoveAll(stage) })
		}
	}()

	if err := writeFiles(ctx, p, entries, stage, "import"); err != nil {
		return res, err
	}
	if res.Opened, err = checkImported(ctx, stage, m, opts, &res.Notices); err != nil {
		return res, err
	}

	// The lock goes before the rename: Windows renames no directory with a
	// file open in it.
	if err := lock.Unlock(); err != nil {
		return res, err
	}
	lock = nil
	if err := fsx.Remove(filepath.Join(stage, stageLockName)); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	// An empty target is replaced: rename moves no directory over another
	// on Windows.
	if err := os.Remove(root); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("%s is no longer empty: %w", root, err)
	}
	if err := os.Rename(stage, root); err != nil {
		return res, err
	}
	stage = ""
	if err := fsx.SyncDir(parent); err != nil {
		return res, err
	}
	return res, nil
}

// checkTarget refuses a target that holds anything: an import makes a new
// database, and never merges into one.
func checkTarget(root string) error {
	entries, err := os.ReadDir(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		info, serr := os.Stat(root)
		if serr == nil && !info.IsDir() {
			return lifecycle.Usagef("%s is a file; import into a new or empty folder", root)
		}
		return err
	case len(entries) > 0:
		return lifecycle.Usagef("%s is not empty; import only into a new or empty folder (an import never merges into a database)", root)
	}
	return nil
}

// checkReadable refuses a package whose database this DUES cannot read.
func checkReadable(m duesarc.DatabaseManifest) error {
	db := m.Database
	if db.Format != dbmeta.FormatName {
		return fmt.Errorf("the package holds a %q database, not %q", db.Format, dbmeta.FormatName)
	}
	if db.FormatVersion > dbmeta.FormatVersion {
		return fmt.Errorf("%w (format version %d; this DUES reads up to %d)", dbmeta.ErrNewerFormat, db.FormatVersion, dbmeta.FormatVersion)
	}
	if n := db.Components["graph_schema"]; n > schema.CurrentVersion {
		return fmt.Errorf("the package's graph is schema version %d, and this DUES reads up to %d; upgrade DUES", n, schema.CurrentVersion)
	}
	return nil
}

// newStage makes the locked directory an import of root writes in.
func newStage(root string) (string, *fsx.Lock, error) {
	return lockedDir(filepath.Dir(root), "."+filepath.Base(root)+stageMarker+"*", stageLockName)
}

// sweepStages removes the directories earlier imports of root left, when
// killed, and no process holds.
func sweepStages(root string) {
	sweepLocked(filepath.Dir(root), "."+filepath.Base(root)+stageMarker, stageLockName)
}

// lockedDir makes a new directory in parent, named by pattern as for
// os.MkdirTemp, and locks lockName in it, so that no sweep removes it while
// it is in use.
func lockedDir(parent, pattern, lockName string) (string, *fsx.Lock, error) {
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		return "", nil, err
	}
	lock, err := fsx.TryLock(filepath.Join(dir, lockName), false)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if err := os.Chmod(dir, cnst.DirPerm); err != nil {
		_ = lock.Unlock()
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, lock, nil
}

// sweepLocked removes the directories in parent whose names start with
// prefix, made by lockedDir with lockName, that are older than
// staleStageAge and that no process holds: left by a process that was
// killed.
func sweepLocked(parent, prefix, lockName string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < staleStageAge {
			continue
		}
		p := filepath.Join(parent, e.Name())
		if lock, err := fsx.TryLock(filepath.Join(p, lockName), false); err == nil {
			_ = lock.Unlock()
			_ = os.RemoveAll(p)
		}
	}
}

// writeFiles writes the package's directories and files into stage,
// through os.Root so that no name reaches outside it, checking each file
// against the manifest as it is written and syncing it. label names the
// progress phase.
func writeFiles(ctx context.Context, p *Package, entries map[string]*zip.File, stage, label string) error {
	r, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer r.Close()
	m := p.Manifest
	for _, d := range m.Dirs {
		local, err := duesarc.LocalName(d)
		if err != nil {
			return err
		}
		if err := r.MkdirAll(local, cnst.DirPerm); err != nil {
			return err
		}
	}
	bad := &DamagedError{}
	ph := progress.Start(ctx, label, progress.Bytes, m.Bytes())
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			ph.Done(err)
			return err
		}
		reason, err := writeFile(ctx, r, entries[f.Path], f, ph)
		if err != nil {
			ph.Done(err)
			return err
		}
		if reason != "" {
			bad.add(entries[f.Path].Name, reason)
		}
	}
	if bad.Count > 0 {
		ph.Done(bad)
		return bad
	}
	ph.Done(nil)
	return syncDirs(stage, m.Dirs)
}

// writeFile writes one file and says what does not match the manifest.
func writeFile(ctx context.Context, r *os.Root, zf *zip.File, f duesarc.File, ph *progress.Phase) (string, error) {
	local, err := duesarc.LocalName(f.Path)
	if err != nil {
		return "", err
	}
	if dir := filepath.Dir(local); dir != "." {
		if err := r.MkdirAll(dir, cnst.DirPerm); err != nil {
			return "", err
		}
	}
	out, err := r.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, cnst.BlobPerm)
	if err != nil {
		return "", err
	}
	reason, err := checkEntry(ctx, zf, f, out, ph)
	if err == nil && reason == "" {
		err = out.Sync()
	}
	return reason, errors.Join(err, out.Close())
}

// syncDirs syncs the stage and its directories, deepest first, so the
// files' names are on disk before the stage becomes the target.
func syncDirs(stage string, dirs []string) error {
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := fsx.SyncDir(filepath.Join(stage, filepath.FromSlash(dirs[i]))); err != nil {
			return err
		}
	}
	return fsx.SyncDir(stage)
}

// checkImported opens the staged database read-only, as a reader would,
// when it can: unencrypted, or encrypted with its password given. It
// compares the evidence it counts with the manifest's.
func checkImported(ctx context.Context, stage string, m duesarc.DatabaseManifest, opts ImportOptions, notices *[]string) (bool, error) {
	if m.Database.Encrypted && !opts.Creds.IsSet() {
		*notices = append(*notices, "the database is encrypted and no password was given: its files were checked against the package, but the database was not opened")
		return false, nil
	}
	if err := openStaged(ctx, stage, m, dbopen.Request{Creds: opts.Creds, TempDir: opts.TempDir, Tuning: opts.Tuning}, notices, nil); err != nil {
		return false, err
	}
	// The open made the root lock file; the target gets its own.
	if err := fsx.Remove(filepath.Join(stage, dbopen.LockName)); err != nil {
		return false, err
	}
	return true, nil
}

// openStaged opens the database written into dir read-only, as a reader
// would, with req's credentials, temporary directory and tuning, and compares the evidence it counts with the manifest's. With
// report set, it also reads back every chunk of every evidence against its
// hash (store.VerifyEvidence) into report.
func openStaged(ctx context.Context, dir string, m duesarc.DatabaseManifest, req dbopen.Request, notices *[]string, report *store.VerifyReport) (err error) {
	req.Path, req.Mode = dir, dbopen.ReadOnly
	h, err := dbopen.Open(ctx, req)
	if err != nil {
		return fmt.Errorf("the package's database does not open: %w", err)
	}
	defer func() { err = errors.Join(err, h.Close()) }()
	*notices = append(*notices, h.Notices...)
	stats, err := evidenceStats(ctx, h.DB)
	if err != nil {
		return err
	}
	if want := m.Stats; want != nil && *want != *stats {
		return fmt.Errorf("the package's database holds %d evidence of %d bytes; its manifest says %d of %d", stats.Evidence, stats.LogicalBytes, want.Evidence, want.LogicalBytes)
	}
	if report != nil {
		*report, err = store.VerifyEvidence(ctx, h.DB)
	}
	return err
}
