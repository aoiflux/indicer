package cli

import (
	"context"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/search"
	"strings"
	"time"

	"github.com/fatih/color"
	"go.uber.org/zap"
)

func SearchCmd(ctx context.Context, opts DBOptions, query string, rankAlpha float64, fullText bool) (err error) {
	start := time.Now()
	logging.GetLogger().Info("SearchCmd START",
		zap.String("db_path", opts.Path),
		zap.Int("query_length", len(query)),
		zap.Bool("full_text", fullText),
	)

	if len(query) < 2 {
		err := cnst.ErrSmallQuery
		logging.GetLogger().Error("SearchCmd VALIDATION_ERROR", zap.Error(err))
		return cnst.ErrSmallQuery
	}
	if rankAlpha < 0 {
		err := fmt.Errorf("rank-alpha must be >= 0")
		logging.GetLogger().Error("SearchCmd VALIDATION_ERROR", zap.Error(err), zap.String("parameter", "rank_alpha"))
		return err
	}
	logging.GetLogger().Debug("SearchCmd DB_CONNECT")
	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("SearchCmd DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)
	db := h.DB
	search.SetOccurrenceBoostAlpha(rankAlpha)
	query = strings.ToLower(query)
	if fullText {
		logging.GetLogger().Info("SearchCmd SEARCH_EXECUTE", zap.String("mode", "hybrid_full_text_fallback"))
		color.New(color.FgHiCyan, color.Bold).Fprintln(console.Err(), "[search] Mode: hybrid full-text + scan ranking")
		err = search.SearchWithContextFullTextFallback(ctx, query, db)
		if err != nil {
			logging.GetLogger().Error("SearchCmd SEARCH_ERROR", zap.Error(err))
			return err
		}
		logging.GetLogger().Info("SearchCmd COMPLETE",
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.String("mode", "hybrid_full_text_fallback"),
		)
		return nil
	}
	logging.GetLogger().Info("SearchCmd SEARCH_EXECUTE", zap.String("mode", "scan_only"))
	color.New(color.FgHiWhite, color.Bold).Fprintln(console.Err(), "[search] Mode: scan ranking only (use --fts to enable hybrid mode)")
	err = search.SearchWithContext(ctx, query, db)
	if err != nil {
		logging.GetLogger().Error("SearchCmd SEARCH_ERROR", zap.Error(err))
		return err
	}
	logging.GetLogger().Info("SearchCmd COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("mode", "scan_only"),
	)
	return nil
}
