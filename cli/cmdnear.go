package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/lib/dbopen"
	"indicer/lib/logging"
	"indicer/lib/near"

	"github.com/fatih/color"
	"go.uber.org/zap"
)

// NearOptions are the near commands' options.
type NearOptions struct {
	Deep         bool
	Verify       bool
	TopK         int
	ExplainExact bool // near out only
	Out          string
	Force        bool
}

func (o NearOptions) near() near.Options {
	return near.Options{Deep: o.Deep, Verify: o.Verify, TopK: o.TopK, ExplainExact: o.ExplainExact, Out: o.Out, Force: o.Force}
}

// NearInData finds the stored objects related to the stored object inhash.
func NearInData(ctx context.Context, opts DBOptions, inhash string, no NearOptions) (err error) {
	return nearData(ctx, opts, "near in", inhash, no, func(h *dbopen.Handle) (near.Outcome, error) {
		return near.In(ctx, h.DB, inhash, no.near())
	})
}

// NearOutData finds the stored objects related to the file at outpath.
func NearOutData(ctx context.Context, opts DBOptions, outpath string, no NearOptions) (err error) {
	return nearData(ctx, opts, "near out", outpath, no, func(h *dbopen.Handle) (near.Outcome, error) {
		return near.Out(ctx, h.DB, outpath, no.near())
	})
}

func nearData(ctx context.Context, opts DBOptions, cmd, query string, no NearOptions, fn func(*dbopen.Handle) (near.Outcome, error)) (err error) {
	start := time.Now()
	logging.GetLogger().Info("NearData START",
		zap.String("command", cmd),
		zap.String("db_path", opts.Path),
		zap.String("query", query),
		zap.Bool("deep", no.Deep),
		zap.Bool("verify", no.Verify),
		zap.Int("top_k", no.TopK),
	)
	if no.TopK < 0 {
		return lifecycle.Usagef("--top-k must be >= 0")
	}
	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		logging.GetLogger().Error("NearData DB_CONNECT_ERROR", zap.Error(err))
		return err
	}
	defer closeDB(h, &err)

	console.Println("Finding NeAR artefacts....")
	o, err := fn(h)
	if errors.Is(err, near.ErrReportExists) {
		return lifecycle.Usagef("%v", err)
	}
	if err != nil {
		logging.GetLogger().Error("NearData ERROR", zap.Error(err))
		return err
	}
	for _, n := range o.Notes {
		color.New(color.FgHiYellow, color.Bold).Fprintf(console.Err(), "[near] %s\n", n)
	}
	c := o.Coverage
	console.Printf("Matched %d of %d distinct chunks (%d chunk positions); %d related objects. Report: %s\n",
		c.MatchedChunks, c.DistinctChunks, c.QueryChunks, o.Matches, o.ReportPath)
	console.Printf("Done.... %v\n", time.Since(start))
	logging.GetLogger().Info("NearData COMPLETE", zap.Int64("duration_ms", time.Since(start).Milliseconds()), zap.Int("matches", o.Matches))
	if c.WithoutReverseRelations > 0 {
		return &lifecycle.WarningError{Err: fmt.Errorf("%d of %d stored evidence were not searched: they have no reverse relations", c.WithoutReverseRelations, c.StoredEvidence)}
	}
	return nil
}
