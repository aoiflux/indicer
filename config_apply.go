package main

// Turning the parsed command line into runtime settings and credentials.

import (
	"bufio"
	"errors"
	"fmt"
	"indicer/internal/appconfig"
	"indicer/internal/lifecycle"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"io"
	"os"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/fatih/color"
	"github.com/klauspost/compress/zstd"
)

type pipelineTuning struct {
	storeWorkers   int
	storeQueue     int
	restoreWorkers int
	restoreQueue   int
	activePreset   string
	issues         []appconfig.Issue
	// layout is the database layout the command line asks for.
	layout dbopen.Requested
}

// Password sources, highest precedence first: --password-file,
// --password-stdin, DUES_PASSWORD, --password.
const (
	flagPasswordFile  = "password-file"
	flagPasswordStdin = "password-stdin"
	envPassword       = "DUES_PASSWORD"
)

// resolveCredentials returns the database password from the first source
// that has one, and warnings for the user. An empty password is no password.
func resolveCredentials(f cliFlags, set map[string]bool, stdin io.Reader) (dbmeta.Credentials, []string, error) {
	var warnings []string
	pw := ""
	switch {
	case *f.pwdFile != "":
		b, err := os.ReadFile(*f.pwdFile)
		if err != nil {
			return dbmeta.Credentials{}, nil, lifecycle.Usagef("--%s: %v", flagPasswordFile, err)
		}
		pw = firstLine(string(b))
		if pw == "" {
			return dbmeta.Credentials{}, nil, lifecycle.Usagef("--%s: %s holds no password", flagPasswordFile, *f.pwdFile)
		}
	case *f.pwdStdin:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return dbmeta.Credentials{}, nil, lifecycle.Usagef("--%s: %v", flagPasswordStdin, err)
		}
		pw = firstLine(line)
		if pw == "" {
			return dbmeta.Credentials{}, nil, lifecycle.Usagef("--%s: no password on standard input", flagPasswordStdin)
		}
	case os.Getenv(envPassword) != "":
		pw = os.Getenv(envPassword)
	case set[cnst.FlagPassword] && *f.pwd != "":
		pw = *f.pwd
		warnings = append(warnings, fmt.Sprintf("--%s on the command line is visible to other users of this machine; prefer --%s, --%s or %s",
			cnst.FlagPassword, flagPasswordFile, flagPasswordStdin, envPassword))
	}
	if pw == "" {
		return dbmeta.NoPassword(), warnings, nil
	}
	return dbmeta.Password(pw), warnings, nil
}

