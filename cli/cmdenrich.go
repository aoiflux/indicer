package cli

import (
	"fmt"
	"indicer/lib/enrichment"
	"indicer/lib/logging"
	"time"

	"go.uber.org/zap"
)

func EnrichData(chonkSize int, dbpath string, key []byte) error {
	start := time.Now()
	logging.GetLogger().Info("EnrichData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
	)

	db, dbpath, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("EnrichData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer db.Close()

	logging.GetLogger().Debug("EnrichData OPEN_GRAPH_REPOSITORY")
	repo, err := enrichment.OpenGrapheneRepository(dbpath)
	if err != nil {
		logging.GetLogger().Error("EnrichData OPEN_GRAPH_REPOSITORY_ERROR", zap.Error(err))
		return err
	}

	service := enrichment.NewService(db, repo)
	defer service.Close()

	logging.GetLogger().Info("EnrichData ENRICH_ALL_EXECUTE")
	if err := service.EnrichAll(); err != nil {
		logging.GetLogger().Error("EnrichData ENRICH_ALL_ERROR", zap.Error(err))
		return err
	}

	fmt.Println("Graph enrichment complete: evidence files, partitions, and indexed-file metadata upserted.")
	logging.GetLogger().Info("EnrichData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}
