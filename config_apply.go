package main

// Turning the parsed command line into runtime settings and credentials.

import (
	"bufio"
	"errors"
	"fmt"
	"indicer/internal/appconfig"
	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
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

// The password comes from --password-file or --password-stdin, never from
// the command line or the environment, which other users of the machine
// can read.
const (
	flagPasswordFile  = "password-file"
	flagPasswordStdin = "password-stdin"
)

// resolveCredentials returns the database password from --password-file,
// or else --password-stdin. An empty password is no password. The removed
// --password (-p) is refused with what replaces it.
func resolveCredentials(f cliFlags, set map[string]bool, stdin io.Reader) (dbmeta.Credentials, error) {
	if set[cnst.FlagPassword] {
		return dbmeta.Credentials{}, lifecycle.Usagef("--%s (-%c) was removed: a password on the command line is visible to other users of this machine; use --%s or --%s",
			cnst.FlagPassword, cnst.FlagPasswordShort, flagPasswordFile, flagPasswordStdin)
	}
	pw := ""
	switch {
	case *f.pwdFile != "":
		b, err := os.ReadFile(*f.pwdFile)
		if err != nil {
			return dbmeta.Credentials{}, lifecycle.Usagef("--%s: %v", flagPasswordFile, err)
		}
		pw = firstLine(string(b))
		if pw == "" {
			return dbmeta.Credentials{}, lifecycle.Usagef("--%s: %s holds no password", flagPasswordFile, *f.pwdFile)
		}
	case *f.pwdStdin:
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return dbmeta.Credentials{}, lifecycle.Usagef("--%s: %v", flagPasswordStdin, err)
		}
		pw = firstLine(line)
		if pw == "" {
			return dbmeta.Credentials{}, lifecycle.Usagef("--%s: no password on standard input", flagPasswordStdin)
		}
	}
	if pw == "" {
		return dbmeta.NoPassword(), nil
	}
	return dbmeta.Password(pw), nil
}

// The server's bearer token comes from --token-file only, for the same
// reason as the password.
const (
	flagListen     = "listen"
	flagToken      = "token"
	flagTokenFile  = "token-file"
	flagCORSOrigin = "cors-origin"
)

