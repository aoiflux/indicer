package cli

import (
	"context"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/store"
	"time"

	"go.uber.org/zap"
)

func RepairData(ctx context.Context, opts DBOptions, fix bool) (err error) {
	start := time.Now()
	logging.GetLogger().Info("RepairData START",
		zap.String("db_path", opts.Path),
		zap.Bool("fix", fix),
	)

	// Without --fix repair only inspects. With it, repair itself decides
	// what to change, so the automatic startup recovery stays off (CFG-11).
	mode := dbopen.ReadOnly
	if fix {
		mode = dbopen.ReadWrite
	}
	h, err := Common(ctx, opts, mode, false)
	if err != nil {
		logging.GetLogger().Error("RepairData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)

	logging.GetLogger().Info("RepairData REPAIR_EXECUTE", zap.Bool("fix", fix))
	err = store.Repair(h.DB, fix)
	if err != nil {
		logging.GetLogger().Error("RepairData REPAIR_ERROR", zap.Error(err), zap.Bool("fix", fix))
		return err
	}

	if fix {
		compactValueLog(h, "RepairData")
	}
	logging.GetLogger().Info("RepairData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.Bool("fix", fix),
	)
	return nil
}
