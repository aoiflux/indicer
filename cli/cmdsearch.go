package cli

import (
	"context"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/logging"
	"indicer/lib/search"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"go.uber.org/zap"
)

func SearchCmd(chonkSize int, query, dbpath string, key []byte, rankAlpha float64, fullText bool) error {
	start := time.Now()
	logging.GetLogger().Info("SearchCmd START",
		zap.Int("chonk_size", chonkSize),
		zap.String("db_path", dbpath),
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
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		logging.GetLogger().Error("SearchCmd DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	search.SetOccurrenceBoostAlpha(rankAlpha)
	query = strings.ToLower(query)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if fullText {
		logging.GetLogger().Info("SearchCmd SEARCH_EXECUTE", zap.String("mode", "hybrid_full_text_fallback"))
		color.New(color.FgHiCyan, color.Bold).Fprintln(os.Stderr, "[search] Mode: hybrid full-text + scan ranking")
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
	color.New(color.FgHiWhite, color.Bold).Fprintln(os.Stderr, "[search] Mode: scan ranking only (use --enable-fts to enable hybrid mode)")
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
