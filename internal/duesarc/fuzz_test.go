package duesarc

import (
	"encoding/json"
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
		local, err := LocalName(name)
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

// FuzzParseDatabaseManifest reads database package manifests it did not
// write: an error, or a manifest every path of which is a plain relative
// path, sorted, that its content hash covers; never a panic.
func FuzzParseDatabaseManifest(f *testing.F) {
	m := validManifest()
	m.Dirs = []string{"kvdb"}
	good, _ := json.Marshal(m)
	f.Add(good)
	f.Add([]byte(`{"version":"v3","kind":"database","db_path":"db","files":[{"path":"../x","size":1,"sha256":""}]}`))
	f.Add([]byte(`{"version":"v9","kind":"database"}`))
	f.Add([]byte(`{`))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseDatabaseManifest(data)
		if err != nil {
			return
		}
		for i, file := range m.Files {
			local, err := LocalName(file.Path)
			if err != nil || local == "" || !filepath.IsLocal(local) {
				t.Fatalf("accepted path %q", file.Path)
			}
			if i > 0 && m.Files[i-1].Path >= file.Path {
				t.Fatalf("accepted %q after %q", file.Path, m.Files[i-1].Path)
			}
		}
		if ContentHash(m.Files) != m.ContentSHA256 {
			t.Fatal("accepted a content hash that does not cover the files")
		}
	})
}
