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
)

func SearchCmd(chonkSize int, query, dbpath string, key []byte, rankAlpha float64) error {
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
	return search.SearchWithContext(ctx, query, db)
}
