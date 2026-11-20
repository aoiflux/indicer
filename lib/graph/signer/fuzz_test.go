package signer

import (
	"bytes"
	"crypto/ed25519"
	"testing"
	"time"
)

// FuzzParseKeyFile: any input parses or is refused without a panic, and a
// parsed file's parts cover it exactly.
func FuzzParseKeyFile(f *testing.F) {
	seed := bytes.Repeat([]byte{7}, seedSize)
	for _, u := range []Unlock{
		Passphrase([]byte("pw")).withKDF(testKDF),
		ForDatabase(bytes.Repeat([]byte{1}, 32)),
		ForDatabase(nil),
	} {
		b, err := sealKey(seed, 7, 9, u)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(keyMagic))
	f.Fuzz(func(t *testing.T, b []byte) {
		kf, err := parseKeyFile(b)
		if err != nil {
			return
		}
		if len(kf.aad)+len(kf.nonce)+len(kf.body) != len(b) || !bytes.HasPrefix(b, kf.aad) {
			t.Fatalf("parts of %d bytes cover %d+%d+%d", len(b), len(kf.aad), len(kf.nonce), len(kf.body))
		}
		want := sealedSize
		if kf.protection == ProtectNone {
			want = seedSize
		}
		if len(kf.body) != want || kf.keyID == 0 || kf.actorID == 0 {
			t.Fatalf("parsed %+v", kf)
		}
		if kf.protection == ProtectNone {
			if got, err := kf.open(ForDatabase(nil)); err != nil || !bytes.Equal(got, kf.body) {
				t.Fatalf("unprotected open: %v", err)
			}
		}
	})
}

// FuzzKeyring: any keyring.json or actors.json parses or is refused without
// a panic, and a keyring that parses writes back to one that parses the same.
func FuzzKeyring(f *testing.F) {
	db := ed25519.PublicKey(bytes.Repeat([]byte{3}, ed25519.PublicKeySize))
	jane := ed25519.PublicKey(bytes.Repeat([]byte{4}, ed25519.PublicKeySize))
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	r := &Keyring{}
	for _, e := range []Entry{
		{KeyID: KeyID(db), ActorID: ActorID(DatabaseActor), Actor: DatabaseActor, Public: db, Protection: ProtectDatabase, Created: at},
		{KeyID: KeyID(jane), ActorID: ActorID("jane"), Actor: "jane", Public: jane, Protection: ProtectPassphrase, Created: at, Revoked: at, RevokeReason: "lost"},
	} {
		if err := r.add(e); err != nil {
			f.Fatal(err)
		}
	}
	b, err := r.Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	actors, err := marshalActors([]Actor{{Name: DatabaseActor, ID: ActorID(DatabaseActor), Created: at}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(actors)
	f.Add([]byte(`{"version":1,"keys":[]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseActors(b)
		r, err := ParseKeyring(b)
		if err != nil {
			return
		}
		out, err := r.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		again, err := ParseKeyring(out)
		if err != nil {
			t.Fatalf("a parsed keyring writes one that does not parse: %v\n%s", err, out)
		}
		a, z := r.Entries(), again.Entries()
		if len(a) != len(z) {
			t.Fatalf("%d keys, then %d", len(a), len(z))
		}
		for i := range a {
			x, y := a[i], z[i]
			if x.KeyID != y.KeyID || x.ActorID != y.ActorID || x.Actor != y.Actor || !x.Public.Equal(y.Public) ||
				x.Protection != y.Protection || !x.Created.Equal(y.Created) || !x.Revoked.Equal(y.Revoked) ||
				x.RevokeReason != y.RevokeReason {
				t.Fatalf("key %d: %+v, then %+v", i, x, y)
			}
		}
	})
}
