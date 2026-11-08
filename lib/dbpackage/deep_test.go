package dbpackage

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/duesarc"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
)

// verifyDeep opens a package and verifies it deeply, copying into tmp.
func verifyDeep(t *testing.T, path, tmp string, creds dbmeta.Credentials) (VerifyResult, error) {
	t.Helper()
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	return p.VerifyDeep(t.Context(), DeepOptions{Creds: creds, TempDir: tmp})
}

// noDeepStage fails if a deep verify left its copy in tmp.
func noDeepStage(t *testing.T, tmp string) {
	t.Helper()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), deepStagePrefix) {
			t.Errorf("a deep verify left %s", e.Name())
		}
	}
}

// TestVerifyDeepReadsTheEvidence: a deep verify of an intact package reads
// its evidence back and leaves nothing in the temporary directory, which it
// also clears of what a killed verify left.
func TestVerifyDeepReadsTheEvidence(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	tmp := t.TempDir()
	stale := filepath.Join(tmp, deepStagePrefix+"killed")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStageAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	vr, err := verifyDeep(t, res.Output, tmp, dbmeta.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	if ev := vr.Evidence; ev == nil || ev.Verified != 1 || ev.Problems != 0 {
		t.Fatalf("deep verify = %+v, evidence %+v", vr, vr.Evidence)
	}
	if vr.Files != len(res.Manifest.Files) || vr.Bytes != res.Manifest.Bytes() {
		t.Errorf("deep verify counted %d files, %d bytes; the package has %d, %d", vr.Files, vr.Bytes, len(res.Manifest.Files), res.Manifest.Bytes())
	}
	noDeepStage(t, tmp)
}

// TestVerifyDeepFindsDamagedEvidence: a package made from a database with
// a damaged chunk matches its manifest, so verify passes; a deep verify
// reads the evidence back and reports it damaged.
func TestVerifyDeepFindsDamagedEvidence(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	var blob string
	err := filepath.WalkDir(filepath.Join(root, cnst.BLOBSDIR), func(p string, d fs.DirEntry, err error) error {
		if err == nil && blob == "" && d.Type().IsRegular() {
			blob = p
		}
		return err
	})
	if err != nil || blob == "" {
		t.Fatalf("no blob in %s: %v", root, err)
	}
	raw, err := os.ReadFile(blob)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-10] ^= 0x10
	if err := os.WriteFile(blob, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	res := export(t, ExportOptions{Path: root})

	p, err := Open(res.Output)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Verify(t.Context())
	p.Close()
	if err != nil {
		t.Fatalf("verify of a package that matches its manifest: %v", err)
	}
	tmp := t.TempDir()
	vr, err := verifyDeep(t, res.Output, tmp, dbmeta.NoPassword())
	if err != nil {
		t.Fatal(err)
	}
	if ev := vr.Evidence; ev == nil || ev.Verified != 0 || ev.Problems != 1 || len(ev.Results) != 1 || ev.Results[0].Problem == "" {
		t.Fatalf("deep verify of damaged evidence: %+v", vr.Evidence)
	}
	noDeepStage(t, tmp)
}

// TestVerifyDeepRefuses: a deep verify of an encrypted package needs its
// password; one whose files do not match the manifest, or whose database
// holds other evidence than the manifest says, fails. None leaves its copy.
func TestVerifyDeepRefuses(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	enc := export(t, ExportOptions{Path: newRoot(t, dbmeta.Password("right"))})
	if _, err := verifyDeep(t, enc.Output, tmp, dbmeta.NoPassword()); !isUsage(err) {
		t.Errorf("encrypted, without the password: %v, want a usage error", err)
	}
	if _, err := verifyDeep(t, enc.Output, tmp, dbmeta.Password("wrong")); err == nil {
		t.Error("encrypted, with a wrong password: no error")
	}
	if vr, err := verifyDeep(t, enc.Output, tmp, dbmeta.Password("right")); err != nil || vr.Evidence == nil || vr.Evidence.Verified != 1 {
		t.Errorf("encrypted, with the password: %+v, %v", vr, err)
	}

	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	const target = "db/kvdb/MANIFEST"
	missing := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) { return data, name != target }, nil)
	if _, err := verifyDeep(t, missing, tmp, dbmeta.NoPassword()); !isDamaged(err, target) {
		t.Errorf("a file missing: %v", err)
	}
	changed := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
		if name == target {
			data = bytes.Clone(data)
			data[len(data)/2] ^= 0xff
		}
		return data, true
	}, nil)
	if _, err := verifyDeep(t, changed, tmp, dbmeta.NoPassword()); !isDamaged(err, target) {
		t.Errorf("a file changed: %v", err)
	}

	// The content hash covers the file list, not the counts: a manifest
	// that claims more evidence still opens, and the deep verify counts.
	more := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
		if name == duesarc.ManifestName {
			data = bytes.Replace(data, []byte(`"evidence": 1`), []byte(`"evidence": 2`), 1)
		}
		return data, true
	}, nil)
	_, err := verifyDeep(t, more, tmp, dbmeta.NoPassword())
	if err == nil || !strings.Contains(err.Error(), "holds 1 evidence") {
		t.Errorf("a manifest that claims more evidence: %v", err)
	}
	if _, ok := errors.AsType[*DamagedError](err); ok {
		t.Errorf("the count is no damage to the files: %v", err)
	}
	noDeepStage(t, tmp)
}
