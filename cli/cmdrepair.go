package cli

import (
	"indicer/lib/logging"
	"indicer/lib/store"
	"time"

	"go.uber.org/zap"
)

func RepairData(chonkSize int, dbpath string, key []byte, fix bool) error {
	start := time.Now()
	logging.GetLogger().Info("RepairData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
		zap.Bool("fix", fix),
	)

	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("RepairData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer db.Close()

	logging.GetLogger().Info("RepairData REPAIR_EXECUTE", zap.Bool("fix", fix))
	err = store.Repair(db, fix)
	if err != nil {
		logging.GetLogger().Error("RepairData REPAIR_ERROR", zap.Error(err), zap.Bool("fix", fix))
		return err
	}

	logging.GetLogger().Info("RepairData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Bool("fix", fix),
	)
	return nil
}
