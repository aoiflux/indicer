package cli

import (
	"context"
	"errors"
	"fmt"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/fsx"
	"indicer/lib/logging"
	"indicer/lib/store"
	"io/fs"
	"os"
	"time"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// RestoreOptions control a restore to a path.
type RestoreOptions struct {
	// Force replaces an existing file at the restore path.
	Force bool
	// Verify is how much the restore checks what it writes.
	Verify store.VerifyLevel
}

// RestoreData restores the object whose hash is rhash to rpath. An existing
// rpath is replaced only with ropts.Force, and a directory never is.
func RestoreData(ctx context.Context, opts DBOptions, rhash, rpath string, ropts RestoreOptions) (err error) {
	force := ropts.Force
	start := time.Now()
	logging.GetLogger().Info("RestoreData START",
		zap.String("db_path", opts.Path),
		zap.String("restore_hash", rhash),
		zap.String("restore_path", rpath),
		zap.Bool("force", force),
		zap.Stringer("verify", ropts.Verify),
	)

	// Check the target first, so a refusal costs no restore.
	if err := checkRestoreTarget(rpath, force); err != nil {
		return err
	}

	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("RestoreData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)

	console.Println("Restoring file ...")
	report, err := restoreToPath(ctx, h.DB, rhash, rpath, ropts)
	if err != nil {
		logging.GetLogger().Error("RestoreData RESTORE_ERROR", zap.Error(err), zap.String("restore_hash", rhash))
		return err
	}
	console.Println(describeVerification(report))

	console.Println("Restored in: ", time.Since(start))
	logging.GetLogger().Info("RestoreData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("restore_path", rpath),
		zap.String("digest", string(report.Digest)),
		zap.String("digest_form", report.DigestForm),
	)
	return nil
}

// describeVerification says what a restore checked.
func describeVerification(r store.RestoreReport) string {
	switch r.Digest {
	case store.DigestVerified:
		return fmt.Sprintf("Verified: %d chunks match their hashes, and the content matches its recorded %s hash.", r.Chunks, r.DigestForm)
	case store.DigestUnavailable:
		return fmt.Sprintf("Verified: %d chunks match their hashes. The record carries no hash of the whole content to check.", r.Chunks)
	}
	return "Not verified against the recorded hash (--verify)."
}

// restoreToPath restores into a temporary file beside rpath, syncs it and
// renames it into place (RST-4). A failed or cancelled restore removes the
// temporary file and leaves rpath as it was; nothing else, such as a
// <rpath>.bak, is touched.
func restoreToPath(ctx context.Context, db *badger.DB, rhash, rpath string, ropts RestoreOptions) (store.RestoreReport, error) {
	var report store.RestoreReport
	if err := checkRestoreTarget(rpath, ropts.Force); err != nil {
		return report, err
	}
	err := fsx.WriteAtomic(rpath, cnst.FilePerm, fsx.Options{NoOverwrite: !ropts.Force}, func(f *os.File) error {
		var err error
		report, err = store.Restore(ctx, db, rhash, f, store.RestoreOptions{Verify: ropts.Verify})
		return err
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return report, ctxErr
	}
	if errors.Is(err, fs.ErrExist) {
		return report, lifecycle.Usagef("%s was created while restoring; pass --force to replace it", rpath)
	}
	return report, err
}

// checkRestoreTarget refuses a directory, and an existing file unless force.
func checkRestoreTarget(rpath string, force bool) error {
	fi, err := os.Lstat(rpath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		return lifecycle.Usagef("%s is a directory; restore needs a file path", rpath)
	case !force:
		return lifecycle.Usagef("%s already exists; pass --force to replace it", rpath)
	}
	return nil
}
