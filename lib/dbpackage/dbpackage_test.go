package dbpackage

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"indicer/internal/duesarc"
	"indicer/internal/lifecycle"
	"indicer/internal/testutil"
	"indicer/internal/testutil/leakcheck"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/graph/signer"
	"indicer/lib/ingest"
	"indicer/lib/util"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m)) }

var fastKDF = dbmeta.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}

const evidenceBytes = 300 << 10

// newRoot makes a data root holding one evidence, plus what a package
// leaves out (locks, a killed writer's temporary file, an upload's part
// file) and stand-ins for a full-text index and a graph, which a package
// copies as files.
func newRoot(t *testing.T, creds dbmeta.Credentials) string {
	t.Helper()
	root := testdb.NewDataRoot(t)
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite, Creds: creds, KDF: &fastKDF, WrittenBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	src := testutil.WriteRandomFile(t, t.TempDir(), "evidence.bin", 7, evidenceBytes)
	if _, err := ingest.Run(t.Context(), h.DB, ingest.Request{Path: src, Decode: true, Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"graph/graphene.wal":                 "wal",
		"graph/dues.schema.json":             `{"schema_version": 3}`,
		"graph/graphene.csr.tmp":             "a compaction's image, half built",
		"graph/dues.lock":                    "",
		"graph/dues.lock.1-ab.info":          "holder",
		"graph/graphene.lock":                "",
		"fts/store/root.bolt":                "bolt",
		"uploads/a.part":                     "part",
		".dues-manifest.json.dues-tmp-12345": "temp",
		"dues.keys/keyring.json":             `{"version":1,"keys":[]}`,
		"dues.keys/actors.json":              `{"version":1,"actors":[]}`,
		"dues.keys/actors/" + testKeyFile:    "a private key",
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "fts", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// testKeyFile is the name of an actor's key file, as the keystore names one.
const testKeyFile = "0000000000000001-0000000000000002" + signer.KeyFileExt

func export(t *testing.T, opts ExportOptions) ExportResult {
	t.Helper()
	if opts.Output == "" {
		opts.Output = filepath.Join(t.TempDir(), "case.duesarc")
	}
	opts.Creator = duesarc.Creator{Product: "dues", Version: "test"}
	res, err := Export(t.Context(), opts)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return res
}

func paths(m duesarc.DatabaseManifest) []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, f.Path)
	}
	return out
}

// TestExportCopiesTheRoot: a package holds every file of the data root
// except the locks, temporary files and uploads, with its empty
// directories; verify finds it intact; a second package of the same state
// has the same content hash; and --without-derived leaves out the
// full-text index and the graph.
func TestExportCopiesTheRoot(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	res := export(t, ExportOptions{Path: root})
	m := res.Manifest
	got := paths(m)
	for _, want := range []string{dbmeta.ManifestName, "kvdb/MANIFEST", "kvdb/" + dbmeta.MirrorName, "graph/graphene.wal", "graph/dues.schema.json", "fts/store/root.bolt", "dues.keys/keyring.json", "dues.keys/actors.json"} {
		if !slices.Contains(got, want) {
			t.Errorf("the package lacks %s", want)
		}
	}
	if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "blob/") }) {
		t.Error("the package holds no blob")
	}
	for _, left := range []string{dbopen.LockName, "kvdb/LOCK", "graph/dues.lock", "graph/dues.lock.1-ab.info", "graph/graphene.lock", "graph/graphene.csr.tmp", "uploads/a.part", ".dues-manifest.json.dues-tmp-12345", "dues.keys/actors/" + testKeyFile} {
		if slices.Contains(got, left) {
			t.Errorf("the package holds %s", left)
		}
	}
	if !slices.Contains(m.Dirs, "fts/empty") || slices.Contains(m.Dirs, "uploads") {
		t.Errorf("dirs = %v", m.Dirs)
	}
	if m.Stats == nil || m.Stats.Evidence != 1 || m.Stats.LogicalBytes != evidenceBytes {
		t.Errorf("stats = %+v, want 1 evidence of %d bytes", m.Stats, evidenceBytes)
	}
	if m.Database.Components["graph_schema"] != 3 || m.Database.Encrypted || m.Host != nil {
		t.Errorf("database = %+v, host %+v", m.Database, m.Host)
	}
	if len(res.Files) != 1 || res.Files[0] != res.Output {
		t.Errorf("files = %v", res.Files)
	}

	p, err := Open(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	vr, err := p.Verify(t.Context(), VerifyOptions{})
	p.Close()
	if err != nil || vr.Files != len(m.Files) || vr.Bytes != m.Bytes() {
		t.Fatalf("verify = %+v, %v; want %d files, %d bytes", vr, err, len(m.Files), m.Bytes())
	}

	again := export(t, ExportOptions{Path: root, HostInfo: true})
	if again.Manifest.ContentSHA256 != m.ContentSHA256 {
		t.Error("two packages of one state have different content hashes")
	}
	if again.Manifest.Host == nil {
		t.Error("--host-info recorded no host")
	}

	bare := export(t, ExportOptions{Path: root, WithoutDerived: true})
	for _, f := range paths(bare.Manifest) {
		if strings.HasPrefix(f, "fts/") || strings.HasPrefix(f, "graph/") {
			t.Errorf("--without-derived kept %s", f)
		}
	}
	// The keyring is not derived: it checks what the database signed.
	if !slices.Contains(paths(bare.Manifest), "dues.keys/keyring.json") {
		t.Error("--without-derived left out the keyring")
	}
	if bare.Manifest.Database.Components != nil {
		t.Errorf("--without-derived recorded components %v", bare.Manifest.Database.Components)
	}
}

