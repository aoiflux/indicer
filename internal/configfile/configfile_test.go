package configfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type testConfig struct {
	Name  *string      `json:"name"`
	Plain *bool        `json:"plain"`
	Count *int         `json:"count"`
	List  *[]string    `json:"list"`
	Store *testSection `json:"store"`
}

type testSection struct {
	Workers *int    `json:"workers"`
	Mode    *string `json:"mode"`
}

func TestParseAccepts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in string
		want     testConfig
	}{
		{"empty object", `{}`, testConfig{}},
		{"every kind", `{"name": "a", "plain": true, "count": 8, "list": ["x", "y"], "store": {"workers": 2, "mode": "staged"}}`,
			testConfig{Name: new("a"), Plain: new(true), Count: new(8), List: new([]string{"x", "y"}),
				Store: &testSection{Workers: new(2), Mode: new("staged")}}},
		{"false and zero are values", `{"plain": false, "count": 0, "list": []}`,
			testConfig{Plain: new(false), Count: new(0), List: new([]string{})}},
		{"empty section", `{"store": {}}`, testConfig{Store: &testSection{}}},
		{"escaped key", "{\"n\\" + "u0061me\": \"a\"}", testConfig{Name: new("a")}},
		{"byte order mark", "\xEF\xBB\xBF{\"name\": \"a\"}", testConfig{Name: new("a")}},
		{"whitespace around", "\r\n  {\"name\": \"a\"}\r\n\r\n", testConfig{Name: new("a")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got testConfig
			if _, err := parse("c.json", []byte(tc.in), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in  string
		line, col int
		key, msg  string
	}{
		{"empty file", ``, 1, 1, "", "is empty"},
		{"only space", "  \n ", 2, 2, "", "is empty"},
		{"not an object", `["a"]`, 1, 1, "", "must hold one JSON object"},
		{"a string", `"a"`, 1, 1, "", "must hold one JSON object"},
		{"two objects", `{"name": "a"}{}`, 1, 14, "", "has more after the JSON object"},
		{"trailing text", `{"name": "a"} x`, 1, 15, "", "has more after the JSON object"},
		{"wrong case", `{"Name": "a"}`, 1, 2, "Name", `did you mean "name"?`},
		{"duplicate", `{"name": "a", "name": "b"}`, 1, 15, "name", "appears more than once"},
		{"unknown", `{"nope": 1}`, 1, 2, "nope", "unknown key"},
		{"belongs in a section", `{"workers": 1}`, 1, 2, "workers", `goes in the "store" section`},
		{"belongs at the top", `{"store": {"plain": true}}`, 1, 12, "store.plain", "it goes at the top level"},
		{"unknown in a section", `{"store": {"nope": 1}}`, 1, 12, "store.nope", "unknown key"},
		{"section not an object", `{"store": 5}`, 1, 11, "store", "must be an object"},
		{"section a list", `{"store": []}`, 1, 11, "store", "must be an object"},
		{"null", `{"name": null}`, 1, 10, "name", "null is not a value"},
		{"null in a section", `{"store": {"mode": null}}`, 1, 20, "store.mode", "null is not a value"},
		{"string for a number", `{"count": "8"}`, 1, 11, "count", "must be a whole number"},
		{"fraction", `{"count": 8.5}`, 1, 11, "count", "must be a whole number"},
		{"exponent", `{"count": 1e3}`, 1, 11, "count", "must be a whole number"},
		{"overflow", `{"count": 99999999999999999999}`, 1, 11, "count", "must be a whole number"},
		{"string for a list", `{"list": "a"}`, 1, 10, "list", "must be a list of strings"},
		{"number in a list", `{"list": ["a", 1]}`, 1, 10, "list", "must be a list of strings"},
		{"string for a bool", `{"plain": "yes"}`, 1, 11, "plain", "must be true or false"},
		{"object for a string", `{"name": {}}`, 1, 10, "name", "must be a string"},
		{"syntax on line 3", "{\n  \"name\": \"a\",\n  \"count\": x\n}", 3, 12, "", "invalid character 'x'"},
		{"trailing comma", `{"name": "a",}`, 1, 13, "", "has a comma before the closing bracket"},
		{"trailing comma in a list", `{"list": ["a", ]}`, 1, 14, "", "has a comma before the closing bracket"},
		{"comment", "{\n  // no comments\n}", 2, 3, "", "invalid character '/'"},
		{"unclosed", `{"name": "a"`, 1, 13, "", "ends before the JSON object does"},
		{"unclosed section", `{"store": {"mode": "a"`, 1, 23, "", "ends before the JSON object does"},
		{"invalid UTF-8", "{\"name\": \"\xff\"}", 1, 11, "", "is not valid UTF-8"},
		{"column counts characters", `{"name": "é", "Count": 1}`, 1, 15, "Count", "did you mean"},
		{"byte order mark is not a column", "\xEF\xBB\xBF{\"nope\": 1}", 1, 2, "nope", "unknown key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got testConfig
			_, err := parse("c.json", []byte(tc.in), &got)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("got %v, want an *Error", err)
			}
			if e.Line != tc.line || e.Col != tc.col || e.Key != tc.key || !strings.Contains(e.Msg, tc.msg) {
				t.Fatalf("got %d:%d key %q %q, want %d:%d key %q containing %q", e.Line, e.Col, e.Key, e.Msg, tc.line, tc.col, tc.key, tc.msg)
			}
		})
	}
}