// firstLine returns s up to its first line break.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// applyRuntimeConfig validates the command line and sets the runtime
// globals from it. Every rejected flag is reported together, as a usage error
// (exit code 2). Adjustments that are not errors come back in tuning.issues.
func applyRuntimeConfig(parsed string, args []string, cliDef cliDefinition) (pipelineTuning, error) {
	tuning := pipelineTuning{}
	f := cliDef.flags

	set, err := flagsSetByUser(cliDef.app, args)
	if err != nil {
		return tuning, lifecycle.Usagef("%v (see dues --help)", err)
	}
	raw := appconfig.Raw{
		Command:           parsed,
		ChunkKiB:          *f.chonkSize,
		Preset:            *f.preset,
		QuickModePreset:   *f.quickMode,
		PerformancePreset: *f.performanceMode,
		LowResourcePreset: *f.lowResourceMode,
		Low:               *f.memopt,
		Quick:             *f.quickOpt,
		Container:         *f.containerMode,
		Hierarchical:      *f.hierarchicalIndex,
		CompressLevel:     *f.compressLevel,
		StoreWorkers:      *f.storeWorkers,
		StoreQueue:        *f.storeQueue,
		RestoreWorkers:    *f.restoreWorkers,
		RestoreQueue:      *f.restoreQueue,
		RestoreProgressMs: *f.restoreProgressMs,
		RestoreBufferMB:   *f.restoreBufferMB,
		ListStatus:        *f.listStatus,
		RankAlpha:         *f.rankAlpha,
		MicroTopK:         *f.microExtractTopK,
		Set:               set,
	}
	switch parsed {
	case cliDef.commands.nearIn.FullCommand():
		raw.NearTopK = *f.inTopK
	case cliDef.commands.nearOut.FullCommand():
		raw.NearTopK = *f.outTopK
	}

	eff, issues, err := appconfig.Resolve(raw, appconfig.Machine())
	if err != nil {
		return tuning, err
	}
	tuning.issues = issues
	tuning.activePreset = eff.Preset

	cnst.MEMOPT = eff.Low
	cnst.QUICKOPT = eff.Quick
	cnst.CONTAINERMODE = eff.Container
	cnst.HIERARCHICALINDEX = eff.Hierarchical
	cnst.CompressLevel = eff.CompressLevel

	selectedHashAlgo := cnst.BLAKE3
	switch parsed {
	case cliDef.commands.store.FullCommand():
		selectedHashAlgo = *f.storeHashAlgo
	case cliDef.commands.server.FullCommand():
		selectedHashAlgo = *f.serverHashAlgo
	}
	if err := cnst.SetHashAlgo(selectedHashAlgo); err != nil {
		return tuning, lifecycle.Usagef("--%s: %v", cnst.FlagHashAlgo, err)
	}
	tuning.layout = dbopen.Requested{
		ChunkKiB:     eff.ChunkKiB,
		Container:    eff.Container,
		Hierarchical: eff.Hierarchical,
		Quick:        eff.Quick,
		Set:          set,
	}
	if set[cnst.FlagHashAlgo] {
		tuning.layout.ChunkHash = cnst.HASHALGO
	}

	if parsed == cliDef.commands.store.FullCommand() {
		if err := cnst.SetStoreHashStrategy(*f.storeHashStrategy); err != nil {
			return tuning, lifecycle.Usagef("--%s: %v", cnst.FlagHashStrategy, err)
		}
		if err := cnst.SetStorePipeline(*f.storePipeline); err != nil {
			return tuning, lifecycle.Usagef("--%s: %v", cnst.FlagStorePipeline, err)
		}
		if err := cnst.SetHochoMode(*f.hochoMode); err != nil {
			return tuning, lifecycle.Usagef("--%s: %v", cnst.FlagHochoMode, err)
		}
		if cnst.StoreHashStrategy != cnst.HochoHashStrategy && set[cnst.FlagHochoMode] {
			return tuning, lifecycle.Usagef("--%s is only valid when --%s=hocho", cnst.FlagHochoMode, cnst.FlagHashStrategy)
		}

		cnst.ENABLESIMHASH = *f.enableSimhash
		cnst.ENABLEREVREL = *f.enableRevRel
		cnst.StoreAsyncFileWrites = *f.storeAsyncFileWrite
		if err := cnst.SetStoreIOEngine(*f.storeIOEngine); err != nil {
			return tuning, lifecycle.Usagef("--%s: %v", cnst.FlagStoreIOEngine, err)
		}
		if err := cnst.SetStoreIOUringQueueDepth(*f.storeIOUringQueue); err != nil {
			return tuning, lifecycle.Usagef("--%s: %v", cnst.FlagStoreIOUringQueueDepth, err)
		}
	}

	// Resolve has already fitted these to the machine's memory; the pipelines
	// read them through cnst.GetStorePipelineTuning/GetRestorePipelineTuning.
	if eff.StoreWorkers > 0 {
		cnst.StoreWorkerCount = eff.StoreWorkers
		cnst.StoreTaskQueueDepth = eff.StoreQueue
		tuning.storeWorkers, tuning.storeQueue = cnst.GetStorePipelineTuning(cnst.CONTAINERMODE, cnst.HIERARCHICALINDEX)
	}
	if eff.RestoreWorkers > 0 {
		cnst.RestoreWorkerCount = eff.RestoreWorkers
		cnst.RestoreTaskQueueDepth = eff.RestoreQueue
		cnst.RestoreProgressIntervalMs = eff.RestoreProgressMs
		cnst.RestoreWriteBufferMB = eff.RestoreBufferMB
		tuning.restoreWorkers, tuning.restoreQueue = cnst.GetRestorePipelineTuning()
	}

	return tuning, nil
}

