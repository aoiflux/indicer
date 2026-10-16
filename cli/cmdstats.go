package cli

import (
	"context"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/store"
	"time"

	"go.uber.org/zap"
)

func StatsData(ctx context.Context, opts DBOptions) (err error) {
	start := time.Now()
	logging.GetLogger().Info("StatsData START",
		zap.String("db_path", opts.Path),
	)

	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("StatsData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)
	encryptionNotice(h)

	logging.GetLogger().Info("StatsData STATS_EXECUTE")
	err = store.Stats(h.DB)
	if err != nil {
		logging.GetLogger().Error("StatsData STATS_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Info("StatsData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}
