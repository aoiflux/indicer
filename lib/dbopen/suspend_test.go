package dbopen

import (
	"bytes"
	"context"
	"crypto/sha3"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"indicer/internal/testutil/legacyfixture"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/fts"
	"indicer/lib/graph"
	"indicer/lib/graph/signer"
	"indicer/lib/structs"
)

func suspend(t *testing.T, h *Handle) *Suspended {
	t.Helper()
	s, err := h.Suspend()
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	return s
}

func resume(t *testing.T, s *Suspended, opts ResumeOptions) *Handle {
	t.Helper()
	h, err := s.Resume(t.Context(), opts)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	return h
}

// held reports whether another open of root is kept out.
func held(t *testing.T, root string) bool {
	t.Helper()
	err := openErrOrClose(t, Request{Path: root, Mode: ReadOnly})
	return errors.Is(err, ErrInUse)
}

func openErrOrClose(t *testing.T, req Request) error {
	t.Helper()
	h, err := Open(t.Context(), req)
	if err == nil {
		return h.Close()
	}
	return err
}

// TestSuspendHoldsTheRoot: a suspended database is unregistered and keeps
// every other open and snapshot out; its snapshot is the root under the
// suspension's lock, which releasing the snapshot keeps. Resume needs no
// password, gives back the same handle with what was stored, writes again,
// and its graph signs as the database actor; a second Resume, or Release
// after it, does nothing to the open handle.
func TestSuspendHoldsTheRoot(t *testing.T) {
	t.Parallel()
	root := setup(t)
	creds := dbmeta.Password("right")
	h := open(t, Request{Path: root, Mode: ReadWrite, Creds: creds})
	before := bytes.Repeat([]byte("before "), 100)
	kBefore := storeChunk(t, h.DB, before)

	db := h.DB
	s := suspend(t, h)
	if h.DB != nil {
		t.Fatal("a suspended handle still has its database")
	}
	if _, ok := engine.For(db).Layout(); ok {
		t.Error("the suspended database is still registered")
	}
	if !held(t, root) {
		t.Error("a read-only open got past the suspension")
	}
	if _, err := TakeSnapshot(t.Context(), root); !errors.Is(err, ErrInUse) {
		t.Errorf("TakeSnapshot beside a suspension: %v, want %v", err, ErrInUse)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Root != h.Root || snap.Manifest.Encryption.Scheme != h.Manifest.Encryption.Scheme {
		t.Errorf("snapshot of %s %+v, want %s", snap.Root, snap.Manifest.Encryption, h.Root)
	}
	if err := snap.Release(); err != nil || !held(t, root) {
		t.Errorf("releasing the snapshot let go of the root (%v)", err)
	}

	got := resume(t, s, ResumeOptions{})
	if got != h || h.DB == nil {
		t.Fatal("Resume did not reopen the suspended handle")
	}
	if data, err := dbio.GetChonkNode(kBefore, h.DB); err != nil || !bytes.Equal(data, before) {
		t.Fatalf("after Resume: %v", err)
	}
	after := bytes.Repeat([]byte("after "), 100)
	kAfter := storeChunk(t, h.DB, after)
	g, err := graph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	actor := g.Actor().Name
	if err := g.Release(); err != nil {
		t.Fatal(err)
	}
	if actor != signer.DatabaseActor {
		t.Errorf("after Resume the graph signs as %q, want %q", actor, signer.DatabaseActor)
	}

	if _, err := s.Resume(t.Context(), ResumeOptions{}); !errors.Is(err, errNotSuspended) {
		t.Errorf("a second Resume: %v, want %v", err, errNotSuspended)
	}
	if _, err := s.Snapshot(); !errors.Is(err, errNotSuspended) {
		t.Errorf("Snapshot after Resume: %v, want %v", err, errNotSuspended)
	}
	if err := s.Release(); err != nil || h.DB == nil {
		t.Fatalf("Release after Resume closed the handle (%v)", err)
	}

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	ro := open(t, Request{Path: root, Mode: ReadOnly, Creds: creds})
	for k, want := range map[string][]byte{string(kBefore): before, string(kAfter): after} {
		if data, err := dbio.GetChonkNode([]byte(k), ro.DB); err != nil || !bytes.Equal(data, want) {
			t.Errorf("reopened: %v", err)
		}
	}
}

// TestSuspensionEnds: Release, or Close on the suspended handle, zeroes the
// kept keys and lets go of the root; Resume then refuses.
func TestSuspensionEnds(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(*Handle, *Suspended) error{
		"release": func(_ *Handle, s *Suspended) error { return s.Release() },
		"close":   func(h *Handle, _ *Suspended) error { return h.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := setup(t)
			h := open(t, Request{Path: root, Mode: ReadWrite, Creds: dbmeta.Password("right")})
			keys := [][]byte{h.keys.Badger, h.keys.Blob, h.actorKey}
			if slices.ContainsFunc(keys, func(k []byte) bool { return len(k) == 0 }) {
				t.Fatal("a writable open kept no keys")
			}
			s := suspend(t, h)
			if err := end(h, s); err != nil {
				t.Fatal(err)
			}
			for i, k := range keys {
				if slices.ContainsFunc(k, func(b byte) bool { return b != 0 }) {
					t.Errorf("key %d was not zeroed", i)
				}
			}
			if held(t, root) {
				t.Error("the root is still held")
			}
			if _, err := s.Resume(t.Context(), ResumeOptions{}); !errors.Is(err, errNotSuspended) {
				t.Errorf("Resume after the end: %v, want %v", err, errNotSuspended)
			}
			if err := s.Release(); err != nil {
				t.Errorf("a second Release: %v", err)
			}
		})
	}
}

