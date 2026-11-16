package main

// The config file: the settings an operator keeps between runs, read from
// <home>/.config/dues/dues.json or the file --config names. Flags override
// it; nothing comes from the environment.

import (
	"errors"
	"reflect"
	"slices"
	"strings"

	"indicer/internal/appconfig"
	"indicer/internal/configfile"
	"indicer/internal/lifecycle"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/ingest"
	"indicer/lib/logging"
	"indicer/lib/server"
)

const (
	flagConfig   = configfile.FlagConfig
	flagNoConfig = configfile.FlagNoConfig
)

// fileConfig is dues.json. Its keys are the long names of the flags they
// stand for; a command's own settings sit in an object named after the
// command, and apply to that command only. A nil field is a key the file
// does not set.
//
// Only preferences belong here. The database layout (chonksize, container,
// hash-algo, quick) is fixed when a database is created, so a file that
// set it would make every existing database a usage error. One-run choices
// (fix, force, out, ...) and the password itself are flags only.
type fileConfig struct {
	Color   *string `json:"color"`
	Plain   *bool   `json:"plain"`
	CI      *bool   `json:"ci"`
	NoFun   *bool   `json:"no-fun"`
	Theme   *string `json:"theme"`
	Taskbar *bool   `json:"taskbar"`

	LogDir  *string `json:"log-dir"`
	TempDir *string `json:"temp-dir"`
	Debug   *bool   `json:"debug"`

	GraphCompact *string `json:"graph-compact"`

	DBPath       *string `json:"dbpath"`
	PasswordFile *string `json:"password-file"`

	Actor                *string `json:"actor"`
	ActorKeyPasswordFile *string `json:"actor-key-password-file"`

	Preset        *string `json:"preset"`
	CompressLevel *string `json:"compress-level"`
	Low           *bool   `json:"low"`

	Store   *fileStore   `json:"store"`
	Restore *fileRestore `json:"restore"`
	Server  *fileServer  `json:"server"`
}

type fileStore struct {
	Pipeline          *string `json:"pipeline"`
	HashStrategy      *string `json:"hash-strategy"`
	IOEngine          *string `json:"io-engine"`
	IOUringQueueDepth *int    `json:"io-uring-queue-depth"`
	Workers           *int    `json:"store-workers"`
	Queue             *int    `json:"store-queue"`
	Jobs              *int    `json:"store-jobs"`
}

type fileRestore struct {
	Workers  *int `json:"restore-workers"`
	Queue    *int `json:"restore-queue"`
	BufferMB *int `json:"restore-buffer-mb"`
}

type fileServer struct {
	Listen             *[]string `json:"listen"`
	TokenFile          *string   `json:"token-file"`
	CORSOrigin         *[]string `json:"cors-origin"`
	UploadJobs         *int      `json:"upload-jobs"`
	ExportDir          *string   `json:"export-dir"`
	ExportKeepHours    *int      `json:"export-keep-hours"`
	ExportDrainTimeout *string   `json:"export-drain-timeout"`
}

