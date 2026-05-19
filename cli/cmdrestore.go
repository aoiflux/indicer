package cli

import (
	"fmt"
	"indicer/lib/logging"
	"indicer/lib/store"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

func RestoreData(chonkSize int, dbpath, rhash, rpath string, key []byte) (err error) {
	start := time.Now()
	logging.GetLogger().Info("RestoreData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
		zap.String("restore_hash", rhash),
		zap.String("restore_path", rpath),
	)

	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("RestoreData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer func() {
		closeErr := db.Close()
		if closeErr != nil {
			logging.GetLogger().Error("RestoreData DB_CLOSE_ERROR", zap.Error(closeErr))
			if err == nil {
				err = closeErr
			}
		}
	}()

	tempDir := filepath.Dir(rpath)
	tempPattern := filepath.Base(rpath) + ".tmp-*"
	logging.GetLogger().Debug("RestoreData OPEN_TEMP_OUTPUT_FILE",
		zap.String("file_path", rpath),
		zap.String("temp_dir", tempDir),
	)
	fhandle, err := os.CreateTemp(tempDir, tempPattern)
	if err != nil {
		logging.GetLogger().Error("RestoreData OPEN_TEMP_OUTPUT_FILE_ERROR", zap.Error(err), zap.String("file_path", rpath))
		return err
	}
	tempPath := fhandle.Name()
	keepTemp := false
	defer func() {
		if fhandle != nil {
			if closeErr := fhandle.Close(); closeErr != nil && err == nil {
				logging.GetLogger().Error("RestoreData TEMP_FILE_CLOSE_ERROR", zap.Error(closeErr), zap.String("temp_path", tempPath))
				err = closeErr
			}
		}
		if !keepTemp {
			if removeErr := os.Remove(tempPath); removeErr != nil && !os.IsNotExist(removeErr) {
				logging.GetLogger().Warn("RestoreData TEMP_FILE_REMOVE_ERROR", zap.Error(removeErr), zap.String("temp_path", tempPath))
			}
		}
	}()

	logging.GetLogger().Info("RestoreData RESTORE_EXECUTE")
	fmt.Println("Restoring file ...")
	err = store.Restore(rhash, fhandle, db)
	if err != nil {
		logging.GetLogger().Error("RestoreData RESTORE_ERROR", zap.Error(err), zap.String("restore_hash", rhash))
		return err
	}

	err = fhandle.Sync()
	if err != nil {
		logging.GetLogger().Error("RestoreData TEMP_FILE_SYNC_ERROR", zap.Error(err), zap.String("temp_path", tempPath))
		return err
	}
	err = fhandle.Close()
	if err != nil {
		logging.GetLogger().Error("RestoreData TEMP_FILE_CLOSE_ERROR", zap.Error(err), zap.String("temp_path", tempPath))
		return err
	}
	fhandle = nil

	err = replaceRestoreTarget(tempPath, rpath)
	if err != nil {
		logging.GetLogger().Error("RestoreData RENAME_ERROR",
			zap.Error(err),
			zap.String("temp_path", tempPath),
			zap.String("restore_path", rpath),
		)
		return err
	}
	keepTemp = true

	fmt.Println("Restored in: ", time.Since(start))
	logging.GetLogger().Info("RestoreData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("restore_path", rpath),
	)
	return nil
}

func replaceRestoreTarget(tempPath, targetPath string) error {
	if _, err := os.Stat(targetPath); err != nil {
		if os.IsNotExist(err) {
			return os.Rename(tempPath, targetPath)
		}
		return err
	}

	backupPath := targetPath + ".bak"
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(targetPath, backupPath); err != nil {
		return err
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		if rollbackErr := os.Rename(backupPath, targetPath); rollbackErr != nil {
			return fmt.Errorf("move temp into place: %w; rollback failed: %v", err, rollbackErr)
		}
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	return nil
}