// unsigned is a manifest without its signature.
func unsigned(t *testing.T, data []byte) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "signature")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// rewrite copies the package at src to a new zip, through edit, which may
// change an entry's data or drop it, then adds extra entries.
func rewrite(t *testing.T, src string, edit func(name string, data []byte) ([]byte, bool), extra map[string]string) string {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		data, keep := edit(f.Name, data)
		if !keep {
			continue
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: f.Method, Modified: f.Modified})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range extra {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "edited.duesarc")
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestVerifyNamesWhatIsWrong: verify names the entry that does not match
// the manifest, whatever is wrong with it.
func TestVerifyNamesWhatIsWrong(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	const target = "db/kvdb/MANIFEST"
	same := func(name string, data []byte) ([]byte, bool) { return data, true }
	for _, c := range []struct {
		name   string
		edit   func(string, []byte) ([]byte, bool)
		extra  map[string]string
		entry  string
		reason string
	}{
		{"changed", func(name string, data []byte) ([]byte, bool) {
			if name == target {
				data = bytes.Clone(data)
				data[len(data)/2] ^= 0xff
			}
			return data, true
		}, nil, target, "SHA-256 does not match"},
		{"longer", func(name string, data []byte) ([]byte, bool) {
			if name == target {
				data = append(bytes.Clone(data), 'x')
			}
			return data, true
		}, nil, target, "bytes; the manifest says"},
		{"missing", func(name string, data []byte) ([]byte, bool) { return data, name != target }, nil, target, "missing"},
		{"added", same, map[string]string{"db/kvdb/extra": "x"}, "db/kvdb/extra", "does not list"},
		{"outside", same, map[string]string{"../evil": "x"}, "../evil", "does not list"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := Open(rewrite(t, res.Output, c.edit, c.extra))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			_, err = p.Verify(t.Context(), VerifyOptions{})
			bad, ok := errors.AsType[*DamagedError](err)
			if !ok {
				t.Fatalf("verify = %v, want a *DamagedError", err)
			}
			if bad.Count != 1 || bad.Problems[0].Name != c.entry || !strings.Contains(bad.Problems[0].Reason, c.reason) {
				t.Fatalf("problems = %+v (count %d), want %s: ...%s...", bad.Problems, bad.Count, c.entry, c.reason)
			}
		})
	}
}

