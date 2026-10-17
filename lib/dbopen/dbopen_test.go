package dbopen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil/legacyfixture"
	"indicer/internal/testutil/testdb"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"

	"github.com/dgraph-io/badger/v4"
	"github.com/zeebo/blake3"
)

var fastKDF = dbmeta.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}

func setup(t *testing.T) string {
	t.Helper()
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	return filepath.Join(t.TempDir(), "case")
}

func open(t *testing.T, req Request) *Handle {
	t.Helper()
	if req.KDF == nil {
		req.KDF = &fastKDF
	}
	h, err := Open(context.Background(), req)
	if err != nil {
		t.Fatalf("Open(%+v): %v", req.Mode, err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func openErr(t *testing.T, req Request) error {
	t.Helper()
	if req.KDF == nil {
		req.KDF = &fastKDF
	}
	h, err := Open(context.Background(), req)
	if err == nil {
		h.Close()
		t.Fatalf("Open(%+v) succeeded", req.Mode)
	}
	return err
}

// storeChunk writes one chunk the way ingest does and returns its key.
func storeChunk(t *testing.T, db *badger.DB, data []byte) []byte {
	t.Helper()
	h := blake3.Sum256(data)
	ckey := append([]byte(cnst.ChonkNamespace), h[:]...)
	meta, err := dbio.MaterializeChonkNode(ckey, data, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := dbio.SetNode(ckey, meta, db); err != nil {
		t.Fatal(err)
	}
	return ckey
}

func blobsContain(t *testing.T, root string, needle []byte) bool {
	t.Helper()
	found := false
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, needle) {
				found = true
			}
		}
		return nil
	})
	return found
}

var secret = bytes.Repeat([]byte("PLAINTEXT-EVIDENCE-MARKER "), 4096)

func TestCreateUnencrypted(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	if !h.Created || h.Manifest.Encrypted() || h.Manifest.Layout.BlobEncoding != blobcodec.V2 {
		t.Fatalf("created %v manifest %+v", h.Created, h.Manifest)
	}
	if len(h.Notices) != 1 || !strings.Contains(h.Notices[0], "without encryption") {
		t.Fatalf("notices %q; want the D8 notice", h.Notices)
	}
	for _, p := range []string{dbmeta.ManifestPath(root), dbmeta.MirrorPath(root)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	ckey := storeChunk(t, h.DB, secret)
	got, err := dbio.GetChonkNode(ckey, h.DB)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// A read-only reopen sees the same database and changes nothing.
	before, _ := os.ReadFile(dbmeta.ManifestPath(root))
	ro := open(t, Request{Path: root, Mode: ReadOnly})
	if !ro.ReadOnly || ro.Created {
		t.Fatalf("read-only %v created %v", ro.ReadOnly, ro.Created)
	}
	if got, err := dbio.GetChonkNode(ckey, ro.DB); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("read-only read: %v", err)
	}
	if err := ro.DB.Update(func(txn *badger.Txn) error { return txn.Set([]byte("x"), nil) }); err == nil {
		t.Fatal("read-only database accepted a write")
	}
	after, _ := os.ReadFile(dbmeta.ManifestPath(root))
	if !bytes.Equal(before, after) {
		t.Fatal("read-only open rewrote the manifest")
	}
}

func TestCreateEncrypted(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite, Creds: dbmeta.Password("s3cret")})
	if h.Manifest.Encryption.Scheme != dbmeta.SchemeArgon2id || len(h.Notices) != 0 {
		t.Fatalf("manifest %+v notices %q", h.Manifest.Encryption, h.Notices)
	}
	ckey := storeChunk(t, h.DB, secret)
	h.Close()
	if blobsContain(t, root, []byte("PLAINTEXT-EVIDENCE-MARKER")) {
		t.Fatal("plaintext found in the encrypted database")
	}

	for _, c := range []struct {
		creds dbmeta.Credentials
		want  string
	}{
		{dbmeta.NoPassword(), "pass --password"},
		{dbmeta.Password("wrong"), "wrong password"},
		{dbmeta.Password(""), "wrong password"},
	} {
		err := openErr(t, Request{Path: root, Mode: ReadOnly, Creds: c.creds})
		if !strings.Contains(err.Error(), c.want) || lifecycle.ExitCode(err) != lifecycle.ExitUsage {
			t.Errorf("error %q (exit %d); want %q, exit 2", err, lifecycle.ExitCode(err), c.want)
		}
	}
	ro := open(t, Request{Path: root, Mode: ReadOnly, Creds: dbmeta.Password("s3cret")})
	if got, err := dbio.GetChonkNode(ckey, ro.DB); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("read back: %v", err)
	}
}

