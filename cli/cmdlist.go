package cli

import (
	"indicer/lib/enrichment"
	"indicer/lib/logging"
	"indicer/lib/store"
	"time"

	"go.uber.org/zap"
)

func ListData(chonkSize int, dbpath string, key []byte, statusFilter string) error {
	start := time.Now()
	logging.GetLogger().Info("ListData START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
		zap.String("status_filter", statusFilter),
	)

	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("ListData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer db.Close()

	// Try to open enrichment graphdb for graphdb-first retrieval
	logging.GetLogger().Debug("ListData OPEN_GRAPH_REPOSITORY")
	enrichRepo, err := enrichment.OpenGrapheneRepository(dbpath)
	if err != nil {
		// Graphdb not available, fall back to KVDB-only
		logging.GetLogger().Debug("ListData GRAPH_REPOSITORY_UNAVAILABLE", zap.Error(err))
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
	defer enrichRepo.Close()

	// Try graphdb-first listing
	logging.GetLogger().Info("ListData LIST_EXECUTE", zap.String("mode", "graphdb_first"))
	err = store.ListWithEnrichment(db, enrichRepo, statusFilter)
	if err != nil {
		logging.GetLogger().Warn("ListData LIST_GRAPHDB_FAILED_FALLBACK_KV",
			zap.Error(err),
			zap.String("mode", "graphdb_first"),
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
		zap.String("mode", "graphdb_first"),
	)
	return nil
}
