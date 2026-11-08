package configfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/lib/logging"

	"go.uber.org/zap/zapcore"
)

func TestChoiceRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good, bad := write("good.json", `{"name": "a"}`), write("bad.json", `{"name": 1}`)
	missing := filepath.Join(dir, "missing.json")
	at := func(p string) func() (string, error) { return func() (string, error) { return p, nil } }
	noHome := func() (string, error) { return "", errors.New("uid 1000 has none") }
	refuseB := func(f *File) error { return f.Errorf("name", "b is not allowed") }

	cases := []struct {
		name   string
		choice Choice
		def    func() (string, error)
		check  func(*File) error
		want   Source
		err    string // a substring of the error; "" for none
	}{
		{"default file", Choice{}, at(good), nil, Source{Path: good}, ""},
		{"no default file", Choice{}, at(missing), nil, Source{None: "no file at " + missing}, ""},
		{"no home", Choice{}, noHome, nil, Source{None: "no home directory (uid 1000 has none)"}, ""},
		{"--no-config", Choice{Off: true}, at(good), nil, Source{None: "--no-config", Chosen: true}, ""},
		{"--config", Choice{Path: good}, at(missing), nil, Source{Path: good, Chosen: true}, ""},
		{"--config with no home", Choice{Path: good}, noHome, nil, Source{Path: good, Chosen: true}, ""},
		{"--config missing", Choice{Path: missing}, at(good), nil, Source{}, "config file " + missing + ": "},
		{"both", Choice{Path: good, Off: true}, at(good), nil, Source{}, "--config and --no-config contradict each other; give one"},
		{"bad default", Choice{}, at(bad), nil, Source{}, `"name": must be a string` + "\n(the default config file; tool --no-config runs without it)"},
		{"bad --config", Choice{Path: bad}, at(good), nil, Source{}, `"name": must be a string`},
		{"refused value", Choice{Path: good}, at(good), refuseB, Source{}, `good.json:1:10: "name": b is not allowed`},
	}
	for _, c := range cases {
		var v testConfig
		got, err := c.choice.Read("tool", c.def, &v, c.check)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: error %v; want one holding %q", c.name, err, c.err)
		case got != c.want:
			t.Errorf("%s: %+v; want %+v", c.name, got, c.want)
		}
		if c.name == "bad --config" && strings.Contains(err.Error(), "--no-config runs without it") {
			t.Errorf("a file the command line named needs no hint: %v", err)
		}
		if got.Path != "" && (v.Name == nil || *v.Name != "a") {
			t.Errorf("%s: values %+v; want the file's", c.name, v)
		}
	}
}

func TestSourceSetting(t *testing.T) {
	t.Parallel()
	setting := func(s Source) map[string]any {
		enc := zapcore.NewMapObjectEncoder()
		s.Setting().AddTo(enc)
		m, _ := enc.Fields[FlagConfig].(map[string]any)
		return m
	}
	cases := []struct {
		src                   Source
		value, source, reason any
	}{
		{Source{Path: "/etc/a.json"}, "/etc/a.json", logging.FromDefault, nil},
		{Source{Path: "/etc/a.json", Chosen: true}, "/etc/a.json", logging.FromFlag, nil},
		{Source{None: "no file at /x"}, "none", logging.FromDefault, "no file at /x"},
		{Source{None: "--no-config", Chosen: true}, "none", logging.FromFlag, "--no-config"},
	}
	for _, c := range cases {
		m := setting(c.src)
		if m["value"] != c.value || m["source"] != c.source || m["reason"] != c.reason {
			t.Errorf("%+v: %v; want value %v, source %v, reason %v", c.src, m, c.value, c.source, c.reason)
		}
	}
}

func TestCheckPath(t *testing.T) {
	t.Parallel()
	abs, err := filepath.Abs("x")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parse("c.json", []byte(`{"name": "x", "store": {"mode": "`+strings.ReplaceAll(abs, `\`, `\\`)+`"}}`), &testConfig{})
	if err != nil {
		t.Fatal(err)
	}
	rel := "x"
	if err := f.CheckPath("name", &rel); err == nil ||
		err.Error() != `config file c.json:1:10: "name": "x" is not an absolute path; the config file takes absolute paths only` {
		t.Errorf("relative path: %v", err)
	}
	if err := f.CheckPath("store.mode", &abs); err != nil {
		t.Errorf("absolute path: %v", err)
	}
	if err := f.CheckPath("name", nil); err != nil {
		t.Errorf("a key not in the file: %v", err)
	}
}
