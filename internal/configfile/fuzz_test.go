package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// FuzzParse checks that any input gives a settings struct or an *Error
// with a place inside the file, never a panic, and that what it accepts
// encoding/json reads the same way.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"name": "a", "plain": true, "count": 8, "list": ["x"], "store": {"workers": 2, "mode": "m"}}`,
		`{"Name": 1}`, `{"name": "a", "name": "b"}`, `{"store": {"plain": 1}}`, `{"name": null}`,
		"{\n  \"count\": x\n}", `{"list": ["a", ]}`, "\xEF\xBB\xBF{}", "\xff\xfe", `[[[[[[[[[[]]]]]]]]]]`,
		`{"list": [[[[[[[[["a"]]]]]]]]]}`, `{"store": {"mode": "a"}} {}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var got testConfig
		_, err := parse("c.json", data, &got)
		if err != nil {
			e, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("not an *Error: %v", err)
			}
			if lines := bytes.Count(data, []byte{'\n'}) + 1; e.Line < 0 || e.Line > lines || (e.Line > 0) != (e.Col > 0) {
				t.Fatalf("place %d:%d outside a file of %d lines: %v", e.Line, e.Col, lines, err)
			}
			return
		}
		var want testConfig
		dec := json.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, utf8BOM)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&want); err != nil {
			t.Fatalf("accepted what encoding/json refuses: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, encoding/json read %+v", got, want)
		}
	})
}