// TestQuickNeverDropsEncryption is the CFG-2 regression: --quick with a
// password stored plaintext.
func TestQuickNeverDropsEncryption(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite, Creds: dbmeta.Password("pw"), Layout: Requested{Quick: true}})
	if !h.Manifest.Encrypted() || h.Manifest.Layout.BadgerCompression != dbmeta.EncodingNone {
		t.Fatalf("quick database: %+v", h.Manifest)
	}
	storeChunk(t, h.DB, secret)
	if err := h.DB.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte("E|||:probe"), secret)
	}); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if blobsContain(t, root, []byte("PLAINTEXT-EVIDENCE-MARKER")) {
		t.Fatal("quick mode stored plaintext")
	}
}

func TestReadOnlyNeverCreates(t *testing.T) {
	root := setup(t)
	err := openErr(t, Request{Path: root, Mode: ReadOnly})
	if !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("error %v; want ErrNoDatabase", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only open created the data root")
	}
	empty := t.TempDir()
	if err := openErr(t, Request{Path: empty, Mode: ReadOnly}); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("empty dir: %v", err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Fatalf("read-only open wrote %v", entries)
	}
}

func TestLayoutConflicts(t *testing.T) {
	root := setup(t)
	open(t, Request{Path: root, Mode: ReadWrite, Layout: Requested{ChunkKiB: 64}}).Close()

	// Defaults adopt the database's layout.
	cnst.ChonkSize = 256 * cnst.KB
	h := open(t, Request{Path: root, Mode: ReadOnly, Layout: Requested{ChunkKiB: 256}})
	if cnst.ChonkSize != 64*cnst.KB {
		t.Fatalf("ChonkSize %d after open; want the database's 64 KiB", cnst.ChonkSize)
	}
	h.Close()

	for name, r := range map[string]Requested{
		"chunk":     {ChunkKiB: 256, Set: map[string]bool{cnst.FlagChonkSize: true}},
		"hash":      {ChunkHash: cnst.SHA3, Set: map[string]bool{cnst.FlagHashAlgo: true}},
		"container": {Container: true, Set: map[string]bool{cnst.FlagContainerMode: true}},
	} {
		err := openErr(t, Request{Path: root, Mode: ReadOnly, Layout: r})
		if lifecycle.ExitCode(err) != lifecycle.ExitUsage {
			t.Errorf("%s: %v (exit %d); want a usage error", name, err, lifecycle.ExitCode(err))
		}
	}
}

func TestRootLock(t *testing.T) {
	root := setup(t)
	w := open(t, Request{Path: root, Mode: ReadWrite})
	if err := openErr(t, Request{Path: root, Mode: ReadWrite}); !errors.Is(err, ErrInUse) {
		t.Errorf("second writer: %v", err)
	}
	if err := openErr(t, Request{Path: root, Mode: ReadOnly}); !errors.Is(err, ErrInUse) {
		t.Errorf("reader beside a writer: %v", err)
	}
	w.Close()

	r1 := open(t, Request{Path: root, Mode: ReadOnly})
	r2 := open(t, Request{Path: root, Mode: ReadOnly})
	if err := openErr(t, Request{Path: root, Mode: ReadWrite}); !errors.Is(err, ErrInUse) {
		t.Errorf("writer beside readers: %v", err)
	}
	r1.Close()
	r2.Close()
	open(t, Request{Path: root, Mode: ReadWrite}).Close()
}

