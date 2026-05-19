package cli

import (
	"indicer/lib/logging"
	"indicer/lib/store"
	"time"

	"go.uber.org/zap"
)

func StatsData(chonkSize int, dbpath string, key []byte) error {
	start := time.Now()
	logging.GetLogger().Info("StatsData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
	)

	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("StatsData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Info("StatsData STATS_EXECUTE")
	err = store.Stats(db)
	if err != nil {
		logging.GetLogger().Error("StatsData STATS_ERROR", zap.Error(err))
		return err
	}

	err = db.Close()
	if err != nil {
		logging.GetLogger().Error("StatsData DB_CLOSE_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Info("StatsData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}