// TestOpenRefusesABadManifest: a manifest whose list does not match its
// content hash, or that a newer DUES wrote, is refused before anything is
// read; a package cut short is damaged.
func TestOpenRefusesABadManifest(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	editManifest := func(change func(m map[string]any)) string {
		return rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
			if name != duesarc.ManifestName {
				return data, true
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			change(m)
			out, _ := json.Marshal(m)
			return out, true
		}, nil)
	}
	tampered := editManifest(func(m map[string]any) {
		m["files"].([]any)[0].(map[string]any)["size"] = 1
	})
	if _, err := Open(tampered); err == nil || !strings.Contains(err.Error(), "content hash") {
		t.Errorf("tampered manifest: %v", err)
	}
	newer := editManifest(func(m map[string]any) { m["version"] = "v4" })
	if _, err := Open(newer); !errors.Is(err, duesarc.ErrNewerPackage) {
		t.Errorf("newer manifest: %v, want %v", err, duesarc.ErrNewerPackage)
	}
	files := editManifest(func(m map[string]any) { delete(m, "kind") })
	if _, err := Open(files); err == nil || !strings.Contains(err.Error(), "not a DUES database") {
		t.Errorf("an archive of files: %v", err)
	}
	data, err := os.ReadFile(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.duesarc")
	if err := os.WriteFile(cut, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cut); err == nil {
		t.Error("opened a package cut short")
	} else if _, ok := errors.AsType[*duesarc.DamagedError](err); !ok {
		t.Errorf("a package cut short: %v, want a *duesarc.DamagedError", err)
	}
}

// TestExportRefuses: an export does not start on a database a writer holds
// or that was not closed cleanly, into the database, or over a package
// already there.
func TestExportRefuses(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	ctx := context.Background()

	h, err := dbopen.Open(ctx, dbopen.Request{Path: root, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Export(ctx, ExportOptions{Path: root, Output: filepath.Join(t.TempDir(), "x.duesarc")})
	if cerr := h.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if !errors.Is(err, dbopen.ErrInUse) {
		t.Errorf("beside a writer: %v, want %v", err, dbopen.ErrInUse)
	}

	if _, err := Export(ctx, ExportOptions{Path: root, Output: filepath.Join(root, "x.duesarc")}); !isUsage(err) {
		t.Errorf("into the database: %v, want a usage error", err)
	}
	out := filepath.Join(t.TempDir(), "x.duesarc")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(ctx, ExportOptions{Path: root, Output: out}); !isUsage(err) {
		t.Errorf("over a package: %v, want a usage error", err)
	}
	if _, err := Export(ctx, ExportOptions{Path: root, Output: out, Force: true}); err != nil {
		t.Errorf("--force: %v", err)
	}

	mem := filepath.Join(root, "kvdb", "00001.mem")
	if err := os.WriteFile(mem, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(ctx, ExportOptions{Path: root, Output: filepath.Join(t.TempDir(), "y.duesarc")}); !errors.Is(err, dbopen.ErrNeedsRecovery) {
		t.Errorf("not closed cleanly: %v, want %v", err, dbopen.ErrNeedsRecovery)
	}
	if err := os.Remove(mem); err != nil {
		t.Fatal(err)
	}

	if _, err := Export(ctx, ExportOptions{Path: t.TempDir()}); !errors.Is(err, dbopen.ErrNoDatabase) {
		t.Errorf("no database: %v, want %v", err, dbopen.ErrNoDatabase)
	}
}

func isUsage(err error) bool {
	_, ok := errors.AsType[*lifecycle.UsageError](err)
	return ok
}

// TestExportEncrypted: an encrypted database is packaged without its
// password, as its encrypted files, with no counts in the plaintext
// manifest and a warning that its full-text index and graph are not
// encrypted. A password given is checked.
func TestExportEncrypted(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.Password("right"))
	res := export(t, ExportOptions{Path: root})
	m := res.Manifest
	if !m.Database.Encrypted || m.Stats != nil {
		t.Errorf("database %+v, stats %+v", m.Database, m.Stats)
	}
	if !slices.Equal(m.Database.PlaintextComponents, []string{"fts", "graph"}) || len(res.Warnings) != 1 {
		t.Errorf("plaintext components %v, warnings %q", m.Database.PlaintextComponents, res.Warnings)
	}
	if !slices.ContainsFunc(res.Notices, func(n string) bool { return strings.Contains(n, "no password") }) {
		t.Errorf("notices %q say nothing of the password", res.Notices)
	}

	if _, err := Export(t.Context(), ExportOptions{Path: root, Output: filepath.Join(t.TempDir(), "x.duesarc"), Creds: dbmeta.Password("wrong")}); err == nil {
		t.Error("a wrong password was accepted")
	}
	checked := export(t, ExportOptions{Path: root, Creds: dbmeta.Password("right"), WithoutDerived: true})
	if checked.Manifest.Stats != nil || len(checked.Warnings) != 0 || checked.Manifest.ContentSHA256 == m.ContentSHA256 {
		t.Errorf("with the password, without derived: stats %+v, warnings %q", checked.Manifest.Stats, checked.Warnings)
	}
}

// TestExportSplit: a split package's parts and sidecar verify from any
// part; a changed part is found by the sidecar.
func TestExportSplit(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword()), PartSize: 128 << 10})
	if len(res.Files) < 2 {
		t.Fatalf("files = %v, want parts", res.Files)
	}
	p, err := Open(res.Files[1])
	if err != nil {
		t.Fatal(err)
	}
	vr, err := p.Verify(t.Context(), VerifyOptions{})
	p.Close()
	if err != nil || !vr.SidecarChecked || p.Parts != len(res.Files) {
		t.Fatalf("verify = %+v, %v; parts %d", vr, err, p.Parts)
	}

	data, err := os.ReadFile(res.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(res.Files[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = Open(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Verify(t.Context(), VerifyOptions{}); err == nil || !strings.Contains(err.Error(), "does not match its sidecar") {
		t.Fatalf("verify of a changed part: %v", err)
	}
}

// sameFiles fails unless every file of the package's manifest is in root
// with the bytes it has in src.
func sameFiles(t *testing.T, m duesarc.DatabaseManifest, src, root string) {
	t.Helper()
	for _, f := range m.Files {
		a, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("%s differs after import: %v", f.Path, err)
		}
	}
}

// noStage fails if an import left its directory beside root, or made root.
func noStage(t *testing.T, root string, rootMayExist bool) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), stageMarker) {
			t.Errorf("an import left %s", e.Name())
		}
	}
	if _, err := os.Stat(root); err == nil && !rootMayExist {
		t.Errorf("a failed import made %s", root)
	}
}

