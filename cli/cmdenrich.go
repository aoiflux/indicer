package cli

import (
	"context"
	"errors"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
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
	defer func() {
		if cerr := service.Close(); err == nil {
			err = cerr
		}
	}()

	logging.GetLogger().Info("EnrichData ENRICH_ALL_EXECUTE")
	rep, err := service.EnrichAll(ctx)
	var incomplete *enrichment.IncompleteError
	if err != nil && !errors.As(err, &incomplete) {
		logging.GetLogger().Error("EnrichData ENRICH_ALL_ERROR", zap.Error(err))
		return err
	}

	console.Printf("Graph enrichment: %d evidence, %d partition(s), %d indexed file(s) written.\n", rep.Evidence, rep.Partitions, rep.Files)
	if rep.Quarantined > 0 {
		console.Printf("%d evidence with a quarantined index: only the evidence itself was written.\n", rep.Quarantined)
	}
	if incomplete != nil {
		for _, p := range rep.Problems {
			console.Errorf("unreadable: %s\n", p)
		}
		return &lifecycle.WarningError{Err: incomplete}
	}
	logging.GetLogger().Info("EnrichData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}
