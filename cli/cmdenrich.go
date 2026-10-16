package cli

import (
	"context"
	"indicer/internal/console"
	"indicer/lib/dbopen"
	"indicer/lib/enrichment"
	"indicer/lib/logging"
	"time"

	"go.uber.org/zap"
)

func EnrichData(ctx context.Context, opts DBOptions) (err error) {
	start := time.Now()
	logging.GetLogger().Info("EnrichData START",
		zap.String("db_path", opts.Path),
	)

	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		logging.GetLogger().Error("EnrichData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)
	db := h.DB

	logging.GetLogger().Debug("EnrichData OPEN_GRAPH_REPOSITORY")
	repo, err := enrichment.OpenGrapheneRepository(h.Root)
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

	console.Println("Graph enrichment complete: evidence files, partitions, and indexed-file metadata upserted.")
	logging.GetLogger().Info("EnrichData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}
