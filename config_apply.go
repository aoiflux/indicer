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
	"indicer/lib/graph/signer"
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
	// engine is how the storage engine runs on the databases the command
	// opens.
	engine cnst.Tuning
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
func resolveCredentials(f cliFlags, src sources, stdin io.Reader) (dbmeta.Credentials, error) {
	if src.set[cnst.FlagPassword] {
		return dbmeta.Credentials{}, lifecycle.Usagef("--%s (-%c) was removed: a password on the command line is visible to other users of this machine; use --%s or --%s",
			cnst.FlagPassword, cnst.FlagPasswordShort, flagPasswordFile, flagPasswordStdin)
	}
	pw := ""
	switch {
	case *f.pwdFile != "":
		origin := src.originOf(flagPasswordFile)
		b, err := os.ReadFile(*f.pwdFile)
		if err != nil {
			return dbmeta.Credentials{}, lifecycle.Usagef("--%s%s: %v", flagPasswordFile, origin, err)
		}
		pw = firstLine(string(b))
		if pw == "" {
			return dbmeta.Credentials{}, lifecycle.Usagef("--%s%s: %s holds no password", flagPasswordFile, origin, *f.pwdFile)
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

// readSecret reads a secret from the first line of the file path, or, when
// path is empty, of stdin. An empty secret is an error.
func readSecret(path string, stdin io.Reader, flagFile, flagStdin, origin, what string) ([]byte, error) {
	var line string
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, lifecycle.Usagef("--%s%s: %v", flagFile, origin, err)
		}
		if line = firstLine(string(b)); line == "" {
			return nil, lifecycle.Usagef("--%s%s: %s holds no %s", flagFile, origin, path, what)
		}
		return []byte(line), nil
	}
	got, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, lifecycle.Usagef("--%s: %v", flagStdin, err)
	}
	if line = firstLine(got); line == "" {
		return nil, lifecycle.Usagef("--%s: no %s on standard input", flagStdin, what)
	}
	return []byte(line), nil
}

// checkActorFlag checks the name --actor gives.
func checkActorFlag(name string) error {
	n, err := signer.NormalizeName(name)
	if err != nil {
		return err
	}
	if n == signer.DatabaseActor {
		return fmt.Errorf("%q is the database's own actor, which signs when --%s names no other", n, flagActor)
	}
	return nil
}

