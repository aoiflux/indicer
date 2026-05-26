package cli

import (
	"fmt"
	"indicer/lib/logging"
	"indicer/lib/util"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"go.uber.org/zap"
)

func ResetData(dbpath string) error {
	start := time.Now()
	logging.GetLogger().Info("ResetData START", zap.String("db_path", dbpath))

	var err error

	if dbpath == "" {
		logging.GetLogger().Debug("ResetData RESOLVE_DB_PATH")
		dbpath, err = util.GetDBPath()
		if err != nil {
			logging.GetLogger().Error("ResetData RESOLVE_DB_PATH_ERROR", zap.Error(err))
			return err
		}
	}

	info, err := os.Stat(dbpath)
	if err != nil {
		if os.IsNotExist(err) {
			color.Blue("Nothing to reset: data folder does not exist: %s", dbpath)
			logging.GetLogger().Info("ResetData SKIP_MISSING_PATH",
				zap.String("db_path", dbpath),
				zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			)
			return nil
		}
		logging.GetLogger().Error("ResetData STAT_ERROR", zap.Error(err), zap.String("db_path", dbpath))
		return err
	}
	if !info.IsDir() {
		logging.GetLogger().Error("ResetData INVALID_PATH_NOT_DIR", zap.String("db_path", dbpath))
		return fmt.Errorf("db path is not a directory: %s", dbpath)
	}

	entries, err := os.ReadDir(dbpath)
	if err != nil {
		logging.GetLogger().Error("ResetData READDIR_ERROR", zap.Error(err), zap.String("db_path", dbpath))
		return err
	}
	if len(entries) == 0 {
		color.Blue("Nothing to reset: data folder is already empty: %s", dbpath)
		logging.GetLogger().Info("ResetData SKIP_ALREADY_EMPTY",
			zap.String("db_path", dbpath),
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		)
		return nil
	}

	color.Red("WARNING! This command will DELETE ALL the saved files.")
	fmt.Printf("Are you sure about this? [y/N] ")

	var in string
	fmt.Scanln(&in)
	in = strings.ToLower(in)

	if in != "y" {
		color.Blue("Your data is SAFE!")
		logging.GetLogger().Info("ResetData CANCELLED", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
		return nil
	}

	color.Red("Deleting ALL data!")
	err = os.RemoveAll(dbpath)
	if err != nil {
		logging.GetLogger().Error("ResetData DELETE_ERROR", zap.Error(err), zap.String("db_path", dbpath))
		return err
	}
	logging.GetLogger().Info("ResetData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("db_path", dbpath),
	)
	return nil
}
