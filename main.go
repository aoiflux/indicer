package main

import (
	"context"
	"errors"
	"fmt"
	"indicer/api"
	"indicer/cli"
	"indicer/internal/console"
	"indicer/internal/crash"
	"indicer/internal/duesarc"
	"indicer/internal/lifecycle"
	"indicer/internal/osdirs"
	"indicer/internal/tracefile"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/fio"
	"indicer/lib/graph"
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

func main() {
	os.Exit(realMain(os.Args[1:]))
}

// realMain runs DUES and returns the process exit code. Everything that must
// happen on the way out (flushing the log, keeping or removing the crash
// file) is deferred here, so nothing below may call os.Exit.
func realMain(args []string) (code int) {
	// The config file is read first, and presentation is decided next: help
	// and version print before kingpin parses, and must already honour
	// --color, --plain and --ci, and the file's settings for them.
	conf, rest, confErr := readConfig(args)
	uiOpts, helpArgs := ui.PreScan(rest, conf.values.presentation())
	defer ui.Setup(uiOpts)()

	// A config file that cannot be used stops every command, help included:
	// running without the settings it holds would surprise more.
	if confErr != nil {
		ui.Message(console.Err(), ui.LevelError, confErr.Error())
		return lifecycle.ExitCode(confErr)
	}
	cliDef := newCLIDefinition()
	if maybeHandleHelp(helpArgs, cliDef.app.Model()) {
		return lifecycle.ExitOK
	}

	// The log and the crash file exist before kingpin parses, so a parse
	// error is logged too; their flags are read from the raw arguments, and
	// the file's settings stand in for flags not given.
	v := conf.values
	tempDir := flagOr(args, flagTempDir, v.TempDir)
	debugLog, given := boolFlagFromArgs(args, flagDebug)
	if !given && v.Debug != nil {
		debugLog = *v.Debug
	}
	logInfo := logging.Init(logging.Config{
		Component: "dues",
		Dir:       flagOr(args, flagLogDir, v.LogDir),
		TempDir:   tempDir,
		Debug:     debugLog,
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
	// A second Ctrl-C exits at once, past the deferred Finish; without the
	// hook each such run would leave an empty crash file in the log
	// directory, where log rotation never prunes it.
	defer lifecycle.OnForceExit(crash.Finish)()

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

	err := run(args, conf, cliDef)
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
// before kingpin parses the command line, and whether it is there.
// kingpin validates it later.
func flagFromArgs(args []string, name string) (string, bool) {
	flag := "--" + name
	for i, a := range args {
		if a == "--" {
			break
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v, true
		}
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// flagOr is the string flag's value when the command line gives it, else
// the config file's value, else "".
func flagOr(args []string, name string, file *string) string {
	if v, ok := flagFromArgs(args, name); ok || file == nil {
		return v
	}
	return *file
}

// boolFlagFromArgs finds a boolean flag (--name, or --no-name to undo it;
// the last one counts) before kingpin parses the command line, and whether
// it is there.
func boolFlagFromArgs(args []string, name string) (on, given bool) {
	for _, a := range args {
		if a == "--" {
			break
		}
		switch a {
		case "--" + name:
			on, given = true, true
		case "--no-" + name:
			on, given = false, true
		}
	}
	return on, given
}

// checkTempDir fails fast on a --temp-dir that is not an existing
// directory, before anything is written there. origin says where the value
// came from when the command line did not give it.
func checkTempDir(dir, origin string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return lifecycle.Usagef("--%s%s: %v", flagTempDir, origin, err)
	}
	if !info.IsDir() {
		return lifecycle.Usagef("--%s%s: %s is not a directory", flagTempDir, origin, dir)
	}
	return nil
}

func run(args []string, conf runConfig, cliDef cliDefinition) error {
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
	set, err := flagsSetByUser(cliDef.app, args)
	if err != nil {
		return lifecycle.Usagef("%v (see dues --help)", err)
	}
	src := conf.apply(cliDef.flags, parsed, set)
	if err := cliDef.faults.apply(); err != nil {
		return err
	}
	stopTrace, err := startTrace(*cliDef.flags.trace)
	if err != nil {
		return err
	}
	defer stopTrace()

	tempDir := *cliDef.flags.tempDir
	if err := checkTempDir(tempDir, src.originOf(flagTempDir)); err != nil {
		return err
	}
	// Restores share one container cache per process, so its spill
	// directory is a process setting.
	fio.SetContainerSpillDir(tempDir)

	tuning, err := applyRuntimeConfig(parsed, cliDef, src)
	if err != nil {
		return err
	}
	for _, issue := range tuning.issues {
		logging.GetLogger().Warn("setting adjusted", zap.String("flag", issue.Flag), zap.String("detail", issue.Message))
		ui.Warnf("%s", issue)
	}

	// Container files are written through one backend for the process.
	fio.SetWriteEngine(tuning.engine.IOEngine, tuning.engine.IOUringDepth)

	// dues config says where the password comes from; it does not read it.
	creds := dbmeta.NoPassword()
	if parsed != cliDef.commands.config.FullCommand() {
		if creds, err = resolveCredentials(cliDef.flags, src, os.Stdin); err != nil {
			return err
		}
	}
	var actor *dbopen.Actor
	var passphrase []byte
	if parsed != cliDef.commands.config.FullCommand() {
		keyCommand := parsed == cliDef.commands.actorCreate.FullCommand() || parsed == cliDef.commands.actorRotate.FullCommand()
		if actor, passphrase, err = resolveActor(cliDef.flags, src, os.Stdin, keyCommand); err != nil {
			return err
		}
	}
	dbOpts := cli.DBOptions{Path: *cliDef.flags.dbpath, Creds: creds, Layout: tuning.layout, Tuning: &tuning.engine, TempDir: tempDir,
		Graph: graph.Config{AutoCompact: *cliDef.flags.graphCompact == graphCompactAuto}, Actor: actor}
	logSettings(cliDef.flags, src, conf)

	printRunHeader(parsed, cliDef, tuning)

	// The first SIGINT/SIGTERM cancels ctx and the command winds down; a
	// second one exits at once (LOG-3).
	ctx, stop := lifecycle.Context(context.Background())
	defer stop()
	return executeCommand(ctx, parsed, cliDef, dbOpts, tuning, src, conf, passphrase)
}

// startTrace traces the run into path, when --trace gives one, until stop is
// called: when run returns, or when a second signal ends the process at
// once, so the file still reads.
func startTrace(path string) (stop func(), err error) {
	if path == "" {
		return func() {}, nil
	}
	end, err := tracefile.Start(path)
	if err != nil {
		return nil, lifecycle.Usagef("--%s: %v", flagTrace, err)
	}
	logging.GetLogger().Info("trace START", zap.String("trace_path", path))
	unhook := lifecycle.OnForceExit(func() { _ = end() })
	return func() {
		unhook()
		if err := end(); err != nil {
			logging.GetLogger().Warn("trace STOP_ERROR", zap.String("trace_path", path), zap.Error(err))
			ui.Warnf("--%s: %v", flagTrace, err)
			return
		}
		logging.GetLogger().Info("trace COMPLETE", zap.String("trace_path", path))
	}, nil
}

// newKey is the passphrase of the key dues actor create or rotate makes.
func executeCommand(ctx context.Context, parsed string, cliDef cliDefinition, dbOpts cli.DBOptions, tuning pipelineTuning, src sources, conf runConfig, newKey []byte) error {
	handlers := buildCommandHandlers(cliDef, dbOpts, tuning, src, conf, newKey)
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
// whose JSON document on stdout scripts read.
func eggsAllowed(parsed string, cliDef cliDefinition) bool {
	return parsed != cliDef.commands.repair.FullCommand()
}

// showsProgress reports whether a command shows progress: the long
// operations. Quick commands, the TUI and the server do not.
func showsProgress(parsed string, cliDef cliDefinition) bool {
	c, f := cliDef.commands, cliDef.flags
	switch parsed {
	case c.store.FullCommand(), c.restore.FullCommand(), c.reindex.FullCommand(), c.enrich.FullCommand(),
		c.nearIn.FullCommand(), c.nearOut.FullCommand(), c.microExtract.FullCommand(), c.search.FullCommand(),
		c.pkgExport.FullCommand(), c.pkgVerify.FullCommand(), c.pkgImport.FullCommand():
		return true
	case c.repair.FullCommand():
		return *f.repairFix || *f.repairUpg || *f.repairVfy
	}
	return false
}

func buildCommandHandlers(cliDef cliDefinition, dbOpts cli.DBOptions, tuning pipelineTuning, src sources, conf runConfig, newKey []byte) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		cliDef.commands.version.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Debug("Command: version")
			printVersionInfo()
			return nil
		},
		cliDef.commands.config.FullCommand(): func(context.Context) error {
			logging.GetLogger().Debug("Command: config")
			return printConfig(cliDef.app, cliDef.flags, src, conf)
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
				zap.String("store_pipeline_mode", tuning.engine.Pipeline),
				zap.String("store_io_engine", tuning.engine.IOEngine),
				zap.String("hash_strategy", tuning.engine.HashStrategy),
				zap.Bool("sync_index", *cliDef.flags.syncIndex),
				zap.Bool("no_index", *cliDef.flags.noIndex),
				zap.Bool("fts_enabled", *cliDef.flags.enableFTS),
				zap.Bool("enrichment_enabled", *cliDef.flags.enableEnrichment),
				zap.Bool("reverse_relations_enabled", *cliDef.flags.enableRevRel),
				zap.Bool("simhash_enabled", *cliDef.flags.enableSimhash),
				zap.Int("store_workers", *cliDef.flags.storeWorkers),
				zap.Int("store_queue", *cliDef.flags.storeQueue),
				zap.Int("store_jobs", *cliDef.flags.storeJobs),
				zap.Int("store_workers_effective", tuning.storeWorkers),
				zap.Int("store_queue_effective", tuning.storeQueue))
			jobs, err := cli.Jobs(cnst.FlagStoreJobs, *cliDef.flags.storeJobs, tuning.engine.LowResource)
			if err != nil {
				return err
			}
			return cli.StoreDataWithOptions(ctx, dbOpts, *cliDef.flags.evipath, cli.StoreOptions{
				NoIndex:         *cliDef.flags.noIndex,
				FTS:             *cliDef.flags.enableFTS,
				Enrich:          *cliDef.flags.enableEnrichment,
				RevRel:          *cliDef.flags.enableRevRel,
				Simhash:         *cliDef.flags.enableSimhash,
				Decode:          true,
				RequireVerified: *cliDef.flags.requireVerified,
				Jobs:            jobs,
				AutoJobs:        *cliDef.flags.storeJobs == 0,
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
				zap.Bool("all", *cliDef.flags.reindexAll),
				zap.Int("store_jobs", *cliDef.flags.reindexJobs))
			jobs, err := cli.Jobs(cnst.FlagStoreJobs, *cliDef.flags.reindexJobs, tuning.engine.LowResource)
			if err != nil {
				return err
			}
			return cli.ReindexData(ctx, dbOpts, cli.ReindexOptions{
				IDs:      *cliDef.flags.reindexIDs,
				Failed:   *cliDef.flags.reindexFailed,
				All:      *cliDef.flags.reindexAll,
				Source:   *cliDef.flags.reindexSource,
				FTS:      *cliDef.flags.reindexFTS,
				Enrich:   *cliDef.flags.reindexEnrich,
				Jobs:     jobs,
				AutoJobs: *cliDef.flags.reindexJobs == 0,
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
		cliDef.commands.pkgExport.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: package export", zap.String("dbpath", *cliDef.flags.dbpath), zap.String("out", *cliDef.flags.pkgOut))
			if n := *cliDef.flags.pkgSplitSizeMB; n < 0 {
				return lifecycle.Usagef("--split-size-mb must be 0 or more, not %d", n)
			}
			return cli.PackageExport(ctx, dbOpts, cli.PackageExportOptions{
				Output:         *cliDef.flags.pkgOut,
				SplitSizeMB:    *cliDef.flags.pkgSplitSizeMB,
				Force:          *cliDef.flags.pkgForce,
				WithoutDerived: *cliDef.flags.pkgWithoutDerived,
				HostInfo:       *cliDef.flags.pkgHostInfo,
				Creator:        duesarc.Creator{Product: "dues", Version: duesVersion, Commit: buildCommit},
			})
		},
		cliDef.commands.pkgInspect.FullCommand(): func(context.Context) error {
			logging.GetLogger().Info("Command: package inspect", zap.String("package", *cliDef.flags.pkgInspectPath))
			return cli.PackageInspect(*cliDef.flags.pkgInspectPath)
		},
		cliDef.commands.pkgVerify.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: package verify", zap.String("package", *cliDef.flags.pkgVerifyPath), zap.Bool("deep", *cliDef.flags.pkgVerifyDeep))
			return cli.PackageVerify(ctx, dbOpts, *cliDef.flags.pkgVerifyPath, *cliDef.flags.pkgVerifyDeep, *cliDef.flags.pkgVerifyTrusted)
		},
		cliDef.commands.pkgImport.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: package import", zap.String("package", *cliDef.flags.pkgImportPath), zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.PackageImport(ctx, dbOpts, *cliDef.flags.pkgImportPath, *cliDef.flags.pkgImportTrusted)
		},
		cliDef.commands.graphCompact.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: graph compact", zap.String("dbpath", *cliDef.flags.dbpath), zap.Bool("require_signed", *cliDef.flags.graphRequireSigned))
			return cli.GraphCompact(ctx, dbOpts, *cliDef.flags.graphRequireSigned)
		},
		cliDef.commands.graphStatus.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: graph status", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.GraphStatus(ctx, dbOpts)
		},
		cliDef.commands.actorCreate.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: actor create", zap.String("dbpath", *cliDef.flags.dbpath), zap.String("actor", *cliDef.flags.actorCreateName))
			return cli.ActorCreate(ctx, dbOpts, *cliDef.flags.actorCreateName, newKey)
		},
		cliDef.commands.actorList.FullCommand(): func(context.Context) error {
			logging.GetLogger().Info("Command: actor list", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.ActorList(dbOpts)
		},
		cliDef.commands.actorExport.FullCommand(): func(context.Context) error {
			logging.GetLogger().Info("Command: actor export-public", zap.String("dbpath", *cliDef.flags.dbpath), zap.String("actor", *cliDef.flags.actorExportName))
			return cli.ActorExportPublic(dbOpts, *cliDef.flags.actorExportName, *cliDef.flags.actorExportOut, *cliDef.flags.actorExportForce)
		},
		cliDef.commands.actorRotate.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: actor rotate", zap.String("dbpath", *cliDef.flags.dbpath), zap.String("actor", *cliDef.flags.actorRotateName))
			return cli.ActorRotate(ctx, dbOpts, *cliDef.flags.actorRotateName, newKey)
		},
		cliDef.commands.actorRevoke.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: actor revoke", zap.String("dbpath", *cliDef.flags.dbpath), zap.String("key_id", *cliDef.flags.actorRevokeKey))
			return cli.ActorRevoke(ctx, dbOpts, *cliDef.flags.actorRevokeKey, *cliDef.flags.actorRevokeReason)
		},
		cliDef.commands.reset.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: reset", zap.String("dbpath", *cliDef.flags.dbpath))
			return cli.ResetData(*cliDef.flags.dbpath, *cliDef.flags.resetYes)
		},
		cliDef.commands.coffee.FullCommand():  func(context.Context) error { ui.Coffee(); return nil },
		cliDef.commands.fortune.FullCommand(): func(context.Context) error { ui.Fortune(); return nil },
		cliDef.commands.mew.FullCommand():     func(context.Context) error { ui.Meow(); return nil },
		cliDef.commands.server.FullCommand(): func(ctx context.Context) error {
			logging.GetLogger().Info("Command: server", zap.String("dbpath", *cliDef.flags.dbpath), zap.Int("chonksize_kb", *cliDef.flags.chonkSize),
				zap.Int("upload_jobs", *cliDef.flags.serverUploadJobs))
			tokens, err := resolveServerTokens(cliDef.flags, src)
			if err != nil {
				return err
			}
			jobs, err := cli.Jobs(flagUploadJobs, *cliDef.flags.serverUploadJobs, tuning.engine.LowResource)
			if err != nil {
				return err
			}
			err = api.Server(ctx, dbOpts.Request(), api.Options{
				Listen:      *cliDef.flags.serverListen,
				Tokens:      tokens,
				CORSOrigins: *cliDef.flags.serverCORSOrigins,
				UploadJobs:  jobs,
			})
			// The file's listen addresses can need a token the command line
			// does not give; say where they came from.
			if _, usage := errors.AsType[*lifecycle.UsageError](err); usage && src.filled[flagListen] {
				err = &lifecycle.UsageError{Err: fmt.Errorf("%w (listen set in config file %s)", err, src.file)}
			}
			return err
		},
	}
}