// TestResumeSweeps: Resume removes what a killed writer left, as a writable
// open does, and the part files of uploads unless told to keep them.
func TestResumeSweeps(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	temp := filepath.Join(root, "."+dbmeta.ManifestName+".dues-tmp-1")
	part := filepath.Join(root, cnst.UploadsDir, "0190c0de-0000-7000-8000-000000000000.part")
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}

	s := suspend(t, h)
	write(temp)
	write(part)
	resume(t, s, ResumeOptions{KeepUploads: true})
	if exists(temp) || !exists(part) {
		t.Errorf("keeping uploads: temp file left %v, part file kept %v", exists(temp), exists(part))
	}

	s = suspend(t, h)
	resume(t, s, ResumeOptions{})
	if exists(part) {
		t.Error("Resume kept an upload's part file")
	}
}

// TestResumeRecoversWhenAsked: Resume with Recover fails the ingest left
// pending, as Open with Recover does; without it the ingest stays pending.
func TestResumeRecoversWhenAsked(t *testing.T) {
	t.Parallel()
	h := open(t, Request{Path: setup(t), Mode: ReadWrite})
	id := []byte("crashed-evidence")
	if err := dbio.SetFile(id, structs.NewEvidenceFile("crashed.dd", 0, 128, nil, "dd", ""), h.DB); err != nil {
		t.Fatal(err)
	}
	failed := func() bool {
		t.Helper()
		e, err := dbio.GetEvidenceFile(id, h.DB)
		if err != nil {
			t.Fatal(err)
		}
		return e.Failed
	}
	resume(t, suspend(t, h), ResumeOptions{})
	if failed() {
		t.Fatal("Resume without Recover recovered the pending ingest")
	}
	resume(t, suspend(t, h), ResumeOptions{Recover: true})
	if !failed() {
		t.Fatal("Resume with Recover left the ingest pending")
	}
}

// TestFailedResumeStaysSuspended: a Resume that fails, before badger opens or
// after, leaves the root held and suspended, so it can be tried again.
func TestFailedResumeStaysSuspended(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	s := suspend(t, h)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Resume(ctx, ResumeOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resume with an ended context: %v", err)
	}

	m := h.Manifest
	h.Manifest.Layout.ChunkKiB *= 2
	if _, err := s.Resume(t.Context(), ResumeOptions{}); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("Resume against a changed manifest: %v, want %v", err, ErrMarkerMismatch)
	}
	if h.DB != nil || !held(t, root) {
		t.Fatal("a failed Resume left the database open or the root free")
	}
	if _, err := s.Snapshot(); err != nil {
		t.Fatalf("Snapshot after a failed Resume: %v", err)
	}
	h.Manifest = m
	resume(t, s, ResumeOptions{})
}

// flipCtx is live at its first check and cancelled at every later one, so
// Resume gets past its own check and fails in recovery, with badger open.
type flipCtx struct {
	context.Context
	checks int
}

func (c *flipCtx) Err() error {
	c.checks++
	if c.checks == 1 {
		return nil
	}
	return context.Canceled
}

