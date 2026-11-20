package cli

import (
	"context"
	"errors"
	"indicer/internal/console"
	"indicer/lib/dbopen"
	"indicer/lib/enrichment"
	duesgraph "indicer/lib/graph"
	"indicer/lib/logging"
	"indicer/lib/store"
	"time"

	"go.uber.org/zap"
)

func ListData(ctx context.Context, opts DBOptions, statusFilter string) (err error) {
	start := time.Now()
	logging.GetLogger().Info("ListData START",
		zap.String("db_path", opts.Path),
		zap.String("status_filter", statusFilter),
	)

	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("ListData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)
	db := h.DB
	encryptionNotice(h)

	// Open the enrichment graph, read-only, for the annotations it adds. It
	// lives in the data root, not the working directory, and a listing never
	// creates it (CFG-7).
	logging.GetLogger().Debug("ListData OPEN_GRAPH_REPOSITORY")
	enrichRepo, err := enrichment.OpenGrapheneRepositoryReadOnly(h.Root)
	if err != nil {
		// No graph, or one that cannot be read now: list from the database.
		logging.GetLogger().Debug("ListData GRAPH_REPOSITORY_UNAVAILABLE", zap.Error(err))
		if !errors.Is(err, duesgraph.ErrNoGraph) {
			console.Errorf("warning: listing without graph enrichment: %v\n", err)
		}
		err = store.List(db, statusFilter)
		if err != nil {
			logging.GetLogger().Error("ListData LIST_ERROR", zap.Error(err), zap.String("mode", "kvdb_only"))
			return err
		}
		logging.GetLogger().Info("ListData COMPLETE",
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.String("mode", "kvdb_only"),
		)
		return nil
	}
	defer func() {
		if cerr := enrichRepo.Close(); err == nil {
			err = cerr
		}
	}()

	// List from the database; the graph only annotates indexed files.
	logging.GetLogger().Info("ListData LIST_EXECUTE", zap.String("mode", "database_annotated"))
	err = store.ListWithEnrichment(db, enrichRepo, statusFilter)
	if err != nil {
		logging.GetLogger().Warn("ListData LIST_GRAPHDB_FAILED_FALLBACK_KV",
			zap.Error(err),
			zap.String("mode", "database_annotated"),
		)
		err = store.List(db, statusFilter)
		if err != nil {
			logging.GetLogger().Error("ListData LIST_ERROR", zap.Error(err), zap.String("mode", "kvdb_fallback"))
			return err
		}
		logging.GetLogger().Info("ListData COMPLETE",
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.String("mode", "kvdb_fallback"),
		)
		return nil
	}
	logging.GetLogger().Info("ListData COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("mode", "database_annotated"),
	)
	return nil
}
