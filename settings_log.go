package main

import (
	"indicer/internal/osdirs"
	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/logging"

	"go.uber.org/zap"
)

// logSettings writes the run's "settings" record: the config file read,
// and the presentation, log, temporary-file and database settings in
// effect, each with whether a flag, the config file or the default chose
// it. Paths are the ones in effect. Of the password only its source is
// logged, as "unlock": the log redacts any field that names a password.
func logSettings(f cliFlags, src sources, conf runConfig) {
	setting := func(name string, value any) zap.Field { return logging.Setting(name, value, src.from(name)) }
	path := func(name, dir string, err error) zap.Field {
		if err != nil {
			return logging.Setting(name, "", src.from(name), zap.Error(err))
		}
		return logging.Setting(name, dir, src.from(name))
	}

	temp, tempErr := osdirs.TempOr(*f.tempDir)
	root, rootErr := dbopen.ResolveRoot(*f.dbpath)
	unlock, unlockFrom := "none", logging.FromDefault
	switch {
	case *f.pwdFile != "":
		unlock, unlockFrom = flagPasswordFile, src.from(flagPasswordFile)
	case *f.pwdStdin:
		unlock, unlockFrom = flagPasswordStdin, logging.FromFlag
	}

	logging.GetLogger().Info("settings",
		conf.Setting(),
		setting(ui.FlagColor, *f.color),
		setting(ui.FlagPlain, *f.plain),
		setting(ui.FlagCI, *f.ci),
		setting(ui.FlagNoFun, *f.noFun),
		setting(ui.FlagTheme, *f.theme),
		setting(ui.FlagTaskbar, *f.taskbar),
		setting(flagDebug, *f.debug),
		setting(flagGraphCompact, *f.graphCompact),
		logging.DirSetting(flagLogDir, *f.logDir, src.from(flagLogDir)),
		path(flagTempDir, temp, tempErr),
		path(cnst.FlagDBPath, root, rootErr),
		logging.Setting("unlock", unlock, unlockFrom),
	)
}