// TestImportRoundTrip: an imported database holds the package's files as
// they were, opens, counts what the package says, and takes new evidence.
func TestImportRoundTrip(t *testing.T) {
	t.Parallel()
	src := newRoot(t, dbmeta.NoPassword())
	res := export(t, ExportOptions{Path: src})
	target := filepath.Join(t.TempDir(), "copy")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	ir, err := Import(t.Context(), ImportOptions{Package: res.Output, Path: target})
	if err != nil {
		t.Fatal(err)
	}
	if !ir.Opened || ir.Root != target || ir.Manifest.ContentSHA256 != res.Manifest.ContentSHA256 {
		t.Fatalf("import = %+v", ir)
	}
	sameFiles(t, res.Manifest, src, target)
	for _, left := range []string{dbopen.LockName, stageLockName, "uploads"} {
		if _, err := os.Stat(filepath.Join(target, left)); err == nil {
			t.Errorf("the import made %s", left)
		}
	}
	if info, err := os.Stat(filepath.Join(target, "fts", "empty")); err != nil || !info.IsDir() {
		t.Errorf("the empty directory did not come back: %v", err)
	}
	noStage(t, target, true)

	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: target, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	more := testutil.WriteRandomFile(t, t.TempDir(), "more.bin", 9, 64<<10)
	_, err = ingest.Run(t.Context(), h.DB, ingest.Request{Path: more, Decode: true, Quiet: true})
	stats, serr := EvidenceStats(t.Context(), h.DB)
	if cerr := h.Close(); err != nil || serr != nil || cerr != nil {
		t.Fatalf("store into the import: %v, %v, %v", err, serr, cerr)
	}
	if stats.Evidence != 2 {
		t.Errorf("after one more store: %+v", stats)
	}
}

// TestImportEncrypted: an encrypted package imports without its password,
// unopened, and with it, opened; a wrong password imports nothing.
func TestImportEncrypted(t *testing.T) {
	t.Parallel()
	src := newRoot(t, dbmeta.Password("right"))
	res := export(t, ExportOptions{Path: src})

	blind := filepath.Join(t.TempDir(), "blind")
	ir, err := Import(t.Context(), ImportOptions{Package: res.Output, Path: blind})
	if err != nil || ir.Opened || !slices.ContainsFunc(ir.Notices, func(n string) bool { return strings.Contains(n, "not opened") }) {
		t.Fatalf("without the password: %+v, %v", ir, err)
	}
	sameFiles(t, res.Manifest, src, blind)

	wrong := filepath.Join(t.TempDir(), "wrong")
	if _, err := Import(t.Context(), ImportOptions{Package: res.Output, Path: wrong, Creds: dbmeta.Password("wrong")}); err == nil {
		t.Fatal("a wrong password imported")
	}
	noStage(t, wrong, false)

	opened := filepath.Join(t.TempDir(), "opened")
	if ir, err := Import(t.Context(), ImportOptions{Package: res.Output, Path: opened, Creds: dbmeta.Password("right")}); err != nil || !ir.Opened {
		t.Fatalf("with the password: %+v, %v", ir, err)
	}
}

