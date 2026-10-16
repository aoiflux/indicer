package dbmeta

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"indicer/internal/testutil/legacyfixture"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/klauspost/compress/zstd"
)

// fastKDF keeps tests quick; production uses DefaultKDF.
var fastKDF = KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}

func validManifest() Manifest {
	return Manifest{
		Format: FormatName, Version: FormatVersion, Created: time.Unix(0, 0).UTC(),
		Layout: Layout{ChunkKiB: 256, ChunkHash: HashBLAKE3, Storage: StorageFile,
			BlobEncoding: blobcodec.V2, ValueEncoding: EncodingZstd, BadgerCompression: EncodingZstd},
		Encryption: Encryption{Scheme: SchemeNone},
	}
}

func TestValidate(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatal(err)
	}
	enc, _, err := NewEncryption(Password("pw"), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Manifest){
		"format":          func(m *Manifest) { m.Format = "other" },
		"newer":           func(m *Manifest) { m.Version = FormatVersion + 1 },
		"chunk":           func(m *Manifest) { m.Layout.ChunkKiB = 3 },
		"hash":            func(m *Manifest) { m.Layout.ChunkHash = "md5" },
		"storage":         func(m *Manifest) { m.Layout.Storage = "cloud" },
		"none with seal":  func(m *Manifest) { m.Layout.BlobEncoding = blobcodec.LegacySealed },
		"argon2 legacy":   func(m *Manifest) { m.Encryption = enc; m.Layout.BlobEncoding = blobcodec.LegacySealed },
		"argon2 no check": func(m *Manifest) { m.Encryption = enc; m.Encryption.Check = nil },
		"sha3 v2":         func(m *Manifest) { m.Encryption = Encryption{Scheme: SchemeSHA3Legacy} },
		"raw values only": func(m *Manifest) { m.Layout.ValueEncoding = EncodingRaw },
		"scheme":          func(m *Manifest) { m.Encryption.Scheme = "rot13" },
	}
	for name, mutate := range bad {
		m := validManifest()
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	m := validManifest()
	m.Version = FormatVersion + 1
	if err := m.Validate(); !errors.Is(err, ErrNewerFormat) {
		t.Errorf("newer format: %v", err)
	}
}