// resolveServerToken returns the server's bearer token from --token-file;
// none is no token. The removed --token is refused with what replaces it.
func resolveServerToken(f cliFlags, set map[string]bool) (string, error) {
	if set[flagToken] {
		return "", lifecycle.Usagef("--%s was removed: a token on the command line is visible to other users of this machine; use --%s", flagToken, flagTokenFile)
	}
	if *f.serverTokenFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(*f.serverTokenFile)
	if err != nil {
		return "", lifecycle.Usagef("--%s: %v", flagTokenFile, err)
	}
	tok := strings.TrimSpace(firstLine(string(b)))
	if tok == "" {
		return "", lifecycle.Usagef("--%s: %s holds no token", flagTokenFile, *f.serverTokenFile)
	}
	return tok, nil
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
		ChunkKiB:  eff.ChunkKiB,
		Container: eff.Container,
		Quick:     eff.Quick,
		Set:       set,
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
		// Retired with W6: the index pass runs beside storage, and partition
		// and indexed-file hashes are plain hashes (ING-20), so neither
		// setting changes anything.
		if set[cnst.FlagHochoMode] {
			tuning.issues = append(tuning.issues, appconfig.Issue{
				Flag:    cnst.FlagHochoMode,
				Message: "is retired and has no effect; partition and indexed-file hashes are always plain hashes",
			})
		}
		if set[cnst.FlagSyncIndex] {
			tuning.issues = append(tuning.issues, appconfig.Issue{
				Flag:    cnst.FlagSyncIndex,
				Message: "is retired and has no effect; the index pass always runs beside storage",
			})
		}

		cnst.ENABLESIMHASH = *f.enableSimhash
		cnst.ENABLEREVREL = *f.enableRevRel
		if set[cnst.FlagStoreAsyncFileWrite] {
			// Retired with STO-4: every chunk blob is now synced before its
			// metadata is written.
			tuning.issues = append(tuning.issues, appconfig.Issue{
				Flag:    cnst.FlagStoreAsyncFileWrite,
				Message: "is deprecated and has no effect; chunk blobs are always written durably",
			})
		}
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
		tuning.storeWorkers, tuning.storeQueue = cnst.GetStorePipelineTuning(cnst.CONTAINERMODE)
	}
	if parsed == cliDef.commands.restore.FullCommand() && set[cnst.FlagRestoreProgressMs] {
		// Retired with the progress display, which samples on its own.
		tuning.issues = append(tuning.issues, appconfig.Issue{
			Flag:    cnst.FlagRestoreProgressMs,
			Message: "is retired and has no effect; the progress display updates on its own",
		})
	}
	if eff.RestoreWorkers > 0 {
		cnst.RestoreWorkerCount = eff.RestoreWorkers
		cnst.RestoreTaskQueueDepth = eff.RestoreQueue
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

// reconfigureEncoderForCompressionLevel replaces the process encoder with one
// at --compress-level, sized for the store workers that encode at once.
func reconfigureEncoderForCompressionLevel(workers int) error {
	if err := cnst.ENCODER.Close(); err != nil {
		return err
	}
	var err error
	cnst.ENCODER, err = cnst.NewEncoder(cnst.CompressLevel, workers)
	return err
}

// printRunHeader opens a long operation on stderr: what runs, on what,
// into which database, and the settings in effect. Quick commands (list,
// stats, version, ...) print none, so their output stays as it was; nothing
// here reaches stdout, so JSON on stdout stays pure.
func printRunHeader(parsed string, cliDef cliDefinition, tuning pipelineTuning) {
	f, c := cliDef.flags, cliDef.commands
	h := ui.Header{Version: duesVersion, Command: parsed}
	switch parsed {
	case c.store.FullCommand():
		h.Subject = *f.evipath
	case c.restore.FullCommand():
		h.Subject = *f.rhash
	case c.reindex.FullCommand():
		switch {
		case *f.reindexAll:
			h.Subject = "all evidence"
		case *f.reindexFailed:
			h.Subject = "evidence with a failed index"
		default:
			h.Subject = strings.Join(*f.reindexIDs, ", ")
		}
	case c.nearIn.FullCommand():
		h.Subject = *f.inhash
	case c.nearOut.FullCommand():
		h.Subject = *f.outpath
	case c.search.FullCommand():
		h.Subject = *f.query
	case c.repair.FullCommand():
		if !*f.repairFix && !*f.repairUpg && !*f.repairVfy {
			return
		}
		var modes []string
		for _, m := range []struct {
			on   bool
			flag string
		}{{*f.repairVfy, "--verify"}, {*f.repairUpg, "--upgrade"}, {*f.repairFix, "--fix"}, {*f.repairRevrel, "--revrel"}, {*f.repairReindex, "--reindex-failed"}} {
			if m.on {
				modes = append(modes, m.flag)
			}
		}
		h.Subject = strings.Join(modes, " ")
	case c.server.FullCommand():
		h.Subject = strings.Join(*f.serverListen, ", ")
	case c.enrich.FullCommand(), c.microExtract.FullCommand():
	default:
		return
	}
	if root, err := dbopen.ResolveRoot(*f.dbpath); err == nil {
		h.Target = root
	} else {
		h.Target = *f.dbpath
	}
	h.Settings = runSettings(parsed, cliDef, tuning)
	ui.RunHeader(console.Err(), h)
}

// runSettings lists the settings in effect for a long operation.
func runSettings(parsed string, cliDef cliDefinition, tuning pipelineTuning) []ui.Setting {
	on := func(key, phrase string) ui.Setting { return ui.Setting{Key: key, Value: "on", Phrase: phrase} }
	var s []ui.Setting
	if tuning.activePreset != "" {
		s = append(s, ui.Setting{Key: "preset", Value: tuning.activePreset, Phrase: "preset " + tuning.activePreset})
	}
	if cnst.MEMOPT {
		s = append(s, ui.Setting{Key: "mode", Value: "low-resource", Phrase: "low-resource"})
	} else {
		s = append(s, ui.Setting{Key: "mode", Value: "performance", Phrase: "performance"})
	}
	if cnst.QUICKOPT {
		s = append(s, on("quick", "quick"))
	}
	if cnst.CONTAINERMODE {
		s = append(s, on("container", "container"))
	}
	if cnst.CompressLevel != "default" {
		s = append(s, ui.Setting{Key: "compress", Value: cnst.CompressLevel, Phrase: "zstd " + cnst.CompressLevel})
	}
	if cnst.ENABLESIMHASH {
		s = append(s, on("simhash", "simhash"))
	}
	if cnst.ENABLEREVREL {
		s = append(s, on("revrel", "revrel"))
	}
	n := func(key string, v int, phrase string) ui.Setting {
		return ui.Setting{Key: key, Value: strconv.Itoa(v), Phrase: fmt.Sprintf(phrase, v)}
	}
	switch parsed {
	case cliDef.commands.store.FullCommand():
		s = append(s,
			n("workers", tuning.storeWorkers, "%d workers"),
			n("queue", tuning.storeQueue, "queue %d"),
			ui.Setting{Key: "pipeline", Value: cnst.StorePipelineMode, Phrase: "pipeline " + cnst.StorePipelineMode},
			ui.Setting{Key: "io", Value: cnst.StoreIOEngine, Phrase: cnst.StoreIOEngine + " io"})
	case cliDef.commands.restore.FullCommand():
		s = append(s,
			n("workers", tuning.restoreWorkers, "%d workers"),
			n("queue", tuning.restoreQueue, "queue %d"),
			n("buffer_mib", cnst.GetRestoreWriteBufferSize()/(1024*1024), "%d MiB buffer"))
	}
	return s
}
