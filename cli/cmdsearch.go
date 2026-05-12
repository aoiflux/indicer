package cli

import (
	"context"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/search"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/fatih/color"
)

func SearchCmd(chonkSize int, query, dbpath string, key []byte, rankAlpha float64, fullText bool) error {
	if len(query) < 2 {
		return cnst.ErrSmallQuery
	}
	if rankAlpha < 0 {
		return fmt.Errorf("rank-alpha must be >= 0")
	}
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}
	search.SetOccurrenceBoostAlpha(rankAlpha)
	query = strings.ToLower(query)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if fullText {
		color.New(color.FgHiCyan, color.Bold).Fprintln(os.Stderr, "[search] Mode: hybrid full-text + scan ranking")
		return search.SearchWithContextFullTextFallback(ctx, query, db)
	}
	color.New(color.FgHiWhite, color.Bold).Fprintln(os.Stderr, "[search] Mode: scan ranking only (use --enable-fts to enable hybrid mode)")
	return search.SearchWithContext(ctx, query, db)
}
