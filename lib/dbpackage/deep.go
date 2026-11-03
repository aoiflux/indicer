package dbpackage

import (
	"context"
	"os"
	"path/filepath"

	"indicer/internal/diskspace"
	"indicer/internal/duesarc"
	"indicer/internal/lifecycle"
	"indicer/internal/osdirs"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/fsx"
	"indicer/lib/store"
)

const (
	// deepStagePrefix names the directories a deep verify copies a
	// package's database into, in the temporary directory.
	deepStagePrefix = "dues-verify-"
	// deepLockName is the lock a deep verify holds in its directory.
	deepLockName = ".dues-verify.lock"
)

// DeepOptions say how a deep verify opens a package's database.
type DeepOptions struct {
	// Creds open an encrypted database; a deep verify of one needs them.
	Creds dbmeta.Credentials
	// TempDir is --temp-dir: where the database is copied, and what the
	// read-only open uses. Empty means osdirs.Temp.
	TempDir string
}

// VerifyDeep checks a package as Verify does, then checks the database it
// holds: it copies the files into a directory in the temporary directory,
// checking each against the manifest as it is written, opens the copy
// read-only, counts its evidence against the manifest and reads back every
// chunk of every evidence against its hash (store.VerifyEvidence). The
// copy is removed afterwards, also on failure.
//
// Damage to the package comes back as a *DamagedError; evidence the read
// back finds damaged is in the result's Evidence report, with no error.
func (p *Package) VerifyDeep(ctx context.Context, opts DeepOptions) (res VerifyResult, err error) {
	m := p.Manifest
	if m.Database.Encrypted && !opts.Creds.IsSet() {
		return res, lifecycle.Usagef("the package's database is encrypted: a deep verify reads its evidence, so it needs the password (--password-file or --password-stdin)")
	}
	if err := checkReadable(m); err != nil {
		return res, err
	}
	entries, bad, err := p.checkListing(ctx, &res)
	if err != nil {
		return res, err
	}
	if bad.Count > 0 {
		// Files are missing or added, so there is no database to open: name
		// what else is wrong, as Verify does.
		return res, p.checkContents(ctx, entries, bad, &res)
	}

	base, err := osdirs.TempOr(opts.TempDir)
	if err != nil {
		return res, err
	}
	if err := diskspace.Check(base, uint64(m.Bytes())+uint64(len(m.Files))*4096); err != nil {
		return res, err
	}
	sweepLocked(base, deepStagePrefix, deepLockName)
	stage, lock, err := lockedDir(base, deepStagePrefix+"*", deepLockName)
	if err != nil {
		return res, err
	}
	defer func() {
		_ = lock.Unlock()
		_ = fsx.RetryBusy(func() error { return os.RemoveAll(stage) })
	}()
	// The database goes in a folder of its own, beside the lock.
	dir := filepath.Join(stage, duesarc.DatabaseDir)
	if err := os.Mkdir(dir, cnst.DirPerm); err != nil {
		return res, err
	}
	if err := writeFiles(ctx, p, entries, dir, "copy"); err != nil {
		return res, err
	}
	res.Files, res.Bytes = len(m.Files), m.Bytes()

	report := &store.VerifyReport{}
	if err := openStaged(ctx, dir, m, opts.Creds, opts.TempDir, &res.Notices, report); err != nil {
		return res, err
	}
	res.Evidence = report
	return res, nil
}
