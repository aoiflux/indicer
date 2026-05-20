package cli

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/logging"
	"indicer/lib/store"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/fatih/color"
	"go.uber.org/zap"
)

func Common(chonkSize int, dbpath string, key []byte) (*badger.DB, string, error) {
	var err error
	if dbpath == "" {
		dbpath, err = util.GetDBPath()
		if err != nil {
			return nil, "", err
		}
	}
	util.SetChonkSize(chonkSize)

	// Recommend container mode for small chunk sizes
	if chonkSize < 128 && !cnst.CONTAINERMODE {
		color.Yellow("\n⚠️  RECOMMENDATION: You're using a chunk size of %dKB (< 128KB).", chonkSize)
		color.Yellow("   Consider using --container flag for better filesystem efficiency.")
		color.Yellow("   Container mode packs multiple small chunks into 1GB compressed files.\n")
		fmt.Println()
	}

	db, err := dbio.ConnectDB(dbpath, key)
	if err != nil {
		return nil, "", err
	}

	recoveryReport, err := store.RecoverIncompleteIngests(db)
	if err != nil {
		_ = db.Close()
		return nil, "", err
	}
	if recoveryReport.MarkedFailed > 0 {
		logging.GetLogger().Warn("Common STARTUP_RECOVERY_MARKED_FAILED",
			zap.Int("marked_failed", recoveryReport.MarkedFailed),
			zap.Int("incomplete_found", recoveryReport.IncompleteFound),
		)
	}

	return db, dbpath, nil
}
