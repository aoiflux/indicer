package compression_product

import (
	"testing"

	"indicer/internal/duesarc"
)

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
		if local, err := duesarc.LocalName(m.DbPath); err != nil || local == "" {
			t.Fatalf("accepted database path %q", m.DbPath)
		}
	})
}