func TestSaveLoadAndMirror(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, cnst.KVDBDIR), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("empty root: %v", err)
	}
	want := validManifest()
	if err := Save(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(root)
	if err != nil || !SameLayout(got, want) {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	// The mirror stands in for a lost or damaged root manifest.
	if err := os.WriteFile(ManifestPath(root), []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = Load(root); err != nil || !SameLayout(got, want) {
		t.Fatalf("Load with damaged manifest = %+v, %v", got, err)
	}
	if err := os.Remove(ManifestPath(root)); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(root); err != nil {
		t.Fatalf("Load from mirror only: %v", err)
	}
}

func TestKeys(t *testing.T) {
	enc, keys, err := NewEncryption(Password("correct horse"), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	if enc.Scheme != SchemeArgon2id || len(keys.Badger) != 32 || len(keys.Blob) != 32 || bytes.Equal(keys.Badger, keys.Blob) {
		t.Fatalf("encryption %+v keys %x %x", enc, keys.Badger, keys.Blob)
	}
	again, err := DeriveKeys(enc, Password("correct horse"))
	if err != nil || !bytes.Equal(again.Badger, keys.Badger) || !bytes.Equal(again.Blob, keys.Blob) {
		t.Fatalf("re-derived keys differ: %v", err)
	}
	if _, err := DeriveKeys(enc, Password("wrong")); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := DeriveKeys(enc, NoPassword()); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("no password: %v", err)
	}
	// A second database with the same password gets different keys (salt).
	enc2, keys2, _ := NewEncryption(Password("correct horse"), fastKDF)
	if bytes.Equal(enc2.KDF.Salt, enc.KDF.Salt) || bytes.Equal(keys2.Badger, keys.Badger) {
		t.Fatal("salt not random")
	}
	// No password: unencrypted (D8).
	if enc, keys, err := NewEncryption(NoPassword(), fastKDF); err != nil || enc.Scheme != SchemeNone || keys.Badger != nil {
		t.Fatalf("no-password database: %+v %v", enc, err)
	}
	// The manifest holds no secret.
	m := validManifest()
	m.Encryption = enc
	m.Layout.BlobEncoding = blobcodec.V2
	b, _ := m.Marshal()
	if bytes.Contains(b, keys.Badger) || bytes.Contains(b, []byte("correct horse")) {
		t.Fatal("manifest leaks a secret")
	}
}

func ensureCodecs(t *testing.T) {
	t.Helper()
	if cnst.DECODER == nil {
		d, err := zstd.NewReader(nil)
		if err != nil {
			t.Fatal(err)
		}
		cnst.DECODER = d
	}
}

func openLegacy(t *testing.T, root string, key []byte, raw bool) *badger.DB {
	t.Helper()
	opts := badger.DefaultOptions(filepath.Join(root, cnst.KVDBDIR)).WithLogger(nil).
		WithReadOnly(true).WithBypassLockGuard(true).WithBlockCacheSize(16 << 20).WithIndexCacheSize(16 << 20)
	if !raw {
		opts = opts.WithEncryptionKey(key).WithCompression(options.ZSTD)
	}
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestLegacyFixtures infers the manifest of every legacy database fixture and
// checks it against the flags the fixture was written with.
func TestLegacyFixtures(t *testing.T) {
	ensureCodecs(t)
	for _, f := range legacyfixture.Load(t) {
		t.Run(f.Mode, func(t *testing.T) {
			root := legacyfixture.Extract(t, f)
			if !HasDatabase(root) {
				t.Fatal("HasDatabase = false")
			}
			flags := f.Flags
			quick := slices.Contains(flags, "--quick")
			pw := ""
			if f.Password != nil {
				pw = *f.Password
			}

			enc, base, err := LegacyEncryption(filepath.Join(root, cnst.KVDBDIR))
			if err != nil {
				t.Fatal(err)
			}
			wantScheme := SchemeSHA3Legacy
			if quick {
				wantScheme = SchemeNone // CFG-2: quick mode stored plaintext even with a password
			}
			if enc.Scheme != wantScheme {
				t.Fatalf("scheme %s, want %s", enc.Scheme, wantScheme)
			}

			keys, err := DeriveKeys(enc, Password(pw))
			if err != nil {
				t.Fatal(err)
			}
			kvdb := filepath.Join(root, cnst.KVDBDIR)
			if err := CheckLegacyKey(kvdb, keys.Badger); err != nil {
				t.Fatalf("right key rejected: %v", err)
			}
			if enc.Scheme == SchemeSHA3Legacy {
				wrong, _ := DeriveKeys(enc, Password(pw+"x"))
				if err := CheckLegacyKey(kvdb, wrong.Badger); !errors.Is(err, ErrWrongPassword) {
					t.Fatalf("wrong key: %v", err)
				}
			}

			db := openLegacy(t, root, keys.Badger, quick)
			l, inf := InferLayout(db, root, base)

			wantKiB := 256
			if i := slices.Index(flags, "--chonksize"); i >= 0 {
				wantKiB = 64
			}
			wantHash := HashBLAKE3
			if slices.Contains(flags, "sha3") {
				wantHash = HashSHA3
			}
			wantStorage := StorageFile
			if slices.Contains(flags, "--container") {
				wantStorage = StorageContainer
				if slices.Contains(flags, "--hierarchical") {
					wantStorage = StorageContainerBIDX
				}
			}
			// The E01 fixture's store failed after its first chunk, so no
			// offset shows the chunk size.
			if f.Mode == "e01" {
				wantKiB = 0
			}
			if l.ChunkKiB != wantKiB || l.ChunkHash != wantHash || l.Storage != wantStorage {
				t.Fatalf("inferred %d KiB %q %q (unverified %v, notes %v); want %d KiB %q %q",
					l.ChunkKiB, l.ChunkHash, l.Storage, inf.Unverified, inf.Notes, wantKiB, wantHash, wantStorage)
			}
			if wantUnverified := wantKiB == 0; wantUnverified != slices.Contains(inf.Unverified, "chunk_kib") || len(inf.Unverified) > 1 {
				t.Fatalf("unverified %v", inf.Unverified)
			}
			m := Manifest{Format: FormatName, Version: FormatVersion, Layout: l, Encryption: enc, Inferred: &inf}
			if f.Mode != "e01" {
				if err := m.Validate(); err != nil {
					t.Fatalf("inferred manifest invalid: %v", err)
				}
			}
		})
	}
}
