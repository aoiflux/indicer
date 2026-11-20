package duesarc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validManifest() DatabaseManifest {
	files := []File{
		{Path: "dues-manifest.json", Size: 3, SHA256: strings.Repeat("ab", 32)},
		{Path: "kvdb/MANIFEST", Size: 5, SHA256: strings.Repeat("cd", 32)},
	}
	return DatabaseManifest{
		Version: VersionDatabase, Kind: KindDatabase, Pipeline: PipelineExport, DbPath: DatabaseDir,
		Files: files, ContentSHA256: ContentHash(files),
	}
}

// TestParseDatabaseManifest: a manifest is refused when it is not a
// database's, a newer DUES wrote it, or a path it lists could reach outside
// the package's database or appears twice.
func TestParseDatabaseManifest(t *testing.T) {
	t.Parallel()
	encode := func(m DatabaseManifest) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := ParseDatabaseManifest(encode(validManifest())); err != nil {
		t.Fatalf("a valid manifest: %v", err)
	}
	withFile := func(p string) func(*DatabaseManifest) {
		return func(m *DatabaseManifest) {
			m.Files = append(m.Files, File{Path: p, Size: 1, SHA256: strings.Repeat("ef", 32)})
			m.ContentSHA256 = ContentHash(m.Files)
		}
	}
	for name, c := range map[string]struct {
		change func(*DatabaseManifest)
		want   string
	}{
		"files":         {func(m *DatabaseManifest) { m.Kind = "" }, "not a DUES database"},
		"newer":         {func(m *DatabaseManifest) { m.Version = "v4" }, ErrNewerPackage.Error()},
		"older":         {func(m *DatabaseManifest) { m.Version = "v2" }, "cannot hold a database"},
		"bad version":   {func(m *DatabaseManifest) { m.Version = "3" }, "unknown manifest version"},
		"db path":       {func(m *DatabaseManifest) { m.DbPath = "." }, "puts the database at"},
		"no files":      {func(m *DatabaseManifest) { m.Files = nil; m.ContentSHA256 = ContentHash(nil) }, "lists no files"},
		"parent":        {withFile("../x"), "parent directory"},
		"absolute":      {withFile("/etc/x"), "absolute"},
		"dot element":   {withFile("x/./y"), "not a clean relative path"},
		"backslash":     {withFile(`x\y`), "backslash"},
		"twice":         {withFile("kvdb/MANIFEST"), "out of order or listed twice"},
		"out of order":  {withFile("a"), "out of order"},
		"no sha":        {func(m *DatabaseManifest) { m.Files[0].SHA256 = "zz"; m.ContentSHA256 = ContentHash(m.Files) }, "no valid SHA-256"},
		"negative size": {func(m *DatabaseManifest) { m.Files[0].Size = -1; m.ContentSHA256 = ContentHash(m.Files) }, "negative size"},
		"content hash":  {func(m *DatabaseManifest) { m.Files[0].Size = 4 }, "content hash"},
		"unsafe dir":    {func(m *DatabaseManifest) { m.Dirs = []string{"../up"} }, "parent directory"},
	} {
		m := validManifest()
		c.change(&m)
		_, err := ParseDatabaseManifest(encode(m))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error saying %q", name, err, c.want)
		}
		if name == "newer" && !errors.Is(err, ErrNewerPackage) {
			t.Errorf("newer: %v is not ErrNewerPackage", err)
		}
	}
}

// TestContentHashIgnoresTime: the content hash depends on the files alone,
// in order.
func TestContentHashIgnoresTime(t *testing.T) {
	t.Parallel()
	a, b := validManifest(), validManifest()
	b.CreatedAtUTC, b.Host = "2030-01-01T00:00:00Z", &Host{OS: "plan9"}
	if ContentHash(a.Files) != ContentHash(b.Files) {
		t.Fatal("equal files hash differently")
	}
	b.Files[0], b.Files[1] = b.Files[1], b.Files[0]
	if ContentHash(a.Files) == ContentHash(b.Files) {
		t.Fatal("the order of the files does not change the hash")
	}
}
