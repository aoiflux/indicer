package dbopen

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/graph"
	"indicer/lib/graph/signer"
)

// graphActor opens root's graph writable, as the database's commits are
// written, and returns the actor its handle signs as.
func graphActor(t *testing.T, root string) graph.ActorRef {
	t.Helper()
	g, err := graph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := g.Release(); err != nil {
			t.Fatal(err)
		}
	}()
	return g.Actor()
}

func keyring(t *testing.T, root string) *signer.Keyring {
	t.Helper()
	r, err := signer.NewKeystore(root).Keyring()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestTheFirstGraphWriteMakesTheDatabaseActor: a writable open makes no key
// until its graph opens; then the database actor signs, its key protected
// by the database password, or by nothing on an unencrypted database.
func TestTheFirstGraphWriteMakesTheDatabaseActor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		creds dbmeta.Credentials
		want  signer.Protection
	}{
		{"unencrypted", dbmeta.NoPassword(), signer.ProtectNone},
		{"encrypted", dbmeta.Password("pw"), signer.ProtectDatabase},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := setup(t)
			h := open(t, Request{Path: root, Mode: ReadWrite, Creds: c.creds})
			if _, err := os.Stat(filepath.Join(h.Root, cnst.KEYSDIR)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the open made a keystore: %v", err)
			}
			a := graphActor(t, h.Root)
			if a.Name != signer.DatabaseActor || a.ActorID != signer.ActorID(signer.DatabaseActor) || a.KeyID == 0 {
				t.Fatalf("graph actor %+v", a)
			}
			e, ok := keyring(t, h.Root).Key(a.KeyID)
			if !ok || e.Protection != c.want {
				t.Fatalf("keyring entry %+v (%v), want protection %v", e, ok, c.want)
			}
			p, err := h.Signer()
			if err != nil || p.Actor().KeyID != a.KeyID {
				t.Fatalf("Signer: %v, %v", p, err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			// The next open signs with the same key.
			h = open(t, Request{Path: root, Mode: ReadWrite, Creds: c.creds})
			if again := graphActor(t, h.Root); again.KeyID != a.KeyID {
				t.Fatalf("reopened, key %x, want %x", again.KeyID, a.KeyID)
			}
		})
	}
}

// TestReadOnlyMakesNoActor: a read-only open signs nothing and makes no key;
// it resolves an actor that exists.
func TestReadOnlyMakesNoActor(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	ro := open(t, Request{Path: root, Mode: ReadOnly})
	if _, err := ro.Signer(); !errors.Is(err, signer.ErrNoActor) {
		t.Fatalf("read-only Signer with no actor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ro.Root, cnst.KEYSDIR)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a read-only open made a keystore: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	h = open(t, Request{Path: root, Mode: ReadWrite})
	want := graphActor(t, h.Root)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	ro = open(t, Request{Path: root, Mode: ReadOnly})
	p, err := ro.Signer()
	if err != nil || p.Actor().KeyID != want.KeyID {
		t.Fatalf("read-only Signer: %v, %v", p, err)
	}
}

// TestAnImportedKeyringGetsANewDatabaseKey: a keyring that came with a
// package holds the source's database key without its key file; the first
// graph write makes the database a key of its own and keeps the source's
// for the commits it signed.
func TestAnImportedKeyringGetsANewDatabaseKey(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	source := graphActor(t, h.Root)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(root, cnst.KEYSDIR, "actors")
	if err := os.RemoveAll(keys); err != nil {
		t.Fatal(err)
	}
	h = open(t, Request{Path: root, Mode: ReadWrite})
	a := graphActor(t, h.Root)
	if a.KeyID == source.KeyID || a.ActorID != source.ActorID {
		t.Fatalf("after import: %+v, source %+v", a, source)
	}
	if _, ok := keyring(t, h.Root).Key(source.KeyID); !ok {
		t.Fatal("the source's key left the keyring")
	}
}

// TestAResetDatabaseGetsANewDatabaseKey: a database made again at a root
// whose keystore a reset kept cannot open the old database actor's key; the
// first graph write replaces it, with a warning.
func TestAResetDatabaseGetsANewDatabaseKey(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite, Creds: dbmeta.Password("old")})
	old := graphActor(t, h.Root)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Reset(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, cnst.KEYSDIR, "keyring.json")); err != nil {
		t.Fatalf("reset removed the keyring: %v", err)
	}
	var stderr bytes.Buffer
	console.Configure(console.Out(), &stderr)
	t.Cleanup(console.Reset)
	h = open(t, Request{Path: root, Mode: ReadWrite, Creds: dbmeta.Password("new")})
	a := graphActor(t, h.Root)
	if a.KeyID == old.KeyID {
		t.Fatal("the new database signs with the old one's key")
	}
	if !strings.Contains(stderr.String(), "belongs to an earlier database") {
		t.Fatalf("stderr: %q", stderr.String())
	}
}

// TestANamedActorSigns: Request.Actor signs in place of the database actor,
// with its passphrase; a wrong passphrase, an actor that does not exist and
// the database's own name are refused.
func TestANamedActorSigns(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	want, err := signer.NewKeystore(h.Root).CreateActor("Jane Doe", signer.Passphrase([]byte("correct horse")))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h = open(t, Request{Path: root, Mode: ReadWrite, Actor: &Actor{Name: " jane doe", Passphrase: []byte("correct horse")}})
	if a := graphActor(t, h.Root); a.KeyID != want.KeyID || a.Name != "jane doe" {
		t.Fatalf("graph actor %+v, want %+v", a, want)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		actor Actor
		want  string
	}{
		{Actor{Name: "jane doe", Passphrase: []byte("wrong")}, signer.ErrWrongPassphrase.Error()},
		{Actor{Name: "john", Passphrase: []byte("x")}, "dues actor create"},
	} {
		h = open(t, Request{Path: root, Mode: ReadWrite, Actor: &c.actor})
		_, err := graph.Open(h.Root)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("actor %q: graph open: %v, want %q", c.actor.Name, err, c.want)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	err = openErr(t, Request{Path: root, Mode: ReadWrite, Actor: &Actor{Name: "Database", Passphrase: []byte("x")}})
	if !strings.Contains(err.Error(), "the database's own") {
		t.Fatalf("--actor database: %v", err)
	}
}

// TestCloseEndsSigning: once the database closes, its graph is no longer
// signed by its actor, and a new open may set another.
func TestCloseEndsSigning(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	graphActor(t, h.Root)
	p, err := h.Signer()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Signer().Sign([]byte("x")); err == nil {
		t.Fatal("the closed database's key still signs")
	}
	g, err := graph.Open(h.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Release() }()
	if !g.Actor().Unattributed() {
		t.Fatalf("after close the graph signs as %+v", g.Actor())
	}
}
