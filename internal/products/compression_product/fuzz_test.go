package compression_product

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzLocalName: an archive entry name either is refused or becomes a path
// inside the output directory.
func FuzzLocalName(f *testing.F) {
	for _, s := range []string{"a.txt", "dir/b.txt", "../x", "/abs", `a\b`, "CON", "x:y", "a/./b", "trailing.", "dir/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		local, err := localName(name)
		if err != nil || local == "" {
			return
		}
		if !filepath.IsLocal(local) {
			t.Fatalf("%q became %q, which is not local", name, local)
		}
		for _, c := range strings.Split(filepath.ToSlash(local), "/") {
			if c == ".." || c == "" {
				t.Fatalf("%q became %q", name, local)
			}
		}
	})
}

// FuzzParseManifest reads .duesarc manifests it did not write: an error or
// a manifest whose database path is local, never a panic.
func FuzzParseManifest(f *testing.F) {
	f.Add([]byte(`{"version":"v2","pipeline":"store","db_path":"db","entries":[{"path":"a.txt","type":"file","evidence":"e","size":1}]}`))
	f.Add([]byte(`{"version":"v1","db_path":"../db"}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := parseManifest(data)
		if err != nil {
			return
		}
		if local, err := localName(m.DbPath); err != nil || local == "" {
			t.Fatalf("accepted database path %q", m.DbPath)
		}
	})
}
