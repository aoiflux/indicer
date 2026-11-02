package main

// dues config: the settings in effect and where each came from.

import (
	"encoding/json"
	"reflect"
	"strconv"

	"indicer/internal/console"
	"indicer/internal/osdirs"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/logging"

	"github.com/alecthomas/kingpin/v2"
)

const cmdConfig = "config"

// configDoc is what dues config prints.
type configDoc struct {
	// File is the config file read, null when none was; Reason says why.
	File     *string        `json:"file"`
	Reason   string         `json:"reason,omitempty"`
	Settings map[string]any `json:"settings"`
}

// settingDoc is one setting: its value and whether a flag, the config
// file or the default chose it.
type settingDoc struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
	Error  string `json:"error,omitempty"`
}

// printConfig prints every key the config file takes, with the value in
// effect and its source. A command's own settings show what that command
// would run with, since its flags cannot be given here. Paths are the
// ones in effect. It reads neither the password file nor the token file.
func printConfig(app *kingpin.Application, f cliFlags, src sources, conf runConfig) error {
	doc := configDoc{Reason: conf.none, Settings: map[string]any{}}
	if conf.path != "" {
		doc.File = &conf.path
	}
	for _, b := range f.bindings() {
		s := settingDoc{Value: reflect.ValueOf(b.dst).Elem().Interface(), Source: src.from(b.flag)}
		if b.section == "" {
			switch b.flag {
			case flagLogDir:
				s.Value = logging.Current().Dir
			case flagTempDir:
				s.Value, s.Error = inEffect(osdirs.TempOr(*f.tempDir))
			case cnst.FlagDBPath:
				s.Value, s.Error = inEffect(dbopen.ResolveRoot(*f.dbpath))
			}
			doc.Settings[b.flag] = s
			continue
		}
		if v, ok := conf.values.lookup(b.section, b.flag); ok {
			s = settingDoc{Value: v.Elem().Interface(), Source: logging.FromConfig}
		} else {
			s = settingDoc{Value: flagDefault(app.GetCommand(b.section).GetFlag(b.flag), reflect.TypeOf(b.dst).Elem()), Source: logging.FromDefault}
		}
		section, _ := doc.Settings[b.section].(map[string]settingDoc)
		if section == nil {
			section = map[string]settingDoc{}
			doc.Settings[b.section] = section
		}
		section[b.flag] = s
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	console.Println(string(out))
	return nil
}

func inEffect(dir string, err error) (string, string) {
	if err != nil {
		return "", err.Error()
	}
	return dir, ""
}

// flagDefault is a flag's default as a value of type t. kingpin applies
// defaults only to the flags of the command that runs.
func flagDefault(flag *kingpin.FlagClause, t reflect.Type) any {
	d := flag.Model().Default
	first := ""
	if len(d) > 0 {
		first = d[0]
	}
	switch t.Kind() {
	case reflect.Int:
		n, _ := strconv.Atoi(first)
		return n
	case reflect.Bool:
		b, _ := strconv.ParseBool(first)
		return b
	case reflect.Slice:
		return append([]string{}, d...)
	}
	return first
}
