package signer

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"

	"github.com/aoiflux/graphene/signing"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunChildScenarios(m, map[string]func(args []string) int{
		// Adds the actor args[1] to the keystore of the root args[0], and
		// stops once the new keyring is in its temporary file, until killed.
		"hold-keyring": func(args []string) int {
			k := NewKeystore(args[0])
			k.written = func(path string) {
				if filepath.Base(path) != KeyringName {
					return
				}
				if err := testutil.NotifyPoint("keyring-written"); err != nil {
					os.Exit(1)
				}
				time.Sleep(time.Minute)
			}
			if _, err := k.CreateActor(args[1], ForDatabase(nil)); err != nil {
				return 1
			}
			return 0
		},
	}))
}

func janePass() Unlock { return Passphrase([]byte("correct horse")).withKDF(testKDF) }

// sign signs data as the actor p resolves, failing the test on error.
func sign(t *testing.T, p Provider, data []byte) []byte {
	t.Helper()
	sig, err := p.Signer().Sign(data)
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func resolve(t *testing.T, k *Keystore, name string, u Unlock) Provider {
	t.Helper()
	p, err := k.Resolve(name, u)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestKeystoreRoundTrip: actors created with each protection resolve to the
// keys the keyring lists, and sign what the verifier accepts.
func TestKeystoreRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	k := NewKeystore(root)
	dbKey := databaseKey(t)
	db, err := k.CreateActor(DatabaseActor, ForDatabase(dbKey))
	if err != nil {
		t.Fatal(err)
	}
	jane, err := k.CreateActor(" Jane Doe ", janePass())
	if err != nil {
		t.Fatal(err)
	}
	if jane.Actor != "jane doe" || jane.ActorID != ActorID("jane doe") || jane.KeyID != KeyID(jane.Public) || jane.Protection != ProtectPassphrase {
		t.Fatalf("jane's entry %+v", jane)
	}
	if db.Protection != ProtectDatabase {
		t.Fatalf("database key protection %s", db.Protection)
	}

	actors, err := k.Actors()
	if err != nil || len(actors) != 2 || actors[0].Name != DatabaseActor || actors[1].Name != "jane doe" {
		t.Fatalf("actors %+v, %v", actors, err)
	}
	for _, e := range []Entry{db, jane} {
		path := k.KeyPath(e.ActorID, e.KeyID)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != keyPerm {
			t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), os.FileMode(keyPerm))
		}
	}

	pd := resolve(t, k, DatabaseActor, ForDatabase(dbKey))
	pj := resolve(t, k, "JANE DOE", Passphrase([]byte("correct horse")))
	if pj.Actor() != jane.Ref() || pd.Actor() != db.Ref() {
		t.Fatalf("resolved %+v and %+v", pj.Actor(), pd.Actor())
	}
	if !pj.Public().Equal(jane.Public) {
		t.Fatal("resolved public key differs from the keyring's")
	}
	msg := []byte("commit 1")
	for _, p := range []Provider{pd, pj} {
		sig := sign(t, p, msg)
		if len(sig) != SignatureSize {
			t.Fatalf("signature is %d bytes", len(sig))
		}
		if err := pd.Verifier().Verify(p.Actor().KeyID, msg, sig); err != nil {
			t.Fatalf("verify %s: %v", p.Actor().Name, err)
		}
	}
}

