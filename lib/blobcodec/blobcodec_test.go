package blobcodec

import (
	"bytes"
	"crypto/sha3"
	"errors"
	"testing"

	"indicer/lib/cnst"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"
)

// ensureCodecs sets up the process-wide zstd codecs. testdb cannot be used:
// it imports dbio, which imports this package. The decoder is the
// production one: FuzzDecode feeds it crafted frames, and without its
// memory limit one input can take gigabytes.
func ensureCodecs(t testing.TB) {
	t.Helper()
	if cnst.DECODER == nil {
		d, err := cnst.NewDecoder()
		if err != nil {
			t.Fatal(err)
		}
		cnst.DECODER = d
	}
	if cnst.ENCODER == nil {
		e, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		cnst.ENCODER = e
	}
}

func ckeyFor(hash []byte) []byte { return append([]byte(cnst.ChonkNamespace), hash...) }

func blakeKey(data []byte) []byte {
	h := blake3.Sum256(data)
	return ckeyFor(h[:])
}

func sha3Key(data []byte) []byte {
	h := sha3.Sum512(data)
	return ckeyFor(h[:])
}

var blobKey = bytes.Repeat([]byte{7}, 32)

func codecs(t testing.TB) map[string]Codec {
	t.Helper()
	ensureCodecs(t)
	plain, err := NewV2(nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := NewV2(blobKey)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Codec{
		"legacy-sealed": NewLegacySealed(blobKey),
		"legacy-raw":    NewLegacyRaw(),
		"v2":            plain,
		"v2-sealed":     sealed,
	}
}

func TestRoundTrip(t *testing.T) {
	data := bytes.Repeat([]byte("evidence "), 1000)
	for name, c := range codecs(t) {
		for _, ckey := range [][]byte{blakeKey(data), sha3Key(data)} {
			stored, err := c.Encode(ckey, data)
			if err != nil {
				t.Fatalf("%s: encode: %v", name, err)
			}
			got, err := c.Decode(ckey, stored)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s: decode = %d bytes, %v", name, len(got), err)
			}
		}
	}
}

// TestV2RejectsMisplacedPayload is the STO-1 class: a payload read for the
// wrong chunk must be an error, never another chunk's content.
func TestV2RejectsMisplacedPayload(t *testing.T) {
	a, b := []byte("chunk A contents"), []byte("chunk B contents")
	for _, name := range []string{"v2", "v2-sealed"} {
		c := codecs(t)[name]
		storedA, err := c.Encode(blakeKey(a), a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Decode(blakeKey(b), storedA); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s: chunk A decoded as chunk B: %v", name, err)
		}
	}
}

func TestV2RejectsDamage(t *testing.T) {
	data := bytes.Repeat([]byte{1, 2, 3, 4}, 4096)
	for _, name := range []string{"v2", "v2-sealed"} {
		c := codecs(t)[name]
		ckey := blakeKey(data)
		stored, err := c.Encode(ckey, data)
		if err != nil {
			t.Fatal(err)
		}
		cases := map[string][]byte{
			"empty":       nil,
			"truncated":   stored[:len(stored)/2],
			"bad version": append([]byte{9}, stored[1:]...),
			"bad flags":   append([]byte{stored[0], 0x80}, stored[2:]...),
		}
		flipped := append([]byte(nil), stored...)
		flipped[len(flipped)-3] ^= 0x40
		cases["bit flip"] = flipped
		for what, s := range cases {
			if got, err := c.Decode(ckey, s); !errors.Is(err, ErrCorrupt) {
				t.Errorf("%s %s: decode = %d bytes, %v; want ErrCorrupt", name, what, len(got), err)
			}
		}
	}
}

// TestV2SealingMustMatch: an encrypted database refuses an unencrypted chunk
// (a substituted blob) and an unencrypted one refuses an encrypted chunk.
func TestV2SealingMustMatch(t *testing.T) {
	cs := codecs(t)
	data := []byte("some chunk")
	ckey := blakeKey(data)
	plainStored, _ := cs["v2"].Encode(ckey, data)
	sealedStored, _ := cs["v2-sealed"].Encode(ckey, data)
	if _, err := cs["v2-sealed"].Decode(ckey, plainStored); !errors.Is(err, ErrCorrupt) {
		t.Errorf("sealed codec accepted an unsealed chunk: %v", err)
	}
	if _, err := cs["v2"].Decode(ckey, sealedStored); !errors.Is(err, ErrCorrupt) {
		t.Errorf("unsealed codec accepted a sealed chunk: %v", err)
	}
	other, _ := NewV2(bytes.Repeat([]byte{8}, 32))
	if _, err := other.Decode(ckey, sealedStored); !errors.Is(err, ErrCorrupt) {
		t.Errorf("chunk decrypted under another key: %v", err)
	}
}

func TestV2SealedHidesContent(t *testing.T) {
	data := bytes.Repeat([]byte("SECRET-EVIDENCE-"), 64)
	stored, err := codecs(t)["v2-sealed"].Encode(blakeKey(data), data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("SECRET")) {
		t.Fatal("sealed chunk contains plaintext")
	}
}

func TestVerify(t *testing.T) {
	data := []byte("x")
	s256 := sha3.Sum256(data)
	for _, ckey := range [][]byte{blakeKey(data), sha3Key(data), ckeyFor(s256[:]), ckeyFor([]byte("short"))} {
		if err := Verify(ckey, data); err != nil {
			t.Errorf("Verify(%x): %v", ckey, err)
		}
	}
	if err := Verify(blakeKey([]byte("y")), data); !errors.Is(err, ErrCorrupt) {
		t.Errorf("mismatch not reported: %v", err)
	}
}

func TestForFallsBackToGlobals(t *testing.T) {
	prevQuick := cnst.QUICKOPT
	t.Cleanup(func() { cnst.QUICKOPT = prevQuick })
	cnst.QUICKOPT = false
	db, err := badger.Open(badger.DefaultOptions("").WithInMemory(true).WithLogger(nil).
		WithEncryptionKey(blobKey).WithIndexCacheSize(1 << 20))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if got := For(db).Name(); got != LegacySealed {
		t.Fatalf("unregistered default DB uses %s, want %s", got, LegacySealed)
	}
	c, _ := NewV2(nil)
	Register(db, c)
	if For(db).Name() != V2 {
		t.Fatal("registered codec not returned")
	}
	Unregister(db)
	cnst.QUICKOPT = true
	if got := For(db).Name(); got != LegacyRaw {
		t.Fatalf("unregistered quick DB uses %s, want %s", got, LegacyRaw)
	}
}
