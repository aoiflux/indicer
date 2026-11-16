package dbpackage

import (
	"crypto/sha256"
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

// suspended opens root for writing, to sign as actor (nil: the database's
// own), counts its evidence, and suspends it.
func suspended(t *testing.T, root string, creds dbmeta.Credentials, actor *dbopen.Actor) (*dbopen.Handle, *dbopen.Suspended, *duesarc.Stats) {
	t.Helper()
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite, Creds: creds, KDF: &fastKDF, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	stats, err := evidenceStats(t.Context(), h.DB)
	if err != nil {
		t.Fatal(err)
	}
	s, err := h.Suspend()
	if err != nil {
		t.Fatal(err)
	}
	return h, s, stats
}

func fileSHA256(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

// TestExportHeld: a suspended database is packaged under its own lock,
// without its password, into its exports folder, which the package leaves
// out. The database actor signs, the stats are the ones given, the tee sees
// every byte, and the database resumes after.
func TestExportHeld(t *testing.T) {
	t.Parallel()
	for name, creds := range map[string]dbmeta.Credentials{"plain": dbmeta.NoPassword(), "encrypted": dbmeta.Password("right")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := newRoot(t, creds)
			h, s, stats := suspended(t, root, creds, nil)
			old := filepath.Join(root, cnst.ExportsDir, "old.duesarc")
			if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(old, []byte("an earlier export"), 0o600); err != nil {
				t.Fatal(err)
			}

			tee := sha256.New()
			res := export(t, ExportOptions{Held: s, Stats: stats, Tee: tee, Output: filepath.Join(root, cnst.ExportsDir, "case.duesarc")})
			m := res.Manifest
			if slices.ContainsFunc(paths(m), func(p string) bool { return strings.HasPrefix(p, cnst.ExportsDir+"/") }) || slices.Contains(m.Dirs, cnst.ExportsDir) {
				t.Errorf("the package holds the exports folder: %v", m.Dirs)
			}
			if sum := fileSHA256(t, res.Output); !slices.Equal(tee.Sum(nil), sum[:]) {
				t.Error("the tee saw other bytes than the package's")
			}
			if s := m.Signature; s == nil || s.Actor != signer.DatabaseActor {
				t.Errorf("signature %+v, want the database actor's", s)
			}
			wantStats := stats
			if m.Database.Encrypted {
				wantStats = nil
			}
			if (m.Stats == nil) != (wantStats == nil) || m.Stats != nil && *m.Stats != *wantStats {
				t.Errorf("stats %+v, want %+v", m.Stats, wantStats)
			}
			if vr, err := verify(t, res.Output, VerifyOptions{}); err != nil || vr.Signature == nil {
				t.Fatalf("verify: %+v, %v", vr, err)
			}

			if _, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadOnly, Creds: creds}); !errors.Is(err, dbopen.ErrInUse) {
				t.Errorf("after the export: %v, want the root still held", err)
			}
			if _, err := s.Resume(t.Context(), dbopen.ResumeOptions{}); err != nil {
				t.Fatal(err)
			}
			if h.DB == nil {
				t.Fatal("the database did not resume")
			}
		})
	}
}

// TestExportHeldRefuses: Held names the database alone, and only while it
// is suspended.
func TestExportHeldRefuses(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	_, s, _ := suspended(t, root, dbmeta.NoPassword(), nil)
	out := filepath.Join(t.TempDir(), "x.duesarc")
	if _, err := Export(t.Context(), ExportOptions{Held: s, Path: root, Output: out}); err == nil {
		t.Error("exported with both Path and Held")
	}
	if err := s.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(t.Context(), ExportOptions{Held: s, Output: out}); err == nil {
		t.Error("exported a released database")
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused export left %s", out)
	}
}