// validate checks every value the file sets with the rules its flag
// follows, and reports them all at once, each at its place in the file.
// Checks that depend on the machine (whether temp-dir exists, whether the
// workers fit in memory) run later, as they do for flags.
func (c *fileConfig) validate(f *configfile.File) error {
	var errs []error
	bad := func(key, format string, args ...any) { errs = append(errs, f.Errorf(key, format, args...)) }
	oneOf := func(key string, v *string, values []string) {
		if v != nil && !slices.Contains(values, *v) {
			bad(key, "unknown value %q; use %s", *v, strings.Join(values, ", "))
		}
	}
	named := func(key string, v *string, normalize func(string) string, values string) {
		if v != nil && (strings.TrimSpace(*v) == "" || normalize(*v) == "") {
			bad(key, "unknown value %q; use %s", *v, values)
		}
	}
	path := func(key string, v *string) {
		if err := f.CheckPath(key, v); err != nil {
			errs = append(errs, err)
		}
	}
	count := func(key string, v *int) {
		if v != nil && *v < 0 {
			bad(key, "must be 0 (automatic) or more, got %d", *v)
		}
	}
	jobs := func(key string, v *int) {
		if v != nil && (*v < 0 || *v > ingest.MaxJobs) {
			bad(key, "must be from 0 (automatic) to %d, got %d", ingest.MaxJobs, *v)
		}
	}

	oneOf(ui.FlagColor, c.Color, ui.ColorPolicies)
	oneOf(ui.FlagTheme, c.Theme, ui.ThemeNames)
	path(flagLogDir, c.LogDir)
	path(flagTempDir, c.TempDir)
	oneOf(flagGraphCompact, c.GraphCompact, []string{graphCompactAuto, graphCompactNever})
	path(cnst.FlagDBPath, c.DBPath)
	path(flagPasswordFile, c.PasswordFile)
	if c.Actor != nil {
		if err := checkActorFlag(*c.Actor); err != nil {
			bad(flagActor, "%v", err)
		}
	}
	path(flagActorKeyPasswordFile, c.ActorKeyPasswordFile)
	named(cnst.FlagPreset, c.Preset, appconfig.NormalizePreset, "quick, performance or low-resource")
	named(cnst.FlagCompressLevel, c.CompressLevel, appconfig.NormalizeCompressLevel, "fast, default or best")
	if s := c.Store; s != nil {
		named("store."+cnst.FlagStorePipeline, s.Pipeline, cnst.NormalizeStorePipeline, "batch-owner or staged")
		named("store."+cnst.FlagHashStrategy, s.HashStrategy, cnst.NormalizeHashStrategy, "sync, async or hocho")
		named("store."+cnst.FlagStoreIOEngine, s.IOEngine, cnst.NormalizeStoreIOEngine, "auto, stdlib or io-uring")
		count("store."+cnst.FlagStoreIOUringQueueDepth, s.IOUringQueueDepth)
		count("store."+cnst.FlagStoreWorkers, s.Workers)
		count("store."+cnst.FlagStoreQueue, s.Queue)
		jobs("store."+cnst.FlagStoreJobs, s.Jobs)
	}
	if r := c.Restore; r != nil {
		count("restore."+cnst.FlagRestoreWorkers, r.Workers)
		count("restore."+cnst.FlagRestoreQueue, r.Queue)
		count("restore."+cnst.FlagRestoreBufferMB, r.BufferMB)
	}
	if s := c.Server; s != nil {
		if s.Listen != nil {
			if err := server.CheckListen(*s.Listen); err != nil {
				bad("server."+flagListen, "%v", err)
			}
		}
		path("server."+flagTokenFile, s.TokenFile)
		if s.CORSOrigin != nil {
			for _, o := range *s.CORSOrigin {
				if err := server.CheckOrigin(o); err != nil {
					bad("server."+flagCORSOrigin, "%v", err)
					break
				}
			}
		}
		jobs("server."+flagUploadJobs, s.UploadJobs)
		path("server."+flagExportDir, s.ExportDir)
		if s.ExportKeepHours != nil {
			if err := server.CheckExportKeepHours(*s.ExportKeepHours); err != nil {
				bad("server."+flagExportKeepHours, "%v", err)
			}
		}
		if s.ExportDrainTimeout != nil {
			if _, err := server.ParseDrainTimeout(*s.ExportDrainTimeout); err != nil {
				bad("server."+flagExportDrainTimeout, "%v", err)
			}
		}
	}
	return errors.Join(errs...)
}

// presentation is the file's presentation settings, the base the
// command line's change.
func (c *fileConfig) presentation() ui.Options {
	var o ui.Options
	if c.Color != nil {
		o.Color = ui.ColorPolicy(*c.Color)
	}
	if c.Theme != nil {
		o.Theme = ui.ParseTheme(*c.Theme)
	}
	for _, b := range []struct {
		dst *bool
		src *bool
	}{{&o.Plain, c.Plain}, {&o.CI, c.CI}, {&o.NoFun, c.NoFun}, {&o.Taskbar, c.Taskbar}} {
		if b.src != nil {
			*b.dst = *b.src
		}
	}
	return o
}

// runConfig is the config file a run uses.
type runConfig struct {
	configfile.Source
	values fileConfig
}