func TestErrorText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		e    Error
		want string
	}{
		{Error{Path: "/c.json", Line: 2, Col: 5, Key: "theme", Msg: "bad"}, `config file /c.json:2:5: "theme": bad`},
		{Error{Path: "/c.json", Line: 1, Col: 1, Msg: "is empty"}, `config file /c.json:1:1: is empty`},
		{Error{Path: "/c.json", Msg: "is a directory"}, `config file /c.json: is a directory`},
	} {
		if got := tc.e.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestErrorfPlacesTheValue(t *testing.T) {
	t.Parallel()
	var c testConfig
	f, err := parse("c.json", []byte("{\n  \"store\": {\n    \"mode\": \"fast\"\n  }\n}"), &c)
	if err != nil {
		t.Fatal(err)
	}
	e := f.Errorf("store.mode", "unknown mode %q", *c.Store.Mode)
	if got, want := e.Error(), `config file c.json:3:13: "store.mode": unknown mode "fast"`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if e := f.Errorf("name", "x"); e.Line != 0 {
		t.Fatalf("a key the file does not set has no place, got %d:%d", e.Line, e.Col)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	var c testConfig
	f, err := Load(write("good.json", `{"name": "a"}`), &c)
	if err != nil || *c.Name != "a" || f.Path != filepath.Join(dir, "good.json") {
		t.Fatalf("good file: %v %+v", err, c)
	}

	missing := filepath.Join(dir, "missing.json")
	_, err = Load(missing, &c)
	if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "config file "+missing+": ") {
		t.Fatalf("missing file: %v", err)
	}
	if strings.Count(err.Error(), missing) != 1 {
		t.Fatalf("the path is named once: %v", err)
	}

	if _, err := Load(dir, &c); err == nil || !strings.HasSuffix(err.Error(), "is a directory, not a file") {
		t.Fatalf("directory: %v", err)
	}

	full := write("full.json", "{}"+strings.Repeat(" ", MaxSize-2))
	if _, err := Load(full, &c); err != nil {
		t.Fatalf("a file of exactly MaxSize: %v", err)
	}
	big := write("big.json", "{}"+strings.Repeat(" ", MaxSize-1))
	if _, err := Load(big, &c); err == nil || !strings.HasSuffix(err.Error(), "is larger than 64 KiB") {
		t.Fatalf("a file over MaxSize: %v", err)
	}

	if _, err := Load(write("utf16.json", "\xff\xfe{\x00}\x00"), &c); err == nil || !strings.HasSuffix(err.Error(), "is UTF-16 text; save it as UTF-8") {
		t.Fatalf("UTF-16: %v", err)
	}

	if _, err := Load(write("bad.json", `{"count": "x"}`), &c); err == nil ||
		err.Error() != `config file `+filepath.Join(dir, "bad.json")+`:1:11: "count": must be a whole number` {
		t.Fatalf("bad value: %v", err)
	}
}

// ReadFile reads by Load's rules, and its errors name the path but never
// what the file holds.
func TestReadFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := strings.Repeat("s", 40)
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(good); err != nil || string(b) != secret {
		t.Fatalf("good file: %q %v", b, err)
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(secret+strings.Repeat(" ", MaxSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	utf16 := filepath.Join(dir, "utf16")
	if err := os.WriteFile(utf16, []byte("\xff\xfes\x00s\x00\r\x00\n\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{big, big + ": is larger than 64 KiB"},
		{dir, dir + ": is a directory, not a file"},
		{utf16, utf16 + ": is UTF-16 text; save it as UTF-8"},
	} {
		_, err := ReadFile(c.path)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.path, err, c.want)
		}
	}
	if _, err := ReadFile(filepath.Join(dir, "missing")); err == nil || strings.Contains(err.Error(), "config file") {
		t.Errorf("missing file: %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	homeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { homeDir = defaultHomeDir })
	p, err := DefaultPath("dues")
	if want := filepath.Join(home, ".config", "dues", "dues.json"); err != nil || p != want {
		t.Fatalf("got %q, %v; want %q", p, err, want)
	}

	homeDir = func() (string, error) { return "", errors.New("no home") }
	if _, err := DefaultPath("dues"); err == nil || err.Error() != "no home" {
		t.Fatalf("got %v, want the home directory's error", err)
	}
}

var defaultHomeDir = homeDir

func TestBadSchemasPanic(t *testing.T) {
	t.Parallel()
	for name, into := range map[string]any{
		"not a pointer": testConfig{},
		"not a struct":  new("x"),
		"value field": &struct {
			A string `json:"a"`
		}{},
		"nested section": &struct {
			S *struct {
				T *struct{} `json:"t"`
			} `json:"s"`
		}{},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			parse("c.json", []byte(`{"s": {"t": {}}}`), into)
		})
	}
}
