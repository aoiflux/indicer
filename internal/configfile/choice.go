package configfile

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"indicer/lib/logging"

	"go.uber.org/zap"
)

// The flags that choose the config file, in every binary.
const (
	FlagConfig   = "config"
	FlagNoConfig = "no-config"
)

// Choice is the config file a command line asks for.
type Choice struct {
	Path string // named with --config; "" when not given
	Off  bool   // --no-config
}

// Source is the config file a run read, or why it read none.
type Source struct {
	Path   string // the file read; "" when none was
	None   string // why none was read, when none was
	Chosen bool   // the command line chose it, with --config or --no-config
}

// Read loads the file c chooses into into and checks its values with check
// (nil for none): the file --config names, which must exist; none under
// --no-config; else the file defaultPath gives, if one is there. A missing
// default file, or no home directory to look in, means no config file. An
// error in the default file says that "command --no-config" runs without
// it. On an error, into may hold part of the file; do not use it.
func (c Choice) Read(command string, defaultPath func() (string, error), into any, check func(*File) error) (Source, error) {
	switch {
	case c.Path != "" && c.Off:
		return Source{}, fmt.Errorf("--%s and --%s contradict each other; give one", FlagConfig, FlagNoConfig)
	case c.Off:
		return Source{None: "--" + FlagNoConfig, Chosen: true}, nil
	}
	path, chosen := c.Path, c.Path != ""
	if !chosen {
		p, err := defaultPath()
		if err != nil {
			return Source{None: fmt.Sprintf("no home directory (%v)", err)}, nil
		}
		path = p
	}
	f, err := Load(path, into)
	if err == nil && check != nil {
		err = check(f)
	}
	switch {
	case err == nil:
		return Source{Path: path, Chosen: chosen}, nil
	case !chosen && errors.Is(err, fs.ErrNotExist):
		return Source{None: "no file at " + path}, nil
	case !chosen:
		err = fmt.Errorf("%w\n(the default config file; %s --%s runs without it)", err, command, FlagNoConfig)
	}
	return Source{}, err
}

// Setting is the settings record's entry for the config file: its path, or
// "none" with the reason.
func (s Source) Setting() zap.Field {
	from := logging.FromDefault
	if s.Chosen {
		from = logging.FromFlag
	}
	if s.Path == "" {
		return logging.Setting(FlagConfig, "none", from, zap.String("reason", s.None))
	}
	return logging.Setting(FlagConfig, s.Path, from)
}

// CheckPath refuses a path key that is not absolute: the file applies in
// whatever directory a command runs, so a relative path would point
// somewhere different each time. v is nil for a key not in the file.
func (f *File) CheckPath(key string, v *string) error {
	if v == nil || filepath.IsAbs(*v) {
		return nil
	}
	return f.Errorf(key, "%q is not an absolute path; the config file takes absolute paths only", *v)
}