// defaultConfigPath is where dues looks without --config; tests point it
// at a file of their own.
var defaultConfigPath = func() (string, error) { return configfile.DefaultPath("dues") }

// readConfig reads the config file the command line chooses: the file
// --config names, none under --no-config, else the default file if there
// is one. It also returns the arguments without those two flags, for the
// help that prints before kingpin parses. A file that cannot be used is a
// usage error.
func readConfig(args []string) (runConfig, []string, error) {
	path, _, off, rest, err := configArgs(args)
	if err != nil {
		return runConfig{}, rest, err
	}
	var c runConfig
	c.Source, err = configfile.Choice{Path: path, Off: off}.Read("dues", defaultConfigPath, &c.values, c.values.validate)
	if err != nil {
		// The values may hold part of the file; the help printed with the
		// error must not use them.
		return runConfig{}, rest, &lifecycle.UsageError{Err: err}
	}
	return c, rest, nil
}

// configArgs finds --config FILE (or --config=FILE) and --no-config in the
// arguments, before kingpin parses them, and returns the arguments without
// them.
func configArgs(args []string) (path string, given, off bool, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		v, isConfig := strings.CutPrefix(a, "--"+flagConfig+"=")
		switch {
		case a == "--"+flagNoConfig:
			off = true
			continue
		case a == "--"+flagConfig:
			if i+1 == len(args) {
				return "", false, false, rest, lifecycle.Usagef("--%s needs a file", flagConfig)
			}
			i++
			v, isConfig = args[i], true
		}
		if !isConfig {
			rest = append(rest, a)
			continue
		}
		if given {
			return "", false, false, rest, lifecycle.Usagef("--%s is given twice", flagConfig)
		}
		if v == "" {
			return "", false, false, rest, lifecycle.Usagef("--%s needs a file", flagConfig)
		}
		path, given = v, true
	}
	return path, given, off, rest, nil
}

// sources says where each flag's value came from.
type sources struct {
	set    map[string]bool // given on the command line
	filled map[string]bool // taken from the config file
	file   string          // the config file's path
}

func (s sources) from(name string) string {
	switch {
	case s.set[name]:
		return logging.FromFlag
	case s.filled[name]:
		return logging.FromConfig
	}
	return logging.FromDefault
}

// explicit is every flag that has a value of its own, from the command
// line or the file: a preset does not change those.
func (s sources) explicit() map[string]bool {
	m := make(map[string]bool, len(s.set)+len(s.filled))
	for k := range s.set {
		m[k] = true
	}
	for k := range s.filled {
		m[k] = true
	}
	return m
}

// origins names the file for the flags it filled, for messages.
func (s sources) origins() map[string]string {
	m := make(map[string]string, len(s.filled))
	for k := range s.filled {
		m[k] = "config file " + s.file
	}
	return m
}

// originOf is the text messages about flag name add: where its value came
// from when the command line did not give it.
func (s sources) originOf(name string) string {
	if s.filled[name] {
		return " (set in config file " + s.file + ")"
	}
	return ""
}

// binding ties a config file key to the flag it stands for.
type binding struct {
	section string // the command whose object holds the key; "" for the top level
	flag    string
	dst     any // the value kingpin parsed the flag into: *string, *bool, *int or *[]string
}