// flagsSetByUser returns the long names of the flags given on the command
// line, in any form (long, short or negated). Defaults do not count, and
// neither does an operand that looks like a flag.
func flagsSetByUser(app *kingpin.Application, args []string) (map[string]bool, error) {
	ctx, err := app.ParseContext(args)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, el := range ctx.Elements {
		if fc, ok := el.Clause.(*kingpin.FlagClause); ok {
			set[fc.Model().Name] = true
		}
	}
	return set, nil
}

func reconfigureEncoderForCompressionLevel() error {
	if err := cnst.ENCODER.Close(); err != nil {
		return err
	}

	encLevel := zstd.SpeedBestCompression
	switch cnst.CompressLevel {
	case "fast":
		encLevel = zstd.SpeedFastest
	case "default":
		encLevel = zstd.SpeedDefault
	case "best":
		encLevel = zstd.SpeedBestCompression
	}

	var err error
	cnst.ENCODER, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(encLevel))
	return err
}

func printRuntimeBanner(parsed string, cliDef cliDefinition, tuning pipelineTuning) {
	if parsed == cliDef.commands.tui.FullCommand() || parsed == cliDef.commands.version.FullCommand() {
		return
	}
	if tuning.activePreset != "" {
		color.Cyan("🎛 active preset: %s", tuning.activePreset)
	}

	if cnst.MEMOPT {
		color.Green("🍃 running in LOW RESOURCE mode 🍃")
	} else {
		color.Cyan("⚡ running in HIGH PERFORMANCE mode (CPU x2 workers) ⚡")
	}
	if cnst.QUICKOPT {
		color.Magenta("🛫 quick mode enabled 🛬")
	}
	if cnst.CONTAINERMODE {
		color.Yellow("📦 container mode enabled 📦")
	}
	if cnst.HIERARCHICALINDEX {
		color.Magenta("🏛 hierarchical index enabled (2-level lookup) 🏛")
	}
	if cnst.CompressLevel != "default" {
		color.Yellow("🗜  chunk compression level: %s", cnst.CompressLevel)
	}
	if cnst.ENABLESIMHASH {
		color.Yellow("🔍 simhash enabled (--simhash)")
	}
	if cnst.ENABLEREVREL {
		color.Yellow("↩ reverse relations enabled (--%s)", cnst.FlagEnableRevRel)
	}

	if parsed == cliDef.commands.store.FullCommand() {
		color.Cyan("⚙ store pipeline: workers=%d queue=%d", tuning.storeWorkers, tuning.storeQueue)
		color.Cyan("⚙ store pipeline mode: %s", cnst.StorePipelineMode)
		color.Cyan("⚙ store io engine: %s", cnst.StoreIOEngine)
		if cnst.StoreAsyncFileWrites {
			color.Yellow("⚗ async file writes: enabled (experimental)")
		}
		if cnst.StoreHashStrategy == cnst.HochoHashStrategy {
			color.Cyan("⚙ hocho mode: %s", cnst.HochoMode)
		}
	}

	if parsed == cliDef.commands.restore.FullCommand() {
		color.Cyan("⚙ restore pipeline: workers=%d queue=%d progress=%s buffer=%dMB", tuning.restoreWorkers, tuning.restoreQueue, cnst.GetRestoreProgressInterval(), cnst.GetRestoreWriteBufferSize()/(1024*1024))
	}
}
