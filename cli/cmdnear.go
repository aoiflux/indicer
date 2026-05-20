package cli

import (
	"indicer/lib/logging"
	"indicer/lib/near"
	"time"

	"go.uber.org/zap"
)

func NearInData(deep, verify bool, topK, chonkSize int, dbpath, inhash string, key []byte) error {
	start := time.Now()
	logging.GetLogger().Info("NearInData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
		zap.String("input_hash", inhash),
		zap.Int("top_k", topK),
		zap.Bool("deep", deep),
		zap.Bool("verify", verify),
	)

	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("NearInData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer db.Close()

	logging.GetLogger().Info("NearInData NEAR_SEARCH_EXECUTE")
	err = near.NearInFile(inhash, db, deep, verify, topK)
	if err != nil {
		logging.GetLogger().Error("NearInData NEAR_SEARCH_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Info("NearInData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

func NearOutData(deep, explainExact, verify bool, topK, chonkSize int, dbpath, outpath string, key []byte) error {
	start := time.Now()
	logging.GetLogger().Info("NearOutData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
		zap.String("output_path", outpath),
		zap.Int("top_k", topK),
		zap.Bool("deep", deep),
		zap.Bool("explain_exact", explainExact),
		zap.Bool("verify", verify),
	)

	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("NearOutData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer db.Close()

	logging.GetLogger().Info("NearOutData NEAR_SEARCH_EXECUTE")
	err = near.NearOutFile(outpath, db, deep, explainExact, verify, topK)
	if err != nil {
		logging.GetLogger().Error("NearOutData NEAR_SEARCH_ERROR", zap.Error(err))
		return err
	}

	logging.GetLogger().Info("NearOutData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}