// TestExportHeldNamedActor: a held export signs as the actor it names, with
// the stats it is given, and fails when that actor cannot sign.
func TestExportHeldNamedActor(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	pass := []byte("correct horse")
	if _, err := signer.NewKeystore(root).CreateActor("jane", signer.Passphrase(pass)); err != nil {
		t.Fatal(err)
	}
	_, s, stats := suspended(t, root, dbmeta.NoPassword(), nil)

	out := filepath.Join(t.TempDir(), "wrong.duesarc")
	_, err := Export(t.Context(), ExportOptions{Held: s, Stats: stats, Output: out, Actor: &dbopen.Actor{Name: "jane", Passphrase: []byte("wrong")}})
	if err == nil || !strings.Contains(err.Error(), "cannot sign") {
		t.Errorf("a wrong passphrase: %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed export left %s", out)
	}

	res := export(t, ExportOptions{Held: s, Stats: stats, Actor: &dbopen.Actor{Name: "jane", Passphrase: pass}})
	m := res.Manifest
	if m.Signature == nil || m.Signature.Actor != "jane" {
		t.Errorf("signature %+v, want jane's", m.Signature)
	}
	if m.Stats == nil || *m.Stats != *stats {
		t.Errorf("stats %+v, want %+v", m.Stats, stats)
	}
}

// TestExportHeldUnsigned: a held export whose open named an actor that
// cannot sign fails; one the database actor cannot sign is unsigned, with a
// notice.
func TestExportHeldUnsigned(t *testing.T) {
	t.Parallel()
	t.Run("named", func(t *testing.T) {
		t.Parallel()
		root := newRoot(t, dbmeta.NoPassword())
		_, s, stats := suspended(t, root, dbmeta.NoPassword(), &dbopen.Actor{Name: "ghost", Passphrase: []byte("boo")})
		out := filepath.Join(t.TempDir(), "x.duesarc")
		_, err := Export(t.Context(), ExportOptions{Held: s, Stats: stats, Output: out})
		if err == nil || !strings.Contains(err.Error(), "actor \"ghost\" cannot sign") {
			t.Errorf("an actor not in the keystore: %v", err)
		}
		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a failed export left %s", out)
		}
	})
	t.Run("database", func(t *testing.T) {
		t.Parallel()
		root := newRoot(t, dbmeta.NoPassword())
		keys := filepath.Join(root, cnst.KEYSDIR)
		if err := os.RemoveAll(keys); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keys, []byte("not a keystore"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, s, stats := suspended(t, root, dbmeta.NoPassword(), nil)
		res := export(t, ExportOptions{Held: s, Stats: stats})
		if res.Manifest.Signature != nil || !slices.ContainsFunc(res.Notices, func(n string) bool { return strings.Contains(n, "the package is unsigned") }) {
			t.Errorf("signature %+v, notices %q", res.Manifest.Signature, res.Notices)
		}
	})
}

// TestExportIntoTheExportsFolder: a package may go into the root's exports
// folder, which later packages leave out; elsewhere in the root it may not,
// nor into a folder that only looks like it.
func TestExportIntoTheExportsFolder(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	exports := filepath.Join(root, cnst.ExportsDir)
	if err := os.Mkdir(exports, 0o700); err != nil {
		t.Fatal(err)
	}
	first := export(t, ExportOptions{Path: root, Output: filepath.Join(exports, "first.duesarc")})
	second := export(t, ExportOptions{Path: root, Output: filepath.Join(exports, "second.duesarc")})
	if first.Manifest.ContentSHA256 != second.Manifest.ContentSHA256 {
		t.Error("the first package changed what the second holds")
	}
	for _, out := range []string{
		filepath.Join(root, cnst.BLOBSDIR, "x.duesarc"),
		exports,
		exports + Ext,
		filepath.Join(root, cnst.ExportsDir+"-old", "x.duesarc"),
	} {
		if _, err := Export(t.Context(), ExportOptions{Path: root, Output: out}); !isUsage(err) {
			t.Errorf("into %s: %v, want a usage error", out, err)
		}
	}

	// Packages leave out only a folder spelled exactly so.
	other := newRoot(t, dbmeta.NoPassword())
	upper := filepath.Join(other, strings.ToUpper(cnst.ExportsDir[:1])+cnst.ExportsDir[1:])
	if err := os.Mkdir(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(t.Context(), ExportOptions{Path: other, Output: filepath.Join(upper, "x.duesarc")}); !isUsage(err) {
		t.Errorf("into %s: %v, want a usage error", upper, err)
	}
}
