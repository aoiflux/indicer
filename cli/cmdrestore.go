package cli

import (
	"context"
	"errors"
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

// RestoreData restores the object whose hash is rhash to rpath. An existing
// rpath is replaced only with force, and a directory never is.
func RestoreData(ctx context.Context, opts DBOptions, rhash, rpath string, force bool) (err error) {
	start := time.Now()
	logging.GetLogger().Info("RestoreData START",
		zap.String("db_path", opts.Path),
		zap.String("restore_hash", rhash),
		zap.String("restore_path", rpath),
		zap.Bool("force", force),
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
	if err := restoreToPath(ctx, h.DB, rhash, rpath, force); err != nil {
		logging.GetLogger().Error("RestoreData RESTORE_ERROR", zap.Error(err), zap.String("restore_hash", rhash))
		return err
	}

	console.Println("Restored in: ", time.Since(start))
	logging.GetLogger().Info("RestoreData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("restore_path", rpath),
	)
	return nil
}

// restoreToPath restores into a temporary file beside rpath, syncs it and
// renames it into place (RST-4). A failed or cancelled restore removes the
// temporary file and leaves rpath as it was; nothing else, such as a
// <rpath>.bak, is touched.
func restoreToPath(ctx context.Context, db *badger.DB, rhash, rpath string, force bool) error {
	if err := checkRestoreTarget(rpath, force); err != nil {
		return err
	}
	err := fsx.WriteAtomic(rpath, cnst.FilePerm, fsx.Options{NoOverwrite: !force}, func(f *os.File) error {
		return store.Restore(ctx, rhash, f, db)
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, fs.ErrExist) {
		return lifecycle.Usagef("%s was created while restoring; pass --force to replace it", rpath)
	}
	return err
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
