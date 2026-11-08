package duesarc

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"a.txt":       "a.txt",
		"./a.txt":     "a.txt",
		"dir/":        "dir",
		"a//b.txt":    filepath.Join("a", "b.txt"),
		"./":          "",
		".":           "",
		"x/./y":       filepath.Join("x", "y"),
		"..":          "!",
		"a/..":        "!",
		"/etc/passwd": "!",
		"a\\b":        "!",
		"bs\b":        "?", // a control character: refused on Windows only
		"nul\x00byte": "!",
	} {
		got, err := LocalName(name)
		if want == "?" {
			if refused := err != nil; refused != (runtime.GOOS == "windows") {
				t.Errorf("%q: refused=%v on %s", name, refused, runtime.GOOS)
			}
			continue
		}
		if want == "!" {
			if err == nil {
				t.Errorf("%q: accepted as %q", name, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", name, got, err, want)
		}
	}
}