// TestImportRefuses: an import makes nothing, and leaves nothing, when the
// target holds anything, the package is damaged or from a newer DUES, or
// the import is cancelled; it sweeps what a killed import left.
func TestImportRefuses(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	ctx := t.Context()

	full := filepath.Join(t.TempDir(), "full")
	if err := os.MkdirAll(full, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, ImportOptions{Package: res.Output, Path: full}); !isUsage(err) {
		t.Errorf("into a folder with files: %v, want a usage error", err)
	}

	damaged := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
		if name == "db/kvdb/MANIFEST" {
			data = bytes.Clone(data)
			data[0] ^= 0xff
		}
		return data, true
	}, nil)
	target := filepath.Join(t.TempDir(), "target")
	if _, err := Import(ctx, ImportOptions{Package: damaged, Path: target}); !isDamaged(err, "db/kvdb/MANIFEST") {
		t.Errorf("a damaged package: %v", err)
	}
	noStage(t, target, false)

	newer := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
		if name == duesarc.ManifestName {
			data = bytes.Replace(data, []byte(`"format_version": 1`), []byte(`"format_version": 2`), 1)
		}
		return data, true
	}, nil)
	if _, err := Import(ctx, ImportOptions{Package: newer, Path: target}); !errors.Is(err, dbmeta.ErrNewerFormat) {
		t.Errorf("a newer format: %v, want %v", err, dbmeta.ErrNewerFormat)
	}

	newerGraph := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
		if name == duesarc.ManifestName {
			data = bytes.Replace(data, []byte(`"graph_schema": 3`), []byte(`"graph_schema": 4`), 1)
		}
		return data, true
	}, nil)
	if _, err := Import(ctx, ImportOptions{Package: newerGraph, Path: target}); err == nil || !strings.Contains(err.Error(), "schema version 4") {
		t.Errorf("a newer graph schema: %v", err)
	}
	noStage(t, target, false)

	miscounted := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
		if name == duesarc.ManifestName {
			// Unsigned: a signed manifest that was changed fails earlier.
			data = unsigned(t, bytes.Replace(data, []byte(`"evidence": 1`), []byte(`"evidence": 5`), 1))
		}
		return data, true
	}, nil)
	if _, err := Import(ctx, ImportOptions{Package: miscounted, Path: target}); err == nil || !strings.Contains(err.Error(), "its manifest says 5") {
		t.Errorf("a database that does not hold what the package says: %v", err)
	}
	noStage(t, target, false)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Import(cancelled, ImportOptions{Package: res.Output, Path: target}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	noStage(t, target, false)

	stale := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+stageMarker+"123")
	if err := os.MkdirAll(filepath.Join(stale, "kvdb"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStageAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, ImportOptions{Package: res.Output, Path: target}); err != nil {
		t.Fatal(err)
	}
	noStage(t, target, true)
}

func isDamaged(err error, entry string) bool {
	bad, ok := errors.AsType[*DamagedError](err)
	return ok && bad.Problems[0].Name == entry
}

// TestOpenReadsOnlyTheManifest: opening a package, which is all inspect
// does, reads the manifest and no file's data: a package whose every file
// is damaged opens, and only verify finds the damage.
func TestOpenReadsOnlyTheManifest(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	data, err := os.ReadFile(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	damaged := 0
	for _, f := range zr.File {
		if f.Name == duesarc.ManifestName || f.UncompressedSize64 == 0 {
			continue
		}
		off, err := f.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		data[off] ^= 0xff
		damaged++
	}
	if err := os.WriteFile(res.Output, data, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Open(res.Output)
	if err != nil {
		t.Fatalf("open of a package with damaged files: %v", err)
	}
	defer p.Close()
	if p.Manifest.ContentSHA256 != res.Manifest.ContentSHA256 {
		t.Fatal("the manifest read back differs")
	}
	_, err = p.Verify(t.Context(), VerifyOptions{})
	if bad, ok := errors.AsType[*DamagedError](err); !ok || bad.Count != damaged {
		t.Fatalf("verify = %v, want %d problems", err, damaged)
	}
}
