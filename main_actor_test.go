package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/dbopen"
	"indicer/lib/graph/signer"
)

// actorFor parses args and resolves the actor the way run does.
func actorFor(t *testing.T, stdin string, keyCommand bool, args ...string) (*dbopen.Actor, []byte, error) {
	t.Helper()
	cliDef := newCLIDefinition()
	if _, err := cliDef.app.Parse(args); err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	set, err := flagsSetByUser(cliDef.app, args)
	if err != nil {
		t.Fatal(err)
	}
	return resolveActor(cliDef.flags, sources{set: set}, strings.NewReader(stdin), keyCommand)
}

func writePass(t *testing.T, pass string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pass")
	if err := os.WriteFile(p, []byte(pass+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestActorFlagRules: --actor needs its key's passphrase, from a file or
// stdin; the passphrase alone is refused, and so are two readers of stdin,
// the database's own name, and --actor on the commands that name their
// actor as an argument.
func TestActorFlagRules(t *testing.T) {
	t.Parallel()
	pass := writePass(t, "correct horse")

	a, _, err := actorFor(t, "", false, "--actor", " Jane ", "--actor-key-password-file", pass, "list")
	if err != nil || a == nil || a.Name != " Jane " || string(a.Passphrase) != "correct horse" {
		t.Fatalf("file: %+v %v", a, err)
	}
	a, _, err = actorFor(t, "from stdin\nmore\n", false, "--actor", "jane", "--actor-key-password-stdin", "list")
	if err != nil || string(a.Passphrase) != "from stdin" {
		t.Fatalf("stdin: %+v %v", a, err)
	}
	if a, p, err := actorFor(t, "", false, "list"); a != nil || p != nil || err != nil {
		t.Fatalf("no actor: %+v %q %v", a, p, err)
	}
	_, p, err := actorFor(t, "", true, "--actor-key-password-file", pass, "actor", "create", "jane")
	if err != nil || string(p) != "correct horse" {
		t.Fatalf("create's passphrase: %q %v", p, err)
	}

	for name, c := range map[string]struct {
		key  bool
		args []string
		want string
	}{
		"no passphrase":       {false, []string{"--actor", "jane", "list"}, "needs the passphrase"},
		"passphrase alone":    {false, []string{"--actor-key-password-file", pass, "list"}, "name one"},
		"two stdin readers":   {false, []string{"--actor", "jane", "--actor-key-password-stdin", "--password-stdin", "list"}, "both read standard input"},
		"the database's name": {false, []string{"--actor", "DATABASE", "--actor-key-password-file", pass, "list"}, "the database's own actor"},
		"actor on create":     {true, []string{"--actor", "jane", "--actor-key-password-file", pass, "actor", "create", "john"}, "take the actor's name as an argument"},
		"empty passphrase":    {false, []string{"--actor", "jane", "--actor-key-password-file", writePass(t, ""), "list"}, "holds no passphrase"},
	} {
		_, _, err := actorFor(t, "", c.key, c.args...)
		if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(fmt.Sprint(err), c.want) {
			t.Errorf("%s: %v (exit %d); want a usage error with %q", name, err, lifecycle.ExitCode(err), c.want)
		}
	}
}

// TestActorCommands: an actor made with dues actor create signs a store's
// graph commits with --actor; list, export-public, rotate and revoke manage
// its keys.
func TestActorCommands(t *testing.T) {
	_, logArg := logArgs(t)
	db := filepath.Join(t.TempDir(), "case")
	pass, newPass := writePass(t, "correct horse"), writePass(t, "battery staple")
	dues := func(want int, args ...string) (string, string) {
		t.Helper()
		code := -1
		stdout, stderr := captureOutput(t, func() { code = realMain(append(append(logArg, "--dbpath", db), args...)) })
		if code != want {
			t.Fatalf("dues %q exited %d, want %d\nstdout %s\nstderr %s", args, code, want, stdout, stderr)
		}
		return stdout, stderr
	}

	dues(lifecycle.ExitOK, "actor", "create", "Jane", "--actor-key-password-file", pass)
	_, stderr := dues(lifecycle.ExitUsage, "actor", "create", "jane", "--actor-key-password-file", pass)
	if !strings.Contains(stderr, "exists already") {
		t.Fatalf("second create: %s", stderr)
	}
	dues(lifecycle.ExitUsage, "actor", "create", "database", "--actor-key-password-file", pass)

	evidence := filepath.Join(t.TempDir(), "evidence.txt")
	if err := os.WriteFile(evidence, []byte("signed evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dues(lifecycle.ExitOK, "store", evidence, "--enrich", "--actor", "jane", "--actor-key-password-file", pass)
	status, _ := dues(lifecycle.ExitOK, "graph", "status")
	m := regexp.MustCompile(`(\d+) commits, (\d+) signed`).FindStringSubmatch(status)
	if m == nil || m[1] == "0" || m[1] != m[2] {
		t.Fatalf("graph status: %s", status)
	}

	// Compacted by the database actor, which attests the image; from then
	// on the store takes signed commits only, which every DUES write is.
	dues(lifecycle.ExitOK, "graph", "compact", "--require-signed")
	status, _ = dues(lifecycle.ExitOK, "graph", "status")
	if !strings.Contains(status, "every commit must be signed") || !strings.Contains(status, `attested   by actor "database"`) {
		t.Fatalf("graph status after --require-signed: %s", status)
	}
	more := filepath.Join(t.TempDir(), "more.txt")
	if err := os.WriteFile(more, []byte("more evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dues(lifecycle.ExitOK, "store", more, "--enrich")

	list, _ := dues(lifecycle.ExitOK, "actor", "list")
	if !regexp.MustCompile(`jane\s+\S+\s+\S+\s+passphrase\s+\S+\s+current`).MatchString(list) {
		t.Fatalf("actor list: %s", list)
	}
	pub := filepath.Join(t.TempDir(), "jane.pub")
	dues(lifecycle.ExitOK, "actor", "export-public", "jane", "--out", pub)
	dues(lifecycle.ExitUsage, "actor", "export-public", "jane", "--out", pub)
	b, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	old, err := signer.ParsePublicKey(b)
	if err != nil || old.Actor != "jane" {
		t.Fatalf("exported key: %+v %v", old, err)
	}

	dues(lifecycle.ExitOK, "actor", "rotate", "jane", "--actor-key-password-file", newPass)
	list, _ = dues(lifecycle.ExitOK, "actor", "list")
	if !strings.Contains(list, signer.FormatID(old.KeyID)) || !strings.Contains(list, "retired") {
		t.Fatalf("after rotate: %s", list)
	}
	stdout, _ := dues(lifecycle.ExitOK, "actor", "export-public", "jane")
	cur, err := signer.ParsePublicKey([]byte(stdout))
	if err != nil || cur.KeyID == old.KeyID {
		t.Fatalf("current key after rotate: %+v %v", cur, err)
	}
	dues(lifecycle.ExitOK, "actor", "revoke", signer.FormatID(cur.KeyID), "--reason", "laptop lost")
	list, _ = dues(lifecycle.ExitOK, "actor", "list")
	if !strings.Contains(list, "laptop lost") {
		t.Fatalf("after revoke: %s", list)
	}
	dues(lifecycle.ExitUsage, "actor", "revoke", "not-an-id")
	_, stderr = dues(lifecycle.ExitFailure, "actor", "export-public", "jane")
	if !strings.Contains(stderr, "revoked") {
		t.Fatalf("export of a revoked actor: %s", stderr)
	}
}

// TestPackageSignatures: an export is signed by the database actor, whose
// exported public key verify and import then trust; another actor's key is
// refused, and an unreadable trusted key is a usage error.
func TestPackageSignatures(t *testing.T) {
	_, logArg := logArgs(t)
	dir := t.TempDir()
	db := filepath.Join(dir, "case")
	dues := func(want int, args ...string) (string, string) {
		t.Helper()
		code := -1
		stdout, stderr := captureOutput(t, func() { code = realMain(append(append([]string{}, logArg...), args...)) })
		if code != want {
			t.Fatalf("dues %q exited %d, want %d\nstdout %s\nstderr %s", args, code, want, stdout, stderr)
		}
		return stdout, stderr
	}
	evidence := filepath.Join(dir, "evidence.txt")
	if err := os.WriteFile(evidence, []byte("packaged evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dues(lifecycle.ExitOK, "--dbpath", db, "store", evidence)

	pkg := filepath.Join(dir, "case.duesarc")
	stdout, _ := dues(lifecycle.ExitOK, "--dbpath", db, "package", "export", "--out", pkg)
	if !strings.Contains(stdout, `signed by actor "database"`) {
		t.Fatalf("export: %s", stdout)
	}
	if stdout, _ = dues(lifecycle.ExitOK, "package", "inspect", pkg); !strings.Contains(stdout, `by actor "database"`) {
		t.Errorf("inspect: %s", stdout)
	}

	pub := filepath.Join(dir, "database.pub")
	dues(lifecycle.ExitOK, "--dbpath", db, "actor", "export-public", "database", "--out", pub)
	stdout, stderr := dues(lifecycle.ExitOK, "package", "verify", pkg, "--trusted-key", pub)
	if !strings.Contains(stdout, "the key is the trusted key") || !strings.Contains(stderr, "unprotected key") {
		t.Errorf("verify with the database's key:\nstdout %s\nstderr %s", stdout, stderr)
	}
	stdout, _ = dues(lifecycle.ExitOK, "package", "verify", pkg)
	if !strings.Contains(stdout, "not checked against a trusted key") {
		t.Errorf("verify without a trusted key: %s", stdout)
	}

	pass := writePass(t, "correct horse")
	jane := filepath.Join(dir, "jane.pub")
	dues(lifecycle.ExitOK, "--dbpath", db, "actor", "create", "jane", "--actor-key-password-file", pass)
	dues(lifecycle.ExitOK, "--dbpath", db, "actor", "export-public", "jane", "--out", jane)
	if _, stderr = dues(lifecycle.ExitFailure, "package", "verify", pkg, "--trusted-key", jane); !strings.Contains(stderr, `the trusted key is actor "jane"'s`) {
		t.Errorf("verify with jane's key: %s", stderr)
	}
	target := filepath.Join(dir, "imported")
	if _, stderr = dues(lifecycle.ExitFailure, "--dbpath", target, "package", "import", pkg, "--trusted-key", jane); !strings.Contains(stderr, "nothing was imported") {
		t.Errorf("import with jane's key: %s", stderr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a refused import left %s", target)
	}
	dues(lifecycle.ExitOK, "--dbpath", target, "package", "import", pkg, "--trusted-key", pub)

	dues(lifecycle.ExitUsage, "package", "verify", pkg, "--trusted-key", filepath.Join(dir, "missing.pub"))
	dues(lifecycle.ExitUsage, "package", "verify", pkg, "--trusted-key", evidence)
}
