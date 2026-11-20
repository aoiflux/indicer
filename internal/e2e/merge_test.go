package e2e

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
)

// TestPackageMerge runs dues package merge black box: a signed package
// merges, checked against its database's exported key, into a new encrypted
// database of another chunk size, restores to the same bytes and lists where
// it came from; a rerun finds it present and leaves no copy; a damaged
// package or one under a key that did not sign it merges nothing; and an
// encrypted package merges into an unencrypted database only with
// --allow-plaintext.
func TestPackageMerge(t *testing.T) {
	t.Parallel()
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	src := testutil.WriteRandomFile(t, work, "ev.bin", 5, 512<<10)
	db := filepath.Join(work, "case")
	duesMust(t, bin, work, "store", "ev.bin", "--dbpath", db)
	out := duesMust(t, bin, work, "package", "export", "--dbpath", db, "--out", "case.duesarc").Stdout
	hash := contentSHA(t, out)
	duesMust(t, bin, work, "actor", "export-public", "database", "--dbpath", db, "--out", "case.pub")
	pw := filepath.Join(work, "pw.txt")
	if err := os.WriteFile(pw, []byte("merge-target-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(work, "target")
	r := duesMust(t, bin, work, "package", "merge", "case.duesarc", "--dbpath", target, "--password-file", pw, "--chonksize", "64", "--trusted-key", "case.pub")
	for _, want := range []string{"1 merged, 0 aliased, 0 present, 0 skipped, 0 failed", "content sha256 " + hash, "the key is the trusted key case.pub"} {
		if !strings.Contains(r.Stdout, want) {
			t.Errorf("merge lacks %q:\n%s", want, r.Combined())
		}
	}
	if n := strings.Count(r.Stderr, "created database "+target); n != 1 {
		t.Errorf("the new target was noticed %d times:\n%s", n, r.Stderr)
	}
	if !hasOutcome(r.Stdout, "merged", "ev.bin") {
		t.Errorf("merge does not report ev.bin merged:\n%s", r.Stdout)
	}
	noStageLeft(t, work)

	ev := mergedEvidence(t, bin, work, target, "--password-file", pw)
	if m := ev.Merged; m == nil || m.Package != hash || m.SourceEvidenceID == "" || m.SourceActor != "database" || !m.Trusted {
		t.Errorf("list of the target: merged %+v", m)
	}
	restored := filepath.Join(work, "restored.bin")
	duesMust(t, bin, work, "restore", ev.Hash, "-f", restored, "--verify", "full", "--dbpath", target, "--password-file", pw)
	if testutil.FileSHA256(t, restored) != testutil.FileSHA256(t, src) {
		t.Fatal("the merged evidence restored to other content")
	}

	r = duesMust(t, bin, work, "package", "merge", "case.duesarc", "--dbpath", target, "--password-file", pw)
	if !hasOutcome(r.Stdout, "present", "ev.bin") || !strings.Contains(r.Stdout, "0 merged, 0 aliased, 1 present, 0 skipped, 0 failed") {
		t.Errorf("rerun:\n%s", r.Combined())
	}
	noStageLeft(t, work)

	data, err := os.ReadFile(filepath.Join(work, "case.duesarc"))
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/3] ^= 0xff
	if err := os.WriteFile(filepath.Join(work, "bad.duesarc"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The target is made before the copy that finds the damage.
	damaged := filepath.Join(work, "damaged")
	r = duesRun(t, bin, work, "package", "merge", "bad.duesarc", "--dbpath", damaged)
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "db/") || !strings.Contains(r.Stderr, "nothing was merged") || strings.Count(r.Stderr, "created database") != 1 {
		t.Errorf("merge of a changed package: exit %d\n%s", r.ExitCode, r.Combined())
	}
	if list, err := parseList(duesMust(t, bin, work, "list", "--dbpath", damaged).Stdout); err != nil || len(list.Evidence) != 0 {
		t.Errorf("list after a damaged package: %+v, %v", list, err)
	}
	noStageLeft(t, work)

	duesMust(t, bin, work, "package", "export", "--dbpath", target, "--password-file", pw, "--out", "enc.duesarc")
	untrusted := filepath.Join(work, "untrusted")
	r = duesRun(t, bin, work, "package", "merge", "enc.duesarc", "--dbpath", untrusted, "--source-password-file", pw, "--trusted-key", "case.pub")
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "nothing was merged") {
		t.Errorf("merge under a key that did not sign the package: exit %d\n%s", r.ExitCode, r.Combined())
	}
	notMade(t, untrusted)

	plain := filepath.Join(work, "plain")
	r = duesRun(t, bin, work, "package", "merge", "enc.duesarc", "--dbpath", plain, "--source-password-file", pw)
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, "--allow-plaintext") {
		t.Errorf("encrypted into unencrypted: exit %d\n%s", r.ExitCode, r.Combined())
	}
	notMade(t, plain)
	r = duesMust(t, bin, work, "package", "merge", "enc.duesarc", "--dbpath", plain, "--source-password-file", pw, "--allow-plaintext")
	if !hasOutcome(r.Stdout, "merged", "ev.bin") {
		t.Errorf("encrypted into unencrypted with --allow-plaintext:\n%s", r.Combined())
	}
	noStageLeft(t, work)
}

// listedMerge is one evidence of `dues list`, with where it was merged from.
type listedMerge struct {
	Name   string `json:"name"`
	Hash   string `json:"hash"`
	Merged *struct {
		Package          string `json:"package"`
		SourceEvidenceID string `json:"sourceEvidenceId"`
		SourceActor      string `json:"sourceActor"`
		Trusted          bool   `json:"trusted"`
	} `json:"merged"`
}

// mergedEvidence is the one evidence the database at root lists.
func mergedEvidence(t *testing.T, bin, work, root string, flags ...string) listedMerge {
	t.Helper()
	r := duesMust(t, bin, work, append([]string{"list", "--dbpath", root}, flags...)...)
	var doc struct {
		Evidence []listedMerge `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &doc); err != nil || len(doc.Evidence) != 1 {
		t.Fatalf("list of %s: %v\n%s", root, err, r.Combined())
	}
	return doc.Evidence[0]
}

// contentSHA is the content hash an export reports.
func contentSHA(t *testing.T, stdout string) string {
	t.Helper()
	const label = "content sha256 "
	_, rest, ok := strings.Cut(stdout, label)
	if !ok {
		t.Fatalf("no content hash in:\n%s", stdout)
	}
	hash, _, _ := strings.Cut(rest, "\n")
	return hash
}

// hasOutcome reports whether a merge's stdout says the evidence named name
// ended as outcome.
func hasOutcome(stdout, outcome, name string) bool {
	for line := range strings.Lines(stdout) {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == outcome && strings.HasSuffix(f[1], name) {
			return true
		}
	}
	return false
}

// noStageLeft fails when a merge left its copy of a package, or the copy's
// lock, in dir.
func noStageLeft(t *testing.T, dir string) {
	t.Helper()
	if left := testutil.FilesMatching(t, dir, "*.dues-merge-*"); len(left) > 0 {
		t.Fatalf("a merge left %v", left)
	}
}

// notMade fails when a refused merge made its target.
func notMade(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused merge made %s: %v", root, err)
	}
}
