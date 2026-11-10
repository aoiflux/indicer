package signer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublicKeyRoundTrip: an exported public key reads back as its keyring
// entry, and an edited one is refused.
func TestPublicKeyRoundTrip(t *testing.T) {
	t.Parallel()
	k := NewKeystore(t.TempDir())
	e, err := k.CreateActor(DatabaseActor, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalPublicKey(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePublicKey(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != e.KeyID || got.ActorID != e.ActorID || got.Actor != e.Actor || !got.Public.Equal(e.Public) || !got.Created.Equal(e.Created) {
		t.Fatalf("read back %+v, wrote %+v", got, e)
	}

	other, err := k.CreateActor("jane", ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func([]byte) []byte{
		"another actor's name": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"actor": "database"`), []byte(`"actor": "jane"`), 1)
		},
		"another key's ID": func(b []byte) []byte {
			return bytes.Replace(b, []byte(FormatID(e.KeyID)), []byte(FormatID(other.KeyID)), 1)
		},
		"an unknown field": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`{`), []byte(`{"extra": 1,`), 1)
		},
		"a second key after it": func(b []byte) []byte { return append(bytes.Clone(b), b...) },
		"another format":        func(b []byte) []byte { return bytes.Replace(b, []byte(publicKeyFormat), []byte("x"), 1) },
		"a capitalised name": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"actor": "database"`), []byte(`"actor": "Database"`), 1)
		},
	} {
		if _, err := ParsePublicKey(edit(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParsePublicKey([]byte(strings.Repeat(" ", maxPublicKeyLen+1))); err == nil {
		t.Error("an oversized file was accepted")
	}
}

// TestReadPublicKeyFile: a key file reads back, and one past the size bound
// is refused before parsing, naming the file.
func TestReadPublicKeyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	e, err := NewKeystore(dir).CreateActor(DatabaseActor, ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalPublicKey(e)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "database.pub")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPublicKeyFile(path); err != nil || got.KeyID != e.KeyID {
		t.Fatalf("ReadPublicKeyFile = %+v, %v", got, err)
	}

	big := filepath.Join(dir, "big.pub")
	if err := os.WriteFile(big, bytes.Repeat([]byte(" "), maxPublicKeyLen+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPublicKeyFile(big); err == nil || !strings.Contains(err.Error(), big) {
		t.Errorf("an oversized key file: %v", err)
	}
	if _, err := ReadPublicKeyFile(filepath.Join(dir, "missing.pub")); err == nil {
		t.Error("a missing key file reads")
	}
}
