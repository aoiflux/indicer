package signer

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

// testKDF is the cheapest cost a key file may ask for, so tests run quickly.
var testKDF = KDF{Time: 1, MemoryKiB: minKDFMemory, Threads: 1}

func testSeed(t *testing.T) []byte {
	t.Helper()
	seed := make([]byte, seedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	return seed
}

func databaseKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// TestKeyFileRoundTrip: a seed sealed under each protection opens with the
// same unlock, and the file has the size the layout says.
func TestKeyFileRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		u    Unlock
		size int
	}{
		{"passphrase", Passphrase([]byte("correct horse")).withKDF(testKDF), 111},
		{"database", ForDatabase(databaseKey(t)), 86},
		{"none", ForDatabase(nil), 58},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seed := testSeed(t)
			b, err := sealKey(seed, 7, 9, tc.u)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) != tc.size {
				t.Fatalf("key file is %d bytes, want %d", len(b), tc.size)
			}
			f, err := parseKeyFile(b)
			if err != nil {
				t.Fatal(err)
			}
			if f.keyID != 7 || f.actorID != 9 || f.protection != tc.u.Protection() {
				t.Fatalf("parsed %+v", f)
			}
			got, err := f.open(tc.u)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, seed) {
				t.Fatal("opened seed differs")
			}
			if tc.u.Protection() != ProtectNone && bytes.Contains(b, seed) {
				t.Fatal("a protected key file holds its seed in the clear")
			}
		})
	}
}

// TestWrongUnlockFails: another passphrase or database key does not open a
// key file, and neither does an unlock of another kind.
func TestWrongUnlockFails(t *testing.T) {
	t.Parallel()
	pass := Passphrase([]byte("correct horse")).withKDF(testKDF)
	b, err := sealKey(testSeed(t), 7, 9, pass)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseKeyFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.open(Passphrase([]byte("battery staple"))); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := f.open(ForDatabase(databaseKey(t))); err == nil || !strings.Contains(err.Error(), "protected by a passphrase") {
		t.Fatalf("database key on a passphrase file: %v", err)
	}

	db, err := sealKey(testSeed(t), 7, 9, ForDatabase(databaseKey(t)))
	if err != nil {
		t.Fatal(err)
	}
	if f, err = parseKeyFile(db); err != nil {
		t.Fatal(err)
	}
	if _, err := f.open(ForDatabase(databaseKey(t))); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("another database's key: %v", err)
	}
	if _, err := f.open(ForDatabase(nil)); err == nil {
		t.Fatal("an unencrypted database's unlock opened a wrapped key")
	}
}

// TestRelabelledKeyFileFails: the header is bound into the encryption, so a
// file given another key ID, actor ID or KDF cost no longer opens.
func TestRelabelledKeyFileFails(t *testing.T) {
	t.Parallel()
	pass := Passphrase([]byte("correct horse")).withKDF(testKDF)
	b, err := sealKey(testSeed(t), 7, 9, pass)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		off  int
	}{
		{"key ID", headerSize - 9},
		{"actor ID", headerSize - 1},
		{"KDF time", headerSize + 3},
		{"salt", headerSize + kdfSize - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := bytes.Clone(b)
			// 2, not 1: the KDF time stays in bounds (1 becomes 3).
			c[tc.off] ^= 2
			f, err := parseKeyFile(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.open(pass); !errors.Is(err, ErrWrongPassphrase) {
				t.Fatalf("relabelled %s opened: %v", tc.name, err)
			}
		})
	}
}

// TestParseKeyFileRefuses: what is not a key file this build writes is
// refused before anything is decrypted.
func TestParseKeyFileRefuses(t *testing.T) {
	t.Parallel()
	good, err := sealKey(testSeed(t), 7, 9, Passphrase([]byte("pw")).withKDF(testKDF))
	if err != nil {
		t.Fatal(err)
	}
	edit := func(off int, v byte) []byte {
		c := bytes.Clone(good)
		c[off] = v
		return c
	}
	for _, tc := range []struct {
		name string
		b    []byte
		want string
	}{
		{"empty", nil, "not a DUES key file"},
		{"magic", edit(0, 'X'), "not a DUES key file"},
		{"version", edit(8, 2), "version 2"},
		{"protection", edit(9, 9), "unknown protection"},
		{"short", good[:len(good)-1], "want 111"},
		{"long", append(bytes.Clone(good), 0), "want 111"},
		{"zero key", append(append(bytes.Clone(good[:10]), make([]byte, 8)...), good[18:]...), "key or actor 0"},
		{"time", edit(headerSize+3, maxKDFTime+1), "outside what DUES writes"},
		{"memory", edit(headerSize, 0xff), "outside what DUES writes"},
		{"threads", edit(headerSize+8, 0), "outside what DUES writes"},
	} {
		if _, err := parseKeyFile(tc.b); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}