// bindings lists every key of the config file with its flag.
func (f cliFlags) bindings() []binding {
	return []binding{
		{"", ui.FlagColor, f.color},
		{"", ui.FlagPlain, f.plain},
		{"", ui.FlagCI, f.ci},
		{"", ui.FlagNoFun, f.noFun},
		{"", ui.FlagTheme, f.theme},
		{"", ui.FlagTaskbar, f.taskbar},
		{"", flagLogDir, f.logDir},
		{"", flagTempDir, f.tempDir},
		{"", flagDebug, f.debug},
		{"", flagGraphCompact, f.graphCompact},
		{"", cnst.FlagDBPath, f.dbpath},
		{"", flagPasswordFile, f.pwdFile},
		{"", flagActor, f.actor},
		{"", flagActorKeyPasswordFile, f.actorPwdFile},
		{"", cnst.FlagPreset, f.preset},
		{"", cnst.FlagCompressLevel, f.compressLevel},
		{"", cnst.FlagLowResource, f.memopt},
		{cnst.CmdStore, cnst.FlagStorePipeline, f.storePipeline},
		{cnst.CmdStore, cnst.FlagHashStrategy, f.storeHashStrategy},
		{cnst.CmdStore, cnst.FlagStoreIOEngine, f.storeIOEngine},
		{cnst.CmdStore, cnst.FlagStoreIOUringQueueDepth, f.storeIOUringQueue},
		{cnst.CmdStore, cnst.FlagStoreWorkers, f.storeWorkers},
		{cnst.CmdStore, cnst.FlagStoreQueue, f.storeQueue},
		{cnst.CmdStore, cnst.FlagStoreJobs, f.storeJobs},
		{cnst.CmdRestore, cnst.FlagRestoreWorkers, f.restoreWorkers},
		{cnst.CmdRestore, cnst.FlagRestoreQueue, f.restoreQueue},
		{cnst.CmdRestore, cnst.FlagRestoreBufferMB, f.restoreBufferMB},
		{cnst.CmdServer, flagListen, f.serverListen},
		{cnst.CmdServer, flagTokenFile, f.serverTokenFile},
		{cnst.CmdServer, flagCORSOrigin, f.serverCORSOrigins},
		{cnst.CmdServer, flagUploadJobs, f.serverUploadJobs},
		{cnst.CmdServer, flagExportDir, f.serverExportDir},
		{cnst.CmdServer, flagExportKeepHours, f.serverExportKeep},
		{cnst.CmdServer, flagExportDrainTimeout, f.serverExportDrain},
	}
}

// lookup is the file's value for a key, a pointer to it, and whether the
// file sets the key.
func (c *fileConfig) lookup(section, key string) (reflect.Value, bool) {
	v := reflect.ValueOf(c).Elem()
	if section != "" {
		if v = field(v, section); v.IsNil() {
			return reflect.Value{}, false
		}
		v = v.Elem()
	}
	v = field(v, key)
	return v, !v.IsNil()
}

// field is the field of struct v tagged with key.
func field(v reflect.Value, key string) reflect.Value {
	t := v.Type()
	for i := range t.NumField() {
		if t.Field(i).Tag.Get("json") == key {
			return v.Field(i)
		}
	}
	panic("config file key " + key + " has no field in " + t.String())
}

// sectionApplies reports whether the file's section (a command's name, or
// "" for the global keys) applies to the command that runs: a section's
// flags exist only on its command. The TUI stores and restores in process,
// so the store and restore sections tune it too.
func sectionApplies(section, command string) bool {
	return section == "" || section == command ||
		command == cnst.CmdTui && (section == cnst.CmdStore || section == cnst.CmdRestore)
}

// apply fills the flags the command line left unset with the file's
// values, and returns where each value came from (see sectionApplies).
func (c runConfig) apply(f cliFlags, parsed string, set map[string]bool) sources {
	s := sources{set: set, filled: map[string]bool{}, file: c.Path}
	// A preset on the command line asks for its own settings, so the file's
	// preset and the settings it tuned with it give way.
	presetGiven := set[cnst.FlagPreset] || set[cnst.FlagQuickMode] || set[cnst.FlagPerformanceMode] || set[cnst.FlagLowResourceMode]
	for _, b := range f.bindings() {
		switch {
		case !sectionApplies(b.section, parsed), set[b.flag]:
			continue
		case b.flag == cnst.FlagPreset, b.flag == cnst.FlagCompressLevel, b.flag == cnst.FlagLowResource:
			if presetGiven {
				continue
			}
		case b.flag == flagPasswordFile:
			// The command line's way of unlocking replaces the file's.
			if *f.pwdStdin {
				continue
			}
		case b.flag == flagActorKeyPasswordFile:
			if *f.actorPwdStdin {
				continue
			}
		}
		if v, ok := c.values.lookup(b.section, b.flag); ok {
			reflect.ValueOf(b.dst).Elem().Set(v.Elem())
			s.filled[b.flag] = true
		}
	}
	return s
}