// TestResolveRefuses: a wrong passphrase, an unknown actor and a missing key
// file are each refused, with an error that says which.
func TestResolveRefuses(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	jane, err := k.CreateActor("jane", janePass())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Resolve("jane", Passphrase([]byte("battery staple"))); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, err := k.Resolve("john", janePass()); !errors.Is(err, ErrNoActor) {
		t.Errorf("unknown actor: %v", err)
	}
	if err := os.Remove(k.KeyPath(jane.ActorID, jane.KeyID)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Resolve("jane", janePass()); !errors.Is(err, ErrNoKey) {
		t.Errorf("missing key file: %v", err)
	}
}

// TestSelfTestCatchesAForeignSeed: a key file labelled for the keyring's key
// but holding another seed fails the self-test, as an unprotected file
// swapped in by hand would.
func TestSelfTestCatchesAForeignSeed(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	e, err := k.CreateActor(DatabaseActor, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	forged, err := sealKey(testSeed(t), e.KeyID, e.ActorID, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k.KeyPath(e.ActorID, e.KeyID), forged, keyPerm); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Resolve(DatabaseActor, ForDatabase(nil)); err == nil || !strings.Contains(err.Error(), "does not hold key") {
		t.Fatalf("foreign seed: %v", err)
	}
	// A file labelled for another key is caught before it is opened.
	other, err := sealKey(testSeed(t), e.KeyID+1, e.ActorID, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k.KeyPath(e.ActorID, e.KeyID), other, keyPerm); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Resolve(DatabaseActor, ForDatabase(nil)); err == nil || !strings.Contains(err.Error(), "is labelled key") {
		t.Fatalf("relabelled file: %v", err)
	}
}

// TestCreateActorRefusesExisting: an actor with a key, under any spelling of
// its name, is not created twice.
func TestCreateActorRefusesExisting(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	if _, err := k.CreateActor("Jane", janePass()); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CreateActor(" jane", janePass()); !errors.Is(err, ErrActorExists) {
		t.Fatalf("second create: %v", err)
	}
	if r, err := k.Keyring(); err != nil || r.Len() != 1 {
		t.Fatalf("keyring holds %d keys, %v", r.Len(), err)
	}
}

// TestRotateAndRevoke: a rotation makes the new key current and keeps the old
// one verifying; revoking the current key leaves the actor unable to sign,
// with the old key not brought back, until it rotates again.
func TestRotateAndRevoke(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	first, err := k.CreateActor("jane", janePass())
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("before the rotation")
	old := sign(t, resolve(t, k, "jane", janePass()), msg)

	second, err := k.Rotate("jane", janePass())
	if err != nil {
		t.Fatal(err)
	}
	p := resolve(t, k, "jane", janePass())
	if p.Actor().KeyID != second.KeyID || second.KeyID == first.KeyID {
		t.Fatalf("current key %s after the rotation, want %s", FormatID(p.Actor().KeyID), FormatID(second.KeyID))
	}
	if err := p.Verifier().Verify(first.KeyID, msg, old); err != nil {
		t.Fatalf("the old key's signature no longer verifies: %v", err)
	}

	revoked, err := k.Revoke(second.KeyID, "laptop lost")
	if err != nil || !revoked.IsRevoked() || revoked.RevokeReason != "laptop lost" {
		t.Fatalf("revoke: %+v, %v", revoked, err)
	}
	if _, err := k.Revoke(second.KeyID, "again"); err == nil {
		t.Fatal("a key was revoked twice")
	}
	if _, err := k.Resolve("jane", janePass()); !errors.Is(err, ErrNoKey) {
		t.Fatalf("resolve after revoking the current key: %v", err)
	}
	r, err := k.Keyring()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Verifier().Verify(first.KeyID, msg, old); err != nil {
		t.Fatalf("a revoked actor's history no longer verifies: %v", err)
	}
	if _, err := k.Rotate("jane", janePass()); err != nil {
		t.Fatal(err)
	}
	resolve(t, k, "jane", janePass())
	if _, err := k.Rotate("john", janePass()); !errors.Is(err, ErrNoActor) {
		t.Fatalf("rotate an unknown actor: %v", err)
	}
	if _, err := k.Revoke(0x1234, ""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("revoke an unknown key: %v", err)
	}
}

// TestVerifierErrors: an unknown key and a forged signature fail with the
// graphene errors that tell them apart.
func TestVerifierErrors(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	if _, err := k.CreateActor(DatabaseActor, ForDatabase(nil)); err != nil {
		t.Fatal(err)
	}
	p := resolve(t, k, DatabaseActor, ForDatabase(nil))
	msg := []byte("commit")
	sig := sign(t, p, msg)
	v := p.Verifier()
	if err := v.Verify(p.Actor().KeyID+1, msg, sig); !errors.Is(err, signing.ErrUnknownKey) {
		t.Errorf("unknown key: %v", err)
	}
	sig[0] ^= 1
	if err := v.Verify(p.Actor().KeyID, msg, sig); !errors.Is(err, signing.ErrBadSignature) {
		t.Errorf("flipped byte: %v", err)
	}
	if err := v.Verify(p.Actor().KeyID, msg, sig[:63]); !errors.Is(err, signing.ErrBadSignature) {
		t.Errorf("short signature: %v", err)
	}
}

// TestClosedKeyCannotSign: Close clears the private key.
func TestClosedKeyCannotSign(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	if _, err := k.CreateActor(DatabaseActor, ForDatabase(nil)); err != nil {
		t.Fatal(err)
	}
	p, err := k.Resolve(DatabaseActor, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	key := p.Signer().(*Key)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if key.priv != nil {
		t.Fatal("the private key is still held")
	}
	if _, err := p.Signer().Sign([]byte("x")); err == nil {
		t.Fatal("a closed key signed")
	}
}

// TestCrashWhileWritingTheKeyring: a process killed with the new keyring in
// its temporary file leaves the old keyring whole. Its actor has a name and
// a key file but no keyring entry, and creating it again finishes the job.
func TestCrashWhileWritingTheKeyring(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	k := NewKeystore(root)
	first, err := k.CreateActor(DatabaseActor, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	child := testutil.StartChild(t, "hold-keyring", root, "jane")
	child.WaitForPoint(t, "keyring-written", 30*time.Second)
	child.Kill(t)
	child.Wait(t, 30*time.Second)

	r, err := k.Keyring()
	if err != nil {
		t.Fatalf("keyring after the crash: %v", err)
	}
	if es := r.Entries(); len(es) != 1 || es[0].KeyID != first.KeyID {
		t.Fatalf("keyring after the crash holds %+v, want the old one", es)
	}
	if actors, err := k.Actors(); err != nil || len(actors) != 2 {
		t.Fatalf("actors after the crash: %+v, %v", actors, err)
	}
	jane, err := k.CreateActor("jane", ForDatabase(nil))
	if err != nil {
		t.Fatalf("create after the crash: %v", err)
	}
	resolve(t, k, "jane", ForDatabase(nil))
	if r, err = k.Keyring(); err != nil || r.Len() != 2 {
		t.Fatalf("keyring holds %d keys, %v", r.Len(), err)
	}
	if _, ok := r.Current(jane.ActorID); !ok {
		t.Fatal("jane has no current key")
	}
	temps, _ := filepath.Glob(filepath.Join(k.Dir(), ".*"))
	if len(temps) != 0 {
		t.Fatalf("temporary files left: %v", temps)
	}
}

// TestKeyIDIsThePublicKeys: the entry's key ID is derived from its public key.
func TestKeyIDIsThePublicKeys(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	e, err := k.CreateActor(DatabaseActor, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Public) != ed25519.PublicKeySize || e.KeyID != KeyID(e.Public) {
		t.Fatalf("entry %+v", e)
	}
}