func TestMarkerDetectsEditedManifest(t *testing.T) {
	root := setup(t)
	open(t, Request{Path: root, Mode: ReadWrite}).Close()
	m, err := dbmeta.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Layout.ChunkKiB = 64
	b, _ := m.Marshal()
	for _, p := range []string{dbmeta.ManifestPath(root), dbmeta.MirrorPath(root)} {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := openErr(t, Request{Path: root, Mode: ReadOnly}); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("edited manifest: %v", err)
	}
}

func TestNewerFormatRefused(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	m := h.Manifest
	m.Version = dbmeta.FormatVersion + 1
	b, _ := json.Marshal(m)
	if err := h.DB.Update(func(txn *badger.Txn) error { return txn.Set([]byte(dbmeta.MarkerKey), b) }); err != nil {
		t.Fatal(err)
	}
	h.Close()
	if err := openErr(t, Request{Path: root, Mode: ReadOnly}); !errors.Is(err, dbmeta.ErrNewerFormat) {
		t.Fatalf("newer marker: %v", err)
	}
}

// readSomeChunks decodes the chunks behind every relation (the fixtures are
// small), which proves the codec and keys chosen for a legacy database are
// right. Container fixtures hold a second session's chunks at offsets that
// point into the first session's (STO-1); those fail to decode, and only
// those may.
func readSomeChunks(t *testing.T, db *badger.DB, root string, container bool) int {
	t.Helper()
	var chashes [][]byte
	db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.RelationNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			v, _ := it.Item().ValueCopy(nil)
			if !cnst.QUICKOPT {
				v, _ = cnst.DECODER.DecodeAll(v, nil)
			}
			chashes = append(chashes, v)
		}
		return nil
	})
	ok := 0
	for _, c := range chashes {
		ckey := append([]byte(cnst.ChonkNamespace), c...)
		data, err := dbio.GetChonkNode(ckey, db)
		if err == nil {
			err = blobcodec.Verify(ckey, data)
		}
		switch {
		case err == nil:
			ok++
		case container && errors.Is(err, blobcodec.ErrCorrupt):
			t.Logf("chunk %x: %v (known defect STO-1)", c, err)
		default:
			t.Fatalf("chunk %x: %v", c, err)
		}
	}
	if len(chashes) > 0 && ok == 0 {
		t.Fatal("no chunk decoded")
	}
	return ok
}

func TestLegacyFixtures(t *testing.T) {
	for _, f := range legacyfixture.Load(t) {
		t.Run(f.Mode, func(t *testing.T) {
			setup(t)
			root := legacyfixture.Extract(t, f)
			creds := dbmeta.NoPassword()
			if f.Password != nil {
				creds = dbmeta.Password(*f.Password)
			}

			ro := open(t, Request{Path: root, Mode: ReadOnly, Creds: creds})
			if ro.Manifest.Inferred == nil {
				t.Fatal("legacy manifest not marked as inferred")
			}
			wantKiB := int64(256)
			if slices.Contains(f.Flags, "64") {
				wantKiB = 64
			}
			if cnst.ChonkSize != wantKiB*cnst.KB {
				t.Fatalf("ChonkSize %d, want %d KiB", cnst.ChonkSize, wantKiB)
			}
			container := slices.Contains(f.Flags, "--container")
			if n := readSomeChunks(t, ro.DB, root, container); n == 0 && f.Mode != "e01" {
				t.Fatal("no relations to read")
			}
			ro.Close()
			if _, err := os.Stat(dbmeta.ManifestPath(root)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("read-only open wrote a manifest")
			}

			// A writable open records the inferred manifest and the marker;
			// later opens use them.
			rw := open(t, Request{Path: root, Mode: ReadWrite, Creds: creds})
			rw.Close()
			m, err := dbmeta.Load(root)
			if err != nil || m.Inferred == nil {
				t.Fatalf("persisted manifest: %+v, %v", m, err)
			}
			again := open(t, Request{Path: root, Mode: ReadOnly, Creds: creds})
			readSomeChunks(t, again.DB, root, container)
		})
	}
}
