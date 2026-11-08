package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
)

// TestPackage runs dues package black box: export a database with an
// evidence, inspect and verify the package, deeply too, reading the
// evidence back from a copy that is then removed, and see verify fail, naming
// the file, once a byte of the package changes; import it as a new
// database, which lists the evidence, and not over one; split, the parts
// verify from any of them.
func TestPackage(t *testing.T) {
	t.Parallel()
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	testutil.WriteRandomFile(t, work, "ev.bin", 5, 512<<10)
	db := filepath.Join(work, "case")
	duesMust(t, bin, work, "store", "ev.bin", "--dbpath", db)

	out := duesMust(t, bin, work, "package", "export", "--dbpath", db, "--out", "case.duesarc").Stdout
	if !strings.Contains(out, "Exported database "+db+" to "+filepath.Join(work, "case.duesarc")) || !strings.Contains(out, "content sha256 ") {
		t.Fatalf("export:\n%s", out)
	}
	hash := out[strings.Index(out, "content sha256 ")+len("content sha256 "):]
	hash = strings.TrimSpace(hash)

	info := duesMust(t, bin, work, "package", "inspect", "case.duesarc").Stdout
	for _, want := range []string{"DUES database package case.duesarc", "dues-db format 1", "encrypted  no", "evidence   1, 512 KiB", "sha256 " + hash} {
		if !strings.Contains(info, want) {
			t.Errorf("inspect lacks %q:\n%s", want, info)
		}
	}
	if v := duesMust(t, bin, work, "package", "verify", "case.duesarc").Stdout; !strings.Contains(v, "Package case.duesarc is intact") {
		t.Errorf("verify:\n%s", v)
	}
	scratch := t.TempDir()
	deep := duesMust(t, bin, work, "package", "verify", "case.duesarc", "--deep", "--temp-dir", scratch)
	if !strings.Contains(deep.Stdout, "its database opens, and 1 evidence read back against their chunk hashes") || !strings.Contains(deep.Stderr, "Verified 1 evidence; 0 damaged.") {
		t.Errorf("verify --deep:\n%s", deep.Combined())
	}
	if left, _ := os.ReadDir(scratch); len(left) > 0 {
		t.Errorf("verify --deep left %s in --temp-dir", left[0].Name())
	}

	r := duesRun(t, bin, work, "package", "export", "--dbpath", db, "--out", "case.duesarc")
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, "already exists; pass --force") {
		t.Errorf("export over a package: exit %d\n%s", r.ExitCode, r.Combined())
	}

	data, err := os.ReadFile(filepath.Join(work, "case.duesarc"))
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/3] ^= 0xff
	if err := os.WriteFile(filepath.Join(work, "bad.duesarc"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	r = duesRun(t, bin, work, "package", "verify", "bad.duesarc")
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "db/") || !strings.Contains(r.Stderr, "does not match its manifest: 1 problem") {
		t.Errorf("verify of a changed package: exit %d\n%s", r.ExitCode, r.Combined())
	}

	copyDB := filepath.Join(work, "copy")
	imp := duesMust(t, bin, work, "package", "import", "case.duesarc", "--dbpath", copyDB).Stdout
	if !strings.Contains(imp, "Imported case.duesarc as database "+copyDB) || !strings.Contains(imp, "sha256 "+hash) {
		t.Errorf("import:\n%s", imp)
	}
	list, err := parseList(duesMust(t, bin, work, "list", "--dbpath", copyDB).Stdout)
	if err != nil || len(list.Evidence) != 1 || filepath.Base(list.Evidence[0].Name) != "ev.bin" {
		t.Errorf("list of the imported database: %+v, %v", list, err)
	}
	r = duesRun(t, bin, work, "package", "import", "case.duesarc", "--dbpath", copyDB)
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, "is not empty") {
		t.Errorf("import over a database: exit %d\n%s", r.ExitCode, r.Combined())
	}

	duesMust(t, bin, work, "package", "export", "--dbpath", db, "--out", "split.duesarc", "--split-size-mb", "1")
	v := duesMust(t, bin, work, "package", "verify", "split.duesarc.002").Stdout
	if !strings.Contains(v, "parts match their sidecar") || !strings.Contains(v, "sha256 "+hash) {
		t.Errorf("verify of a split package:\n%s", v)
	}
}
