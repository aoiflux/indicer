// Package configfile reads the configuration files of the DUES binaries:
// one JSON object of settings keyed by flag name, with an object per
// command for that command's settings.
//
// It is stricter than encoding/json, which matches keys in any case, keeps
// the last of two equal keys and reports no position for an unknown one.
// Here a key matches exactly and appears once, null is not a value,
// nothing may follow the object, and every error names the file and,
// where there is one, the line and column.
package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"indicer/internal/osdirs"
)

// MaxSize is the largest configuration file read. A settings file is a
// few hundred bytes; anything near this is not one.
const MaxSize = 64 << 10

// homeDir is osdirs.Home; tests replace it.
var homeDir = osdirs.Home

// DefaultPath is <home>/.config/dues/<product>.json, on every operating
// system.
func DefaultPath(product string) (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "dues", product+".json"), nil
}

// Error is a configuration file that could not be used.
type Error struct {
	Path string
	// Line and Col locate the error, counting from 1; 0 when it has no
	// place in the text.
	Line, Col int
	// Key is the key the error is about, "section.key" inside a section.
	Key string
	Msg string
	// Err is the underlying error, if any: fs.ErrNotExist for a missing
	// file.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("config file ")
	b.WriteString(e.Path)
	if e.Line > 0 {
		fmt.Fprintf(&b, ":%d:%d", e.Line, e.Col)
	}
	b.WriteString(": ")
	if e.Key != "" {
		fmt.Fprintf(&b, "%q: ", e.Key)
	}
	b.WriteString(e.Msg)
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// File is a configuration file that was read.
type File struct {
	Path string
	data []byte
	bom  int // the length of a UTF-8 byte order mark at the start, or 0
	// at holds the offset of each key's value, by "key" or "section.key".
	at map[string]int
}

// Errorf returns an error about key, placed at its value when the file
// sets it.
func (f *File) Errorf(key, format string, args ...any) *Error {
	e := &Error{Path: f.Path, Key: key, Msg: fmt.Sprintf(format, args...)}
	if off, ok := f.at[key]; ok {
		e.Line, e.Col = f.position(off)
	}
	return e
}

// Load reads the configuration file at path into into, which points to a
// struct whose fields are pointers tagged with their keys: a field stays
// nil when the file does not set its key. A field that points to a struct
// is a section, the object of that key. A missing file is an *Error that
// wraps fs.ErrNotExist.
func Load(path string, into any) (*File, error) {
	fail := func(msg string, err error) (*File, error) {
		return nil, &Error{Path: path, Msg: msg, Err: err}
	}
	osErr := func(err error) (*File, error) {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			return fail(pe.Err.Error(), err)
		}
		return fail(err.Error(), err)
	}
	f, err := open(path)
	if err != nil {
		return osErr(err)
	}
	defer f.Close()
	// The checks look at the file that was opened, not at the path, which
	// another process could point elsewhere in between.
	info, err := f.Stat()
	if err != nil {
		return osErr(err)
	}
	switch {
	case info.IsDir():
		return fail("is a directory, not a file", nil)
	case !info.Mode().IsRegular():
		return fail("is not a regular file", nil)
	}
	if msg := checkAccess(info); msg != "" {
		return fail(msg, nil)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return osErr(err)
	}
	if len(data) > MaxSize {
		return fail(fmt.Sprintf("is larger than %d KiB", MaxSize>>10), nil)
	}
	return parse(path, data, into)
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// jsonSpace is the white space JSON allows between tokens.
const jsonSpace = " \t\r\n"

// parse decodes data, the contents of the file at path, into into.
func parse(path string, data []byte, into any) (*File, error) {
	v := reflect.ValueOf(into)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		panic(fmt.Sprintf("configfile: Load needs a pointer to a struct, not %T", into))
	}
	f := &File{Path: path, data: data, at: map[string]int{}}
	if bytes.HasPrefix(data, []byte{0xFF, 0xFE}) || bytes.HasPrefix(data, []byte{0xFE, 0xFF}) {
		return nil, &Error{Path: path, Msg: "is UTF-16 text; save it as UTF-8"}
	}
	// Windows PowerShell 5.1 writes a byte order mark; JSON may ignore it.
	if bytes.HasPrefix(data, utf8BOM) {
		f.bom = len(utf8BOM)
	}
	if off := invalidUTF8(data); off >= 0 {
		// encoding/json would quietly replace the bytes, changing a path.
		return nil, f.errAt(off, "", "is not valid UTF-8 text")
	}
	w := walker{f: f, body: data[f.bom:], root: v.Elem().Type()}
	w.dec = json.NewDecoder(bytes.NewReader(w.body))
	if err := w.object(v.Elem(), ""); err != nil {
		return nil, err
	}
	at := w.next()
	if _, err := w.dec.Token(); !errors.Is(err, io.EOF) {
		return nil, f.errAt(at, "", "has more after the JSON object; it must hold one object")
	}
	return f, nil
}

func invalidUTF8(b []byte) int {
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n == 1 {
			return i
		}
		i += n
	}
	return -1
}

// position turns an offset in the file into a line and a column, both
// counted from 1; the column counts characters.
func (f *File) position(off int) (line, col int) {
	off = min(max(off, 0), len(f.data))
	before := f.data[:off]
	line = bytes.Count(before, []byte{'\n'}) + 1
	start := bytes.LastIndexByte(before, '\n') + 1
	if line == 1 {
		start = max(start, min(f.bom, off))
	}
	return line, utf8.RuneCount(before[start:]) + 1
}

