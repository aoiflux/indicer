package main

import (
	"context"
	"errors"
	"fmt"
	"indicer/api"
	"indicer/cli"
	"indicer/internal/crash"
	"indicer/internal/lifecycle"
	"indicer/internal/safe"
	"indicer/lib/cnst"
	"indicer/lib/logging"
	"indicer/lib/store"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

const (
	duesVersion  = "0.39"
	duesCodename = "<pineapple> spacebar"
)

func init() {
	var err error

	cnst.DECODER, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	handle(err)

	cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	handle(err)
}

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain runs DUES and returns the process exit code. Everything that must
// happen on the way out (flushing the log, keeping or removing the crash
// file) is deferred here, so nothing below may call os.Exit.
func realMain(args []string) (code int) {
	if maybeHandleHelp(args) {
		return lifecycle.ExitOK
	}

	logInfo := logging.Init(logging.Config{
		Component: "dues",
		Dir:       logDirFromArgs(args),
		Debug:     os.Getenv("DUES_DEBUG") != "",
	})
	defer logging.Close()

	crashDir := logInfo.Dir
	if crashDir == "" {
		crashDir = filepath.Join(os.TempDir(), "dues-crash")
	}
	if _, err := crash.Setup(crashDir, logInfo.RunID); err != nil {
		logging.GetLogger().Warn("crash output not captured", zap.Error(err))
	}
	defer crash.Finish()

	safe.SetReporter(func(e *safe.PanicError, first bool) {
		if first {
			logging.GetLogger().Error("recovered panic",
				zap.String("panic_id", e.ID), zap.String("site", e.Site.String()),
				zap.Any("value", e.Value), zap.ByteString("stack", e.Stack))
			return
		}
		logging.GetLogger().Warn("recovered panic (repeat)",
			zap.String("panic_id", e.ID), zap.String("site", e.Site.String()))
	})

	defer func() {
		if v := recover(); v != nil {
			stack := debug.Stack()
			logging.GetLogger().Error("unrecovered panic", zap.Any("value", v), zap.ByteString("stack", stack))
			crash.Message(os.Stderr, v, logInfo.RunID, crash.Write(v, stack))
			code = lifecycle.ExitInternal
		}
	}()

	logging.GetLogger().Info("DUES starting",
		zap.String("version", duesVersion),
		zap.String("codename", duesCodename),
		zap.Strings("args", logging.RedactArgs(args)))

	err := run(args)
	if cerr := closeCompressionCodecs(); err == nil {
		err = cerr
	}
	if err != nil {
		logging.GetLogger().Error("command failed", zap.Error(err))
		fmt.Fprintf(os.Stderr, "\ndues: error: %v\n", err)
	}
	return lifecycle.ExitCode(err)
}

// logDirFromArgs finds --log-dir before kingpin parses the command line,
// because the log must exist before parsing can fail.
func logDirFromArgs(args []string) string {
	flag := "--" + flagLogDir
	for i, a := range args {
		if a == "--" {
			break
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func run(args []string) error {
	cliDef := newCLIDefinition()

	// kingpin exits the process itself after --version and --help. Record the
	// code instead, so realMain's deferred cleanup still runs.
	terminated := -1
	cliDef.app.Terminate(func(code int) {
		if terminated < 0 {
			terminated = code
		}
	})
	parsed, err := cliDef.app.Parse(args)
	if terminated == 0 {
		return nil
	}
	if terminated > 0 {
		// kingpin has already printed what was wrong.
		return &lifecycle.UsageError{Err: errors.New("invalid command line")}
	}
	if err != nil {
		return lifecycle.Usagef("%v (see dues --help)", err)
	}

	tuning, err := applyRuntimeConfig(parsed, args, cliDef)
	if err != nil {
		return err
	}
	for _, issue := range tuning.issues {
		logging.GetLogger().Warn("setting adjusted", zap.String("flag", issue.Flag), zap.String("detail", issue.Message))
		fmt.Fprintln(os.Stderr, "dues: warning:", issue)
	}

	if err := reconfigureEncoderForCompressionLevel(); err != nil {
		return err
	}

	creds, warnings, err := resolveCredentials(cliDef.flags, tuning.layout.Set, os.Stdin)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		logging.GetLogger().Warn("credentials", zap.String("detail", w))
		fmt.Fprintln(os.Stderr, "dues: warning:", w)
	}
	dbOpts := cli.DBOptions{Path: *cliDef.flags.dbpath, Creds: creds, Layout: tuning.layout}

	printRuntimeBanner(parsed, cliDef, tuning)

	// The first SIGINT/SIGTERM cancels ctx and the command winds down; a
	// second one exits at once (LOG-3).
	ctx, stop := lifecycle.Context(context.Background())
	defer stop()
	return executeCommand(ctx, parsed, cliDef, dbOpts, tuning)
}

func closeCompressionCodecs() error {
	if err := cnst.ENCODER.Close(); err != nil {
		return err
	}
	cnst.DECODER.Close()
	return nil
}

func executeCommand(ctx context.Context, parsed string, cliDef cliDefinition, dbOpts cli.DBOptions, tuning pipelineTuning) error {
	handlers := buildCommandHandlers(cliDef, dbOpts, tuning)
	handler, ok := handlers[parsed]
	if !ok {
		return fmt.Errorf("unsupported command: %s", parsed)
	}
	// A command cut short by a signal exits with the signal's code.
	return lifecycle.Cause(ctx, handler(ctx))
}

func buildCommandHandlers(cliDef cliDefinition, dbOpts cli.DBOptions, tuning pipelineTuning) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		cliDef.commands.version.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Debug("Command: version")
			printVersionInfo()
			return nil
		},
		cliDef.commands.tui.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: tui", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.TUICmd(ctx, dbOpts)
		},
		cliDef.commands.store.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: store",
				zap.String("file", *cliDef.flags.evipath),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.String("store_pipeline_mode", cnst.StorePipelineMode),
				zap.String("store_io_engine", cnst.StoreIOEngine),
				zap.String("hash_strategy", cnst.StoreHashStrategy),
				zap.Bool("sync_index", *cliDef.flags.syncIndex),
				zap.Bool("no_index", *cliDef.flags.noIndex),
				zap.Bool("fts_enabled", *cliDef.flags.enableFTS),
				zap.Bool("enrichment_enabled", *cliDef.flags.enableEnrichment),
				zap.Bool("reverse_relations_enabled", cnst.ENABLEREVREL),
				zap.Int("store_workers", *cliDef.flags.storeWorkers),
				zap.Int("store_queue", *cliDef.flags.storeQueue),
				zap.Int("store_workers_effective", tuning.storeWorkers),
				zap.Int("store_queue_effective", tuning.storeQueue))
			return cli.StoreDataWithOptions(ctx, dbOpts, *cliDef.flags.evipath, cli.StoreOptions{
				NoIndex:         *cliDef.flags.noIndex,
				FTS:             *cliDef.flags.enableFTS,
				Enrich:          *cliDef.flags.enableEnrichment,
				Decode:          true,
				RequireVerified: *cliDef.flags.requireVerified,
			})
		},
		cliDef.commands.restore.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: restore",
				zap.String("hash", *cliDef.flags.rhash),
				zap.String("output_path", *cliDef.flags.rpath),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.Int("restore_workers", *cliDef.flags.restoreWorkers),
				zap.Int("restore_queue", *cliDef.flags.restoreQueue),
				zap.Int("restore_progress_ms", *cliDef.flags.restoreProgressMs),
				zap.Int("restore_buffer_mb", *cliDef.flags.restoreBufferMB),
				zap.Int("restore_workers_effective", tuning.restoreWorkers),
				zap.Int("restore_queue_effective", tuning.restoreQueue))
			verify, err := store.ParseVerifyLevel(*cliDef.flags.restoreVerify)
			if err != nil {
				return lifecycle.Usagef("%v", err)
			}
			return cli.RestoreData(ctx, dbOpts, *cliDef.flags.rhash, *cliDef.flags.rpath, cli.RestoreOptions{Force: *cliDef.flags.restoreForce, Verify: verify, VerifyEWF: *cliDef.flags.restoreVerifyEWF})
		},
		cliDef.commands.list.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: list",
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.String("status", strings.ToLower(*cliDef.flags.listStatus)))
			return cli.ListData(ctx, dbOpts, *cliDef.flags.listStatus)
		},
		cliDef.commands.stats.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: stats", zap.String("dbpath", *cliDef.flags.dbpath), zap.Int("chonksize_kb", *cliDef.flags.chonkSize))
			return cli.StatsData(ctx, dbOpts)
		},
		cliDef.commands.repair.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: repair",
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.Bool("fix", *cliDef.flags.repairFix),
				zap.Bool("upgrade", *cliDef.flags.repairUpg))
			return cli.RepairData(ctx, dbOpts, cli.RepairOptions{
				Fix:           *cliDef.flags.repairFix,
				Upgrade:       *cliDef.flags.repairUpg,
				Yes:           *cliDef.flags.repairYes,
				Verify:        *cliDef.flags.repairVfy,
				ReindexFailed: *cliDef.flags.repairReindex,
				AddRevrel:     *cliDef.flags.repairRevrel,
			})
		},
		cliDef.commands.reindex.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: reindex",
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Strings("ids", *cliDef.flags.reindexIDs),
				zap.Bool("failed", *cliDef.flags.reindexFailed),
				zap.Bool("all", *cliDef.flags.reindexAll))
			return cli.ReindexData(ctx, dbOpts, cli.ReindexOptions{
				IDs:    *cliDef.flags.reindexIDs,
				Failed: *cliDef.flags.reindexFailed,
				All:    *cliDef.flags.reindexAll,
				Source: *cliDef.flags.reindexSource,
				FTS:    *cliDef.flags.reindexFTS,
				Enrich: *cliDef.flags.reindexEnrich,
			})
		},
		cliDef.commands.nearIn.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: near in",
				zap.String("hash", *cliDef.flags.inhash),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Bool("deep", *cliDef.flags.deep),
				zap.Int("topk", *cliDef.flags.inTopK),
				zap.Bool("verify", *cliDef.flags.inVerify))
			return cli.NearInData(ctx, dbOpts, *cliDef.flags.inhash, cli.NearOptions{
				Deep: *cliDef.flags.deep, Verify: *cliDef.flags.inVerify, TopK: *cliDef.flags.inTopK,
				Out: *cliDef.flags.inOut, Force: *cliDef.flags.inForce,
			})
		},
		cliDef.commands.nearOut.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: near out",
				zap.String("file_path", *cliDef.flags.outpath),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Bool("deep", *cliDef.flags.outDeep),
				zap.Int("topk", *cliDef.flags.outTopK),
				zap.Bool("verify", *cliDef.flags.outVerify),
				zap.Bool("explain_exact", *cliDef.flags.outExplainExact))
			return cli.NearOutData(ctx, dbOpts, *cliDef.flags.outpath, cli.NearOptions{
				Deep: *cliDef.flags.outDeep, Verify: *cliDef.flags.outVerify, TopK: *cliDef.flags.outTopK,
				ExplainExact: *cliDef.flags.outExplainExact, Out: *cliDef.flags.outOut, Force: *cliDef.flags.outForce,
			})
		},
		cliDef.commands.search.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: search",
				zap.String("query", *cliDef.flags.query),
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Float64("rank_alpha", *cliDef.flags.rankAlpha),
				zap.Bool("fulltext", *cliDef.flags.fullText))
			return cli.SearchCmd(ctx, dbOpts, cli.SearchOptions{
				Query:     *cliDef.flags.query,
				RankAlpha: *cliDef.flags.rankAlpha,
				FullText:  *cliDef.flags.fullText,
				Out:       *cliDef.flags.searchOut,
				Force:     *cliDef.flags.searchForce,
			})
		},
		cliDef.commands.microExtract.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: micro extract",
				zap.String("dbpath", *cliDef.flags.dbpath),
				zap.Int("topk", *cliDef.flags.microExtractTopK),
				zap.Bool("force", *cliDef.flags.microExtractForce))
			return cli.MicroArtefactCmd(ctx, dbOpts, cli.MicroOptions{
				Force:     *cliDef.flags.microExtractForce,
				TopK:      *cliDef.flags.microExtractTopK,
				Out:       *cliDef.flags.microExtractOut,
				Overwrite: *cliDef.flags.microExtractOverwrite,
			})
		},
		cliDef.commands.microList.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: micro list", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.ListMicroArtefactsCmd(ctx, dbOpts)
		},
		cliDef.commands.enrich.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: enrich", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.EnrichData(ctx, dbOpts)
		},
		cliDef.commands.reset.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: reset", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.ResetData(*cliDef.flags.dbpath, *cliDef.flags.resetYes)
		},
		cliDef.commands.server.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: server", zap.String("dbpath", *cliDef.flags.dbpath), zap.Int("chonksize_kb", *cliDef.flags.chonkSize))
			token, warnings, err := resolveServerToken(cliDef.flags)
			if err != nil {
				return err
			}
			for _, w := range warnings {
				logging.GetLogger().Warn("server token", zap.String("detail", w))
				fmt.Fprintln(os.Stderr, "dues: warning:", w)
			}
			return api.Server(ctx, dbOpts, api.Options{
				Listen:      *cliDef.flags.serverListen,
				Token:       token,
				CORSOrigins: *cliDef.flags.serverCORSOrigins,
			})
		},
	}
}

// handle stops start-up when a codec cannot be created. It runs from init,
// before realMain can report errors, so it exits directly.
func handle(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "dues: internal error during start-up: %v\n", err)
		os.Exit(lifecycle.ExitInternal)
	}
}
