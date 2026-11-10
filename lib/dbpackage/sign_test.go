package dbpackage

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"indicer/internal/duesarc"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/graph/signer"
)

func verify(t *testing.T, path string, opts VerifyOptions) (VerifyResult, error) {
	t.Helper()
	p, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	return p.Verify(t.Context(), opts)
}

func isSignatureError(err error, want string) bool {
	se, ok := errors.AsType[*SignatureError](err)
	return ok && strings.Contains(se.Reason, want)
}

func currentDatabaseKey(t *testing.T, root string) signer.Entry {
	t.Helper()
	ring, err := signer.NewKeystore(root).Keyring()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := ring.Current(signer.ActorID(signer.DatabaseActor))
	if !ok {
		t.Fatalf("%s has no database actor key: %+v", root, ring.Entries())
	}
	return e
}

// otherDatabaseActor is a database actor of another root: a key and keyring
// that whoever changes a package could sign it with.
func otherDatabaseActor(t *testing.T) (signer.Provider, []byte) {
	t.Helper()
	root := t.TempDir()
	ks := signer.NewKeystore(root)
	if _, err := ks.CreateActor(signer.DatabaseActor, signer.ForDatabase(nil)); err != nil {
		t.Fatal(err)
	}
	p, err := ks.Resolve(signer.DatabaseActor, signer.ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ring, err := os.ReadFile(filepath.Join(root, cnst.KEYSDIR, "keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p, ring
}

// resign rewrites the package at src as whoever holds prov could: with
// keyring, when set, in place of the packaged one, the manifest's hashes
// made to match, and the manifest signed by prov.
func resign(t *testing.T, src string, prov signer.Provider, keyring []byte) string {
	t.Helper()
	return rewrite(t, src, func(name string, data []byte) ([]byte, bool) {
		switch name {
		case duesarc.DatabaseDir + "/" + keyringPath:
			if keyring != nil {
				return keyring, true
			}
		case duesarc.ManifestName:
			m, err := duesarc.ParseDatabaseManifest(data)
			if err != nil {
				t.Fatal(err)
			}
			if keyring != nil {
				i := slices.IndexFunc(m.Files, func(f duesarc.File) bool { return f.Path == keyringPath })
				sum := sha256.Sum256(keyring)
				m.Files[i].Size, m.Files[i].SHA256 = int64(len(keyring)), hex.EncodeToString(sum[:])
				m.ContentSHA256 = duesarc.ContentHash(m.Files)
			}
			if err := sign(&m, prov); err != nil {
				t.Fatal(err)
			}
			out, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return out, true
		}
		return data, true
	}, nil)
}

// editManifest rewrites the package at src with its manifest, as JSON,
// changed by edit.
func editManifest(t *testing.T, src string, edit func(m map[string]any)) string {
	t.Helper()
	return rewrite(t, src, func(name string, data []byte) ([]byte, bool) {
		if name != duesarc.ManifestName {
			return data, true
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out, true
	}, nil)
}

// set sets the field at a dotted path of a decoded JSON object.
func set(m map[string]any, path string, v any) {
	keys := strings.Split(path, ".")
	for _, k := range keys[:len(keys)-1] {
		m = m[k].(map[string]any)
	}
	m[keys[len(keys)-1]] = v
}

// TestExportSignsWithTheDatabaseActor: an export makes the database actor
// the first time, signs with it, and verify checks that signature, against
// a trusted key when given one.
func TestExportSignsWithTheDatabaseActor(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	res := export(t, ExportOptions{Path: root})
	cur := currentDatabaseKey(t, root)
	s := res.Manifest.Signature
	if s == nil || s.Actor != signer.DatabaseActor || s.KeyID != signer.FormatID(cur.KeyID) || s.Protection != "none" {
		t.Fatalf("signature %+v; want the database actor's key %s", s, signer.FormatID(cur.KeyID))
	}

	vr, err := verify(t, res.Output, VerifyOptions{})
	if err != nil || vr.Signature == nil || vr.Signature.Trusted || vr.Signature.Protection != signer.ProtectNone {
		t.Fatalf("verify: %+v, %v", vr.Signature, err)
	}
	vr, err = verify(t, res.Output, VerifyOptions{TrustedKey: &cur})
	if err != nil || !vr.Signature.Trusted {
		t.Fatalf("verify with the trusted key: %+v, %v", vr.Signature, err)
	}

	export(t, ExportOptions{Path: root})
	if ring, _ := signer.NewKeystore(root).Keyring(); ring.Len() != 1 {
		t.Errorf("a second export made another key: %+v", ring.Entries())
	}
}

// TestSignatureCoversTheStatement: a change to any field the statement
// holds breaks the signature, and import refuses the package.
func TestSignatureCoversTheStatement(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	other, _ := otherDatabaseActor(t)
	flipped := func(m map[string]any) {
		sig, _ := base64.StdEncoding.DecodeString(m["signature"].(map[string]any)["signature"].(string))
		sig[0] ^= 1
		set(m, "signature.signature", sig)
	}
	for name, edit := range map[string]func(map[string]any){
		"created_at_utc":       func(m map[string]any) { set(m, "created_at_utc", "2000-01-01T00:00:00Z") },
		"pipeline":             func(m map[string]any) { set(m, "pipeline", "merge") },
		"stats":                func(m map[string]any) { set(m, "stats.evidence", 9) },
		"format version":       func(m map[string]any) { set(m, "database.format_version", 99) },
		"encryption":           func(m map[string]any) { set(m, "database.encryption", "aes-256-gcm") },
		"chunk size":           func(m map[string]any) { set(m, "database.layout.chunk_kib", 1) },
		"graph schema":         func(m map[string]any) { set(m, "database.components.graph_schema", 2) },
		"key protection":       func(m map[string]any) { set(m, "signature.key_protection", "passphrase") },
		"signature byte":       flipped,
		"actor":                func(m map[string]any) { set(m, "signature.actor", "mallory") },
		"public key":           func(m map[string]any) { set(m, "signature.public_key", []byte(other.Public())) },
		"scheme":               func(m map[string]any) { set(m, "signature.scheme", "rsa") },
		"directories":          func(m map[string]any) { set(m, "dirs", []string{}) },
		"plaintext components": func(m map[string]any) { set(m, "database.plaintext_components", []string{"fts"}) },
	} {
		_, err := verify(t, editManifest(t, res.Output, edit), VerifyOptions{})
		if _, ok := errors.AsType[*SignatureError](err); !ok {
			if _, damaged := errors.AsType[*DamagedError](err); !damaged || name != "directories" {
				t.Errorf("%s changed: %v; want a signature error", name, err)
			}
		}
	}

	target := filepath.Join(t.TempDir(), "imported")
	changed := editManifest(t, res.Output, func(m map[string]any) { set(m, "created_at_utc", "2000-01-01T00:00:00Z") })
	if _, err := Import(t.Context(), ImportOptions{Package: changed, Path: target}); !isSignatureError(err, "does not verify") {
		t.Errorf("import of a changed manifest: %v", err)
	}
	noStage(t, target, false)
}

// TestTrustedKeyIsTheOnlyAnchor: whoever changes a package can sign it again
// with a key and keyring of their own, which verify accepts unless given
// the trusted key; a key the package's own keyring does not hold, or holds
// as revoked, fails; and a trusted key asks for a signature.
func TestTrustedKeyIsTheOnlyAnchor(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	res := export(t, ExportOptions{Path: root})
	trusted := currentDatabaseKey(t, root)
	other, otherRing := otherDatabaseActor(t)

	forged := resign(t, res.Output, other, otherRing)
	if vr, err := verify(t, forged, VerifyOptions{}); err != nil || vr.Signature.KeyID == trusted.KeyID {
		t.Fatalf("a package signed again with another keyring: %+v, %v", vr.Signature, err)
	}
	if _, err := verify(t, forged, VerifyOptions{TrustedKey: &trusted}); !isSignatureError(err, "after a rotation") {
		t.Errorf("the same package under the trusted key: %v", err)
	}
	target := filepath.Join(t.TempDir(), "imported")
	if _, err := Import(t.Context(), ImportOptions{Package: forged, Path: target, TrustedKey: &trusted}); !isSignatureError(err, "the trusted key") {
		t.Errorf("import under the trusted key: %v", err)
	}
	noStage(t, target, false)

	if _, err := verify(t, resign(t, res.Output, other, nil), VerifyOptions{}); !isSignatureError(err, "not in the package's keyring") {
		t.Errorf("signed with a key the keyring lacks: %v", err)
	}

	stripped := rewrite(t, res.Output, func(name string, data []byte) ([]byte, bool) {
		if name == duesarc.ManifestName {
			data = unsigned(t, data)
		}
		return data, true
	}, nil)
	if vr, err := verify(t, stripped, VerifyOptions{}); err != nil || vr.Signature != nil {
		t.Errorf("an unsigned package: %+v, %v", vr.Signature, err)
	}
	if _, err := verify(t, stripped, VerifyOptions{TrustedKey: &trusted}); !isSignatureError(err, "unsigned") {
		t.Errorf("an unsigned package under the trusted key: %v", err)
	}

	ks := signer.NewKeystore(root)
	key, err := ks.Resolve(signer.DatabaseActor, signer.ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	if _, err := ks.Revoke(trusted.KeyID, "laptop lost"); err != nil {
		t.Fatal(err)
	}
	revokedRing, err := os.ReadFile(filepath.Join(root, cnst.KEYSDIR, "keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify(t, resign(t, res.Output, key, revokedRing), VerifyOptions{TrustedKey: &trusted}); !isSignatureError(err, "laptop lost") {
		t.Errorf("signed with a revoked key: %v", err)
	}
}

// TestExportSigningOfAnEncryptedDatabase: without the password the database
// actor cannot sign, so the package is unsigned with a notice; with it, the
// key the password protects signs; a named actor signs without it, and one
// whose passphrase is wrong fails the export.
func TestExportSigningOfAnEncryptedDatabase(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.Password("right"))
	res := export(t, ExportOptions{Path: root})
	if res.Manifest.Signature != nil || !slices.ContainsFunc(res.Notices, func(n string) bool { return strings.Contains(n, "unsigned") }) {
		t.Errorf("without the password: signature %+v, notices %q", res.Manifest.Signature, res.Notices)
	}

	res = export(t, ExportOptions{Path: root, Creds: dbmeta.Password("right")})
	if s := res.Manifest.Signature; s == nil || s.Actor != signer.DatabaseActor || s.Protection != "database" {
		t.Errorf("with the password: signature %+v", s)
	}

	pass := []byte("correct horse")
	if _, err := signer.NewKeystore(root).CreateActor("jane", signer.Passphrase(pass)); err != nil {
		t.Fatal(err)
	}
	res = export(t, ExportOptions{Path: root, Actor: &dbopen.Actor{Name: "jane", Passphrase: pass}})
	if s := res.Manifest.Signature; s == nil || s.Actor != "jane" || s.Protection != "passphrase" {
		t.Errorf("by actor jane: signature %+v", s)
	}
	if vr, err := verify(t, res.Output, VerifyOptions{}); err != nil || vr.Signature.Actor != "jane" {
		t.Errorf("verify of jane's package: %+v, %v", vr.Signature, err)
	}

	out := filepath.Join(t.TempDir(), "wrong.duesarc")
	_, err := Export(t.Context(), ExportOptions{Path: root, Output: out, Actor: &dbopen.Actor{Name: "jane", Passphrase: []byte("wrong")}})
	if err == nil || !strings.Contains(err.Error(), "cannot sign") {
		t.Errorf("a wrong passphrase: %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed export left %s", out)
	}
}

// TestPackageHoldsNoPrivateKey: a package that lists an actor's key file is
// damaged, so verify and import refuse it.
func TestPackageHoldsNoPrivateKey(t *testing.T) {
	t.Parallel()
	res := export(t, ExportOptions{Path: newRoot(t, dbmeta.NoPassword())})
	key := cnst.KEYSDIR + "/actors/" + testKeyFile
	data := "a private key"
	sum := sha256.Sum256([]byte(data))
	withKey := rewrite(t, res.Output, func(name string, b []byte) ([]byte, bool) {
		if name != duesarc.ManifestName {
			return b, true
		}
		m, err := duesarc.ParseDatabaseManifest(b)
		if err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, duesarc.File{Path: key, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
		slices.SortFunc(m.Files, func(a, b duesarc.File) int { return strings.Compare(a.Path, b.Path) })
		m.ContentSHA256, m.Signature = duesarc.ContentHash(m.Files), nil
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out, true
	}, map[string]string{duesarc.DatabaseDir + "/" + key: data})

	entry := duesarc.DatabaseDir + "/" + key
	if _, err := verify(t, withKey, VerifyOptions{}); !isDamaged(err, entry) {
		t.Errorf("verify: %v", err)
	}
	target := filepath.Join(t.TempDir(), "imported")
	if _, err := Import(t.Context(), ImportOptions{Package: withKey, Path: target}); !isDamaged(err, entry) {
		t.Errorf("import: %v", err)
	}
	noStage(t, target, false)
}

// TestImportedDatabaseGetsItsOwnActor: an imported package brings the source
// database's public keys but no private one, so the new database's first
// signer is a key of its own, and the source's key stays to check what it
// signed.
func TestImportedDatabaseGetsItsOwnActor(t *testing.T) {
	t.Parallel()
	src := newRoot(t, dbmeta.NoPassword())
	res := export(t, ExportOptions{Path: src})
	source := currentDatabaseKey(t, src)
	target := filepath.Join(t.TempDir(), "imported")
	ir, err := Import(t.Context(), ImportOptions{Package: res.Output, Path: target, TrustedKey: &source})
	if err != nil || ir.Signature == nil || !ir.Signature.Trusted {
		t.Fatalf("import: %+v, %v", ir.Signature, err)
	}

	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: target, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.Signer()
	if err != nil {
		t.Fatal(err)
	}
	if p.Actor().KeyID == source.KeyID {
		t.Error("the imported database signs with the source's key")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	ring, err := signer.NewKeystore(target).Keyring()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ring.Key(source.KeyID); !ok || ring.Len() != 2 {
		t.Errorf("keyring after import: %+v", ring.Entries())
	}
}