// resolveActor returns who signs the run's graph commits: the actor --actor
// names, with the passphrase of its key, or nil for the database's own. For
// dues actor create and rotate, which name their actor as an argument, it
// returns the passphrase of the new key instead, and no actor.
//
// The passphrase is read only when something needs it. Given on the command
// line with nothing to unlock, it is a usage error; from the config file, it
// is left unread.
func resolveActor(f cliFlags, src sources, stdin io.Reader, keyCommand bool) (*dbopen.Actor, []byte, error) {
	if *f.pwdStdin && *f.actorPwdStdin {
		return nil, nil, lifecycle.Usagef("--%s and --%s both read standard input; give one of them as a file", flagPasswordStdin, flagActorKeyPasswordStdin)
	}
	name := *f.actor
	if keyCommand {
		if src.set[flagActor] {
			return nil, nil, lifecycle.Usagef("--%s names who signs a command's graph commits; dues actor create and rotate take the actor's name as an argument", flagActor)
		}
		name = ""
	}
	given := *f.actorPwdFile != "" || *f.actorPwdStdin
	if name == "" && !keyCommand {
		if src.set[flagActorKeyPasswordFile] || *f.actorPwdStdin {
			return nil, nil, lifecycle.Usagef("--%s and --%s unlock the key of the actor --%s names; name one", flagActorKeyPasswordFile, flagActorKeyPasswordStdin, flagActor)
		}
		return nil, nil, nil
	}
	if !given {
		if keyCommand {
			return nil, nil, nil
		}
		return nil, nil, lifecycle.Usagef("--%s%s needs the passphrase of its key: --%s or --%s", flagActor, src.originOf(flagActor), flagActorKeyPasswordFile, flagActorKeyPasswordStdin)
	}
	pass, err := readSecret(*f.actorPwdFile, stdin, flagActorKeyPasswordFile, flagActorKeyPasswordStdin, src.originOf(flagActorKeyPasswordFile), "passphrase")
	if err != nil {
		return nil, nil, err
	}
	if keyCommand {
		return nil, pass, nil
	}
	if err := checkActorFlag(name); err != nil {
		return nil, nil, lifecycle.Usagef("--%s%s: %v", flagActor, src.originOf(flagActor), err)
	}
	return &dbopen.Actor{Name: name, Passphrase: pass}, nil, nil
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
func resolveServerToken(f cliFlags, src sources) (string, error) {
	if src.set[flagToken] {
		return "", lifecycle.Usagef("--%s was removed: a token on the command line is visible to other users of this machine; use --%s", flagToken, flagTokenFile)
	}
	if *f.serverTokenFile == "" {
		return "", nil
	}
	origin := src.originOf(flagTokenFile)
	b, err := os.ReadFile(*f.serverTokenFile)
	if err != nil {
		return "", lifecycle.Usagef("--%s%s: %v", flagTokenFile, origin, err)
	}
	tok := strings.TrimSpace(firstLine(string(b)))
	if tok == "" {
		return "", lifecycle.Usagef("--%s%s: %s holds no token", flagTokenFile, origin, *f.serverTokenFile)
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

// databaseChunkKiB is the chunk size of an existing database at a --dbpath
// (dbopen.ChunkKiBOf); tests replace it.
var databaseChunkKiB = dbopen.ChunkKiBOf

// applyRuntimeConfig validates the settings, from the command line and the
// config file, and returns the layout and engine tuning they ask for. It
// sets nothing process-wide. Every rejected flag is reported together, as a
// usage error (exit code 2). Adjustments that are not errors come back in
// tuning.issues.
func applyRuntimeConfig(parsed string, cliDef cliDefinition, src sources) (pipelineTuning, error) {
	tuning := pipelineTuning{engine: cnst.DefaultTuning()}
	f := cliDef.flags
	set := src.set

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
		Set:               src.explicit(),
		Origin:            src.origins(),
	}
	switch parsed {
	case cnst.CmdStore, cnst.CmdServer, cnst.CmdTui, cnst.CmdRestore:
		// Their workers and queues hold chunks: the fit needs the size of
		// the database's own, which a --chonksize cannot change.
		raw.DBChunkKiB = databaseChunkKiB(*f.dbpath)
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

	tuning.engine.LowResource = eff.Low
	tuning.engine.CompressLevel = eff.CompressLevel

	selectedHashAlgo := cnst.BLAKE3
	switch parsed {
	case cliDef.commands.store.FullCommand():
		selectedHashAlgo = *f.storeHashAlgo
	case cliDef.commands.server.FullCommand():
		selectedHashAlgo = *f.serverHashAlgo
	}
	hashAlgo, err := cnst.ParseHashAlgo(selectedHashAlgo)
	if err != nil {
		return tuning, lifecycle.Usagef("--%s: %v", cnst.FlagHashAlgo, err)
	}
	tuning.layout = dbopen.Requested{
		ChunkKiB:  eff.ChunkKiB,
		Container: eff.Container,
		Quick:     eff.Quick,
		Set:       set,
	}
	if set[cnst.FlagHashAlgo] {
		tuning.layout.ChunkHash = hashAlgo
	}

	// The TUI's store takes these from the config file, as it has no flags
	// for them: unset, they are empty, which each parser reads as its
	// default.
	if parsed == cliDef.commands.store.FullCommand() || parsed == cliDef.commands.tui.FullCommand() {
		e := &tuning.engine
		if e.HashStrategy, err = cnst.ParseHashStrategy(*f.storeHashStrategy); err != nil {
			return tuning, lifecycle.Usagef("--%s%s: %v", cnst.FlagHashStrategy, src.originOf(cnst.FlagHashStrategy), err)
		}
		if e.Pipeline, err = cnst.ParseStorePipeline(*f.storePipeline); err != nil {
			return tuning, lifecycle.Usagef("--%s%s: %v", cnst.FlagStorePipeline, src.originOf(cnst.FlagStorePipeline), err)
		}
		if e.IOEngine, err = cnst.ParseStoreIOEngine(*f.storeIOEngine); err != nil {
			return tuning, lifecycle.Usagef("--%s%s: %v", cnst.FlagStoreIOEngine, src.originOf(cnst.FlagStoreIOEngine), err)
		}
		if err := cnst.CheckIOUringQueueDepth(*f.storeIOUringQueue); err != nil {
			return tuning, lifecycle.Usagef("--%s%s: %v", cnst.FlagStoreIOUringQueueDepth, src.originOf(cnst.FlagStoreIOUringQueueDepth), err)
		}
		e.IOUringDepth = *f.storeIOUringQueue
	}
	if parsed == cliDef.commands.store.FullCommand() {
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

		if set[cnst.FlagStoreAsyncFileWrite] {
			// Retired with STO-4: every chunk blob is now synced before its
			// metadata is written.
			tuning.issues = append(tuning.issues, appconfig.Issue{
				Flag:    cnst.FlagStoreAsyncFileWrite,
				Message: "is deprecated and has no effect; chunk blobs are always written durably",
			})
		}
	}

	// Resolve has already fitted these to the machine's memory; the pipelines
	// read them through the database's engine (cnst.Tuning.StorePipeline and
	// RestorePipeline).
	if eff.StoreWorkers > 0 {
		tuning.engine.StoreWorkers, tuning.engine.StoreQueue = eff.StoreWorkers, eff.StoreQueue
		tuning.storeWorkers, tuning.storeQueue = tuning.engine.StorePipeline(eff.Container)
	}
	if parsed == cliDef.commands.restore.FullCommand() && set[cnst.FlagRestoreProgressMs] {
		// Retired with the progress display, which samples on its own.
		tuning.issues = append(tuning.issues, appconfig.Issue{
			Flag:    cnst.FlagRestoreProgressMs,
			Message: "is retired and has no effect; the progress display updates on its own",
		})
	}
	if eff.RestoreWorkers > 0 {
		tuning.engine.RestoreWorkers, tuning.engine.RestoreQueue = eff.RestoreWorkers, eff.RestoreQueue
		tuning.engine.RestoreBufferMB = eff.RestoreBufferMB
		tuning.restoreWorkers, tuning.restoreQueue = tuning.engine.RestorePipeline()
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
	e := tuning.engine
	if e.LowResource {
		s = append(s, ui.Setting{Key: "mode", Value: "low-resource", Phrase: "low-resource"})
	} else {
		s = append(s, ui.Setting{Key: "mode", Value: "performance", Phrase: "performance"})
	}
	if tuning.layout.Quick {
		s = append(s, on("quick", "quick"))
	}
	if tuning.layout.Container {
		s = append(s, on("container", "container"))
	}
	if e.CompressLevel != "default" {
		s = append(s, ui.Setting{Key: "compress", Value: e.CompressLevel, Phrase: "zstd " + e.CompressLevel})
	}
	if parsed == cliDef.commands.store.FullCommand() {
		if *cliDef.flags.enableSimhash {
			s = append(s, on("simhash", "simhash"))
		}
		if *cliDef.flags.enableRevRel {
			s = append(s, on("revrel", "revrel"))
		}
	}
	n := func(key string, v int, phrase string) ui.Setting {
		return ui.Setting{Key: key, Value: strconv.Itoa(v), Phrase: fmt.Sprintf(phrase, v)}
	}
	switch parsed {
	case cliDef.commands.store.FullCommand():
		s = append(s,
			n("workers", tuning.storeWorkers, "%d workers"),
			n("queue", tuning.storeQueue, "queue %d"),
			ui.Setting{Key: "pipeline", Value: e.Pipeline, Phrase: "pipeline " + e.Pipeline},
			ui.Setting{Key: "io", Value: e.IOEngine, Phrase: e.IOEngine + " io"})
	case cliDef.commands.restore.FullCommand():
		s = append(s,
			n("workers", tuning.restoreWorkers, "%d workers"),
			n("queue", tuning.restoreQueue, "queue %d"),
			n("buffer_mib", e.RestoreWriteBuffer()/(1024*1024), "%d MiB buffer"))
	}
	return s
}
