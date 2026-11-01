package main

import (
	"context"
	"errors"
	"fmt"
	"indicer/api"
	"indicer/cli"
	"indicer/internal/console"
	"indicer/internal/crash"
	"indicer/internal/lifecycle"
	"indicer/internal/osdirs"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/fio"
	"indicer/lib/logging"
	"indicer/lib/store"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	duesVersion  = "0.39.0"
	duesCodename = "<pineapple> spacebar"
)

// buildCommit is the short commit the build came from, with "-dirty" for
// uncommitted changes. The build scripts set it:
// -ldflags "-X main.buildCommit=<commit>". Other builds leave it empty.
var buildCommit string

func buildInfo() ui.Build {
	return ui.Build{Version: duesVersion, Codename: duesCodename, Commit: buildCommit}
}

func init() {
	var err error

	cnst.DECODER, err = cnst.NewDecoder()
	handle(err)

	cnst.ENCODER, err = cnst.NewEncoder(cnst.CompressLevel, cnst.GetMaxThreadCount())
	handle(err)
}

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain runs DUES and returns the process exit code. Everything that must
// happen on the way out (flushing the log, keeping or removing the crash
// file) is deferred here, so nothing below may call os.Exit.
func realMain(args []string) (code int) {
	// Presentation is decided first: help and version print before kingpin
	// parses, and must already honour --color, --plain and --ci.
	uiOpts, helpArgs := ui.PreScan(args)
	defer ui.Setup(uiOpts)()

	if maybeHandleHelp(helpArgs) {
		return lifecycle.ExitOK
	}

	// The log and the crash file exist before kingpin parses, so a parse
	// error is logged too; their flags are read from the raw arguments.
	tempDir := flagFromArgs(args, flagTempDir)
	logInfo := logging.Init(logging.Config{
		Component: "dues",
		Dir:       flagFromArgs(args, flagLogDir),
		TempDir:   tempDir,
		Debug:     boolFlagFromArgs(args, flagDebug),
		// Warnings about the log itself go through console, so they print
		// above a progress display instead of through it.
		Stderr: console.ErrSink,
	})
	defer logging.Close()

	crashDir := logInfo.Dir
	if crashDir == "" {
		if temp, err := osdirs.TempOr(tempDir); err == nil {
			crashDir = filepath.Join(temp, "dues-crash")
		}
	}
	if crashDir == "" {
		logging.GetLogger().Warn("crash output not captured: no log or temporary directory")
	} else if _, err := crash.Setup(crashDir, logInfo.RunID); err != nil {
		logging.GetLogger().Warn("crash output not captured", zap.Error(err))
	}
	defer crash.Finish()

	logging.ReportPanics()

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
		zap.String("commit", buildCommit),
		zap.Strings("args", logging.RedactArgs(args)))

	err := run(args)
	if cerr := closeCompressionCodecs(); err == nil {
		err = cerr
	}
	if err != nil {
		logging.GetLogger().Error("command failed", zap.Error(err))
		level := ui.LevelError
		if lifecycle.IsWarning(err) {
			level = ui.LevelWarning
		}
		ui.Message(console.Err(), level, err.Error())
	}
	return lifecycle.ExitCode(err)
}

// flagFromArgs finds the value of a string flag (--name V or --name=V)
// before kingpin parses the command line. kingpin validates it later.
func flagFromArgs(args []string, name string) string {
	flag := "--" + name
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

// boolFlagFromArgs finds a boolean flag (--name, or --no-name to undo it;
// the last one counts) before kingpin parses the command line.
func boolFlagFromArgs(args []string, name string) bool {
	on := false
	for _, a := range args {
		if a == "--" {
			break
		}
		switch a {
		case "--" + name:
			on = true
		case "--no-" + name:
			on = false
		}
	}
	return on
}

// checkTempDir fails fast on a --temp-dir that is not an existing
// directory, before anything is written there.
func checkTempDir(dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return lifecycle.Usagef("--%s: %v", flagTempDir, err)
	}
	if !info.IsDir() {
		return lifecycle.Usagef("--%s %s is not a directory", flagTempDir, dir)
	}
	return nil
}

func run(args []string) error {
	cliDef := newCLIDefinition()

	// kingpin exits the process itself after its own help and usage output.
	// Record the code instead, so realMain's deferred cleanup still runs.
	terminated := -1
	cliDef.app.Terminate(func(code int) {
		if terminated < 0 {
			terminated = code
		}
	})
	parsed, err := cliDef.app.Parse(args)
	if errors.Is(err, errVersionShown) {
		return nil
	}
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
	if err := cliDef.faults.apply(); err != nil {
		return err
	}

	tempDir := *cliDef.flags.tempDir
	if err := checkTempDir(tempDir); err != nil {
		return err
	}
	// Restores share one container cache per process, so its spill
	// directory is a process setting.
	fio.SetContainerSpillDir(tempDir)

	tuning, err := applyRuntimeConfig(parsed, args, cliDef)
	if err != nil {
		return err
	}
	for _, issue := range tuning.issues {
		logging.GetLogger().Warn("setting adjusted", zap.String("flag", issue.Flag), zap.String("detail", issue.Message))
		ui.Warnf("%s", issue)
	}

	if err := reconfigureEncoderForCompressionLevel(tuning.storeWorkers); err != nil {
		return err
	}

	creds, err := resolveCredentials(cliDef.flags, tuning.layout.Set, os.Stdin)
	if err != nil {
		return err
	}
	dbOpts := cli.DBOptions{Path: *cliDef.flags.dbpath, Creds: creds, Layout: tuning.layout, TempDir: tempDir}
	logSettings(cliDef.flags, tuning.layout.Set)

	printRunHeader(parsed, cliDef, tuning)

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
	if showsProgress(parsed, cliDef) {
		handler = func(ctx context.Context) error {
			start := time.Now()
			err := ui.Run(ctx, handlers[parsed])
			if err == nil && eggsAllowed(parsed, cliDef) {
				ui.MaybeEgg(parsed, time.Since(start))
			}
			return err
		}
	}
	// A command cut short by a signal exits with the signal's code.
	return lifecycle.Cause(ctx, handler(ctx))
}

// eggsAllowed reports whether an extra may follow a command: not repair,
// whose --fix, --upgrade and --verify write JSON to stdout.
func eggsAllowed(parsed string, cliDef cliDefinition) bool {
	return parsed != cliDef.commands.repair.FullCommand()
}

// showsProgress reports whether a command shows progress: the long
// operations. Quick commands, the TUI and the server do not.
func showsProgress(parsed string, cliDef cliDefinition) bool {
	c, f := cliDef.commands, cliDef.flags
	switch parsed {
	case c.store.FullCommand(), c.restore.FullCommand(), c.reindex.FullCommand(), c.enrich.FullCommand(),
		c.nearIn.FullCommand(), c.nearOut.FullCommand(), c.microExtract.FullCommand(), c.search.FullCommand():
		return true
	case c.repair.FullCommand():
		return *f.repairFix || *f.repairUpg || *f.repairVfy
	}
	return false
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
		cliDef.commands.coffee.FullCommand():  func(context.Context) error { ui.Coffee(); return nil },
		cliDef.commands.fortune.FullCommand(): func(context.Context) error { ui.Fortune(); return nil },
		cliDef.commands.mew.FullCommand():     func(context.Context) error { ui.Meow(); return nil },
		cliDef.commands.server.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: server", zap.String("dbpath", *cliDef.flags.dbpath), zap.Int("chonksize_kb", *cliDef.flags.chonkSize))
			token, err := resolveServerToken(cliDef.flags, tuning.layout.Set)
			if err != nil {
				return err
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
