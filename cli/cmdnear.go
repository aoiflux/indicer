package cli

import (
	"context"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/near"
	"time"

	"go.uber.org/zap"
)

func NearInData(ctx context.Context, deep, verify bool, topK int, opts DBOptions, inhash string) (err error) {
	start := time.Now()
	logging.GetLogger().Info("NearInData START",
		zap.String("db_path", opts.Path),
		zap.String("input_hash", inhash),
		zap.Int("top_k", topK),
		zap.Bool("deep", deep),
		zap.Bool("verify", verify),
	)

	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("NearInData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)

	logging.GetLogger().Info("NearInData NEAR_SEARCH_EXECUTE")
	err = near.NearInFile(inhash, h.DB, deep, verify, topK)
	if err != nil {
		logging.GetLogger().Error("NearInData NEAR_SEARCH_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Info("NearInData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

func NearOutData(ctx context.Context, deep, explainExact, verify bool, topK int, opts DBOptions, outpath string) (err error) {
	start := time.Now()
	logging.GetLogger().Info("NearOutData START",
		zap.String("db_path", opts.Path),
		zap.String("output_path", outpath),
		zap.Int("top_k", topK),
		zap.Bool("deep", deep),
		zap.Bool("explain_exact", explainExact),
		zap.Bool("verify", verify),
	)

	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("NearOutData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)

	logging.GetLogger().Info("NearOutData NEAR_SEARCH_EXECUTE")
	err = near.NearOutFile(outpath, h.DB, deep, explainExact, verify, topK)
	if err != nil {
		logging.GetLogger().Error("NearOutData NEAR_SEARCH_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Info("NearOutData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}
