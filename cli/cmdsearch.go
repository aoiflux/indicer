package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/termsafe"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/search"

	"github.com/fatih/color"
	"go.uber.org/zap"
)

// SearchOptions are the search command's options.
type SearchOptions struct {
	Query     string
	RankAlpha float64
	FullText  bool
	Out       string
	Force     bool
}

// SearchCmd searches the database and writes the report. An object that
// could not be read is reported and the command exits 2.
func SearchCmd(ctx context.Context, opts DBOptions, so SearchOptions) (err error) {
	start := time.Now()
	logging.GetLogger().Info("SearchCmd START",
		zap.String("db_path", opts.Path),
		zap.Int("query_length", len(so.Query)),
		zap.Bool("full_text", so.FullText),
	)
	if err := search.ValidateQuery(so.Query); err != nil {
		return lifecycle.Usagef("%v", err)
	}
	if so.RankAlpha < 0 {
		return lifecycle.Usagef("rank-alpha must be >= 0")
	}
	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("SearchCmd DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)

	mode := "scan of every stored object"
	if so.FullText {
		mode = "full-text candidates, then scan"
	}
	color.New(color.FgHiWhite, color.Bold).Fprintf(console.Err(), "[search] Mode: %s\n", mode)
	o, err := search.SearchReport(ctx, h.DB, search.Request{
		Query: so.Query, FullText: so.FullText, RankAlpha: so.RankAlpha,
		Out: so.Out, Force: so.Force, Progress: true,
	})
	if errors.Is(err, search.ErrReportExists) || errors.Is(err, cnst.ErrSmallQuery) || errors.Is(err, search.ErrQueryTooLong) {
		return lifecycle.Usagef("%v", err)
	}
	if err != nil {
		logging.GetLogger().Error("SearchCmd SEARCH_ERROR", zap.Error(err))
		return err
	}
	for _, n := range o.Notes {
		color.New(color.FgHiYellow, color.Bold).Fprintf(console.Err(), "[search] %s\n", n)
	}
	reportSearch(o)
	console.Println("Done....", time.Since(start))
	logging.GetLogger().Info("SearchCmd COMPLETE",
		zap.Int64("duration_ms", time.Since(start).Milliseconds()),
		zap.String("mode", o.Mode),
		zap.Int("result_count", o.Ranked),
	)
	if c := o.Result.Coverage; c.Incomplete() {
		return &lifecycle.WarningError{Err: fmt.Errorf("%d objects could not be read and were not searched; the report's coverage names them", c.Unreadable)}
	}
	return nil
}

func reportSearch(o search.Outcome) {
	c := o.Result.Coverage
	console.Printf("Searched %d evidence, %d partitions and %d indexed files (%d bytes).\n", c.Evidence, c.Partitions, c.Files, c.Bytes)
	if c.NotStored > 0 {
		console.Printf("%d evidence records are not stored (pending, failed or duplicate) and were not searched.\n", c.NotStored)
	}
	if c.Quarantined > 0 {
		console.Printf("%d evidence have a quarantined index: only the evidence itself was searched.\n", c.Quarantined)
	}
	if c.Unavailable > 0 {
		console.Printf("%d indexed files have no readable content (metadata only).\n", c.Unavailable)
	}
	for _, p := range c.Problems {
		color.New(color.FgHiRed).Fprintf(console.Err(), "[search] not searched: %s\n", termsafe.Line(p))
	}
	console.Printf("%d objects hold a match. Report: %s\n", o.Ranked, o.ReportPath)
}