// TestResumeFailsInRecovery: a Resume that fails once the database is open
// and registered closes it again and stays suspended; a retry recovers.
func TestResumeFailsInRecovery(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})
	id := []byte("crashed-evidence")
	if err := dbio.SetFile(id, structs.NewEvidenceFile("crashed.dd", 0, 128, nil, "dd", ""), h.DB); err != nil {
		t.Fatal(err)
	}
	s := suspend(t, h)
	if _, err := s.Resume(&flipCtx{Context: t.Context()}, ResumeOptions{Recover: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resume cancelled in recovery: %v, want %v", err, context.Canceled)
	}
	if h.DB != nil || !held(t, root) {
		t.Fatal("a Resume that failed in recovery left the database open or the root free")
	}
	resume(t, s, ResumeOptions{Recover: true})
	e, err := dbio.GetEvidenceFile(id, h.DB)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Failed {
		t.Error("the retried Resume left the ingest pending")
	}
}

// TestSuspendRefuses: only an open writable database whose graph and
// full-text index are closed is suspended; a database that did not close
// cleanly gives no snapshot.
func TestSuspendRefuses(t *testing.T) {
	t.Parallel()
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite})

	g, err := graph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Suspend(); err == nil {
		t.Error("suspended with the graph open")
	}
	if err := g.Release(); err != nil {
		t.Fatal(err)
	}
	x, err := fts.Acquire(h.DB, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Suspend(); err == nil {
		t.Error("suspended with the full-text index open")
	}
	if err := x.Release(); err != nil {
		t.Fatal(err)
	}

	s := suspend(t, h)
	if _, err := h.Suspend(); err == nil {
		t.Error("suspended twice")
	}
	mem := filepath.Join(root, cnst.KVDBDIR, "00001.mem")
	if err := os.WriteFile(mem, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(); !errors.Is(err, ErrNeedsRecovery) {
		t.Errorf("Snapshot with memtable logs: %v, want %v", err, ErrNeedsRecovery)
	}
	if err := os.Remove(mem); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(); err != nil {
		t.Fatal(err)
	}

	ro := open(t, Request{Path: root, Mode: ReadOnly})
	if _, err := ro.Suspend(); err == nil {
		t.Error("suspended a read-only open")
	}
}

// TestSuspendedSigner: a suspended database's actor signs without its
// password: the actor its open named, or the database's own, made when the
// keyring has none.
func TestSuspendedSigner(t *testing.T) {
	t.Parallel()
	for name, actor := range map[string]*Actor{
		"database": nil,
		"named":    {Name: "jane", Passphrase: []byte("correct horse")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := open(t, Request{Path: setup(t), Mode: ReadWrite, Creds: dbmeta.Password("right"), Actor: actor})
			named, want := "", signer.DatabaseActor
			if actor != nil {
				if _, err := signer.NewKeystore(h.Root).CreateActor(actor.Name, signer.Passphrase(actor.Passphrase)); err != nil {
					t.Fatal(err)
				}
				named, want = actor.Name, actor.Name
			}
			s := suspend(t, h)
			if got := s.Actor(); got != named {
				t.Errorf("Actor %q, want %q", got, named)
			}
			p, err := s.TakeSigner()
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if got := p.Actor().Name; got != want {
				t.Errorf("signer %q, want %q", got, want)
			}
			if err := s.Release(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.TakeSigner(); !errors.Is(err, errNotSuspended) {
				t.Errorf("TakeSigner after Release: %v, want %v", err, errNotSuspended)
			}
			if got := s.Actor(); got != "" {
				t.Errorf("Actor after Release %q, want none", got)
			}
		})
	}
}

// TestLegacyKeyOutlivesClose: closing a writable open of a legacy database
// zeroes the open's copy of its keys, never the caller's key, which opens it
// again.
func TestLegacyKeyOutlivesClose(t *testing.T) {
	t.Parallel()
	var encrypted legacyfixture.Fixture
	for _, f := range legacyfixture.Load(t) {
		if f.Password != nil {
			encrypted = f
		}
	}
	if encrypted.Mode == "" {
		t.Fatal("no encrypted fixture")
	}
	root := legacyfixture.Extract(t, encrypted)
	key := sha3.Sum256([]byte(*encrypted.Password))
	creds := dbmeta.RawKey(key[:])
	for range 2 {
		if err := open(t, Request{Path: root, Mode: ReadWrite, Creds: creds}).Close(); err != nil {
			t.Fatal(err)
		}
	}
}