func (f *File) errAt(off int, key, format string, args ...any) *Error {
	e := &Error{Path: f.Path, Key: key, Msg: fmt.Sprintf(format, args...)}
	e.Line, e.Col = f.position(off)
	return e
}

// walker decodes the file a token at a time, so it sees each key as
// written and where it is.
type walker struct {
	f    *File
	dec  *json.Decoder
	body []byte // the file after any byte order mark
	root reflect.Type
}

// next is the file offset of the next token.
func (w *walker) next() int {
	off := int(w.dec.InputOffset())
	for off < len(w.body) && strings.IndexByte(jsonSpace+",:", w.body[off]) >= 0 {
		off++
	}
	return w.f.bom + off
}

// object decodes one JSON object into v: the whole file when section is
// "", else the object of that section.
func (w *walker) object(v reflect.Value, section string) error {
	at := w.next()
	tok, err := w.dec.Token()
	if err != nil {
		if section == "" && errors.Is(err, io.EOF) {
			return w.f.errAt(at, "", "is empty; it must hold one JSON object, such as {}")
		}
		return w.syntax(err, at)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		if section == "" {
			return w.f.errAt(at, "", "must hold one JSON object, such as {}")
		}
		return w.f.errAt(at, section, "must be an object")
	}
	fields := fieldsOf(v.Type())
	seen := map[string]bool{}
	for w.dec.More() {
		at := w.next()
		tok, err := w.dec.Token()
		if err != nil {
			return w.syntax(err, at)
		}
		key, _ := tok.(string)
		name := key
		if section != "" {
			name = section + "." + key
		}
		i, ok := fields[key]
		if !ok {
			return w.f.errAt(at, name, "%s", w.unknown(key, section, fields))
		}
		if seen[key] {
			return w.f.errAt(at, name, "appears more than once")
		}
		seen[key] = true
		field := v.Field(i)
		at = w.next()
		w.f.at[name] = at
		if isSection(field.Type()) {
			if section != "" {
				panic(fmt.Sprintf("configfile: section %s inside section %s", key, section))
			}
			field.Set(reflect.New(field.Type().Elem()))
			if err := w.object(field.Elem(), key); err != nil {
				return err
			}
			continue
		}
		var raw json.RawMessage
		if err := w.dec.Decode(&raw); err != nil {
			return w.syntax(err, at)
		}
		if string(raw) == "null" {
			return w.f.errAt(at, name, "null is not a value; remove the key to use the default")
		}
		p := reflect.New(field.Type().Elem())
		if err := json.Unmarshal(raw, p.Interface()); err != nil {
			return w.f.errAt(at, name, "must be %s", describe(field.Type().Elem()))
		}
		field.Set(p)
	}
	at = w.next()
	if _, err := w.dec.Token(); err != nil {
		return w.syntax(err, at)
	}
	return nil
}

// syntax turns a decoder error into an *Error at its place.
func (w *walker) syntax(err error, at int) *Error {
	se, ok := errors.AsType[*json.SyntaxError](err)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		ok && se.Error() == "unexpected end of JSON input":
		return w.f.errAt(len(w.f.data), "", "ends before the JSON object does")
	case ok:
		off := int(se.Offset) - 1
		if c := trailingComma(w.body, off); c >= 0 {
			return w.f.errAt(w.f.bom+c, "", "has a comma before the closing bracket; JSON allows none there")
		}
		return w.f.errAt(w.f.bom+off, "", "%s", se.Error())
	}
	return w.f.errAt(at, "", "%v", err)
}

// trailingComma finds the comma before a closing bracket that a syntax
// error at off is about: the error lands on the comma or on the bracket.
// It returns the comma's offset, or -1.
func trailingComma(b []byte, off int) int {
	if off < 0 || off >= len(b) {
		return -1
	}
	switch b[off] {
	case ',':
		rest := bytes.TrimLeft(b[off+1:], jsonSpace)
		if len(rest) > 0 && (rest[0] == '}' || rest[0] == ']') {
			return off
		}
	case '}', ']':
		before := bytes.TrimRight(b[:off], jsonSpace)
		if len(before) > 0 && before[len(before)-1] == ',' {
			return len(before) - 1
		}
	}
	return -1
}

// unknown says why key is not a key here, and what was likely meant.
func (w *walker) unknown(key, section string, fields map[string]int) string {
	for k := range fields {
		if strings.EqualFold(k, key) {
			return fmt.Sprintf("unknown key; keys are case-sensitive: did you mean %q?", k)
		}
	}
	top := fieldsOf(w.root)
	if section != "" {
		if i, ok := top[key]; ok && !isSection(w.root.Field(i).Type) {
			return fmt.Sprintf("is not a key of the %q section; it goes at the top level", section)
		}
		return "unknown key"
	}
	for name, i := range top {
		t := w.root.Field(i).Type
		if !isSection(t) {
			continue
		}
		if _, ok := fieldsOf(t.Elem())[key]; ok {
			return fmt.Sprintf("goes in the %q section", name)
		}
	}
	return "unknown key"
}

// fieldsOf maps the keys of a settings struct to its field indexes.
func fieldsOf(t reflect.Type) map[string]int {
	fields := map[string]int{}
	for i := range t.NumField() {
		sf := t.Field(i)
		name, _, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if sf.Type.Kind() != reflect.Pointer {
			panic(fmt.Sprintf("configfile: field %s of %s is not a pointer", sf.Name, t))
		}
		fields[name] = i
	}
	return fields
}

func isSection(t reflect.Type) bool {
	return t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct
}

// describe names the JSON a value of type t takes.
func describe(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice:
		return "a list of " + strings.TrimPrefix(describe(t.Elem()), "a ") + "s"
	}
	return "a " + t.String()
}
