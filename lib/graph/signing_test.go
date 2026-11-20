package graph

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/graph/signer"
	"indicer/lib/util"

	graphenedisk "github.com/aoiflux/graphene/disk"
	"github.com/aoiflux/graphene/signing"
)

// signedRoot makes a data root whose graph handles sign as a database actor
// with an unprotected key, until the test ends.
func signedRoot(t *testing.T) (string, signer.Provider) {
	t.Helper()
	cleanRegistry(t)
	root := t.TempDir()
	p := resolveActor(t, root, signer.DatabaseActor)
	remove, err := SignWith(root, func() (signer.Provider, error) { return p, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remove)
	return root, p
}

func resolveActor(t *testing.T, root, name string) signer.Provider {
	t.Helper()
	ks := signer.NewKeystore(root)
	if _, err := ks.CreateActor(name, signer.ForDatabase(nil)); err != nil && !errors.Is(err, signer.ErrActorExists) {
		t.Fatal(err)
	}
	p, err := ks.Resolve(name, signer.ForDatabase(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// written numbers the nodes writeSigned writes, so each commit changes
// something.
var written atomic.Int64

// writeSigned commits n batches through a writer that names no actor.
func writeSigned(t *testing.T, h *Handle, n int) {
	t.Helper()
	for range n {
		w, err := h.Writer(ActorRef{})
		if err != nil {
			t.Fatal(err)
		}
		b := w.Batch()
		v := "n" + strconv.FormatInt(written.Add(1), 10)
		if _, err := b.Upsert(UpsertKey{Key: "evidence_file_id", Value: v}, evidenceLabels(h),
			props.Map{"name": v}, map[string][]byte{"name": []byte(v)}); err != nil {
			t.Fatal(err)
		}
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
}

func openOrFail(t *testing.T, root string, opts OpenOptions) *Handle {
	t.Helper()
	h, err := OpenWith(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func logStatus(t *testing.T, root string) LogStatus {
	t.Helper()
	ls, err := inspectLog(util.GraphPath(root))
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

// TestEveryCommitIsSigned: a handle SignWith covers signs every commit as its
// actor, and writes nothing outside a commit.
func TestEveryCommitIsSigned(t *testing.T) {
	root, p := signedRoot(t)
	h := openOrFail(t, root, OpenOptions{})
	if got, want := h.Actor(), p.Actor(); got.ActorID != want.ActorID || got.KeyID != want.KeyID || got.Name != want.Name {
		t.Fatalf("handle actor %+v, want %+v", got, want)
	}
	writeSigned(t, h, 3)
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	// Reopened, so the open's own records count too.
	h = openOrFail(t, root, OpenOptions{})
	writeSigned(t, h, 1)
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	ls := logStatus(t, root)
	if ls.Commits != 4 || ls.Signed != ls.Commits || ls.Loose != 0 {
		t.Fatalf("log: %d commits, %d signed, %d loose", ls.Commits, ls.Signed, ls.Loose)
	}
	// InspectWAL reads the detail of an unsigned commit only: a signed one's
	// payload is count, checksum, sequence, time, actor, key, then the
	// signature.
	path := filepath.Join(util.GraphPath(root), logFileName)
	info, err := graphenedisk.InspectWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range info.Records {
		if r.TypeName != "batch-commit" {
			continue
		}
		payload := log[r.Offset+walRecordHeader:]
		actor, key := binary.LittleEndian.Uint64(payload[24:32]), binary.LittleEndian.Uint64(payload[32:40])
		if want := p.Actor(); actor != want.ActorID || key != want.KeyID {
			t.Fatalf("commit at %d by actor %d key %d, want %+v", r.Offset, actor, key, want)
		}
	}
}

// TestWriterRefusesAnotherActor: a signing handle refuses a writer naming
// someone else, whose commits it would sign with the wrong key.
func TestWriterRefusesAnotherActor(t *testing.T) {
	root, p := signedRoot(t)
	h := openOrFail(t, root, OpenOptions{})
	defer func() { _ = h.Release() }()
	ref := p.Actor()
	if _, err := h.Writer(ActorRef{Name: "jane", ActorID: signer.ActorID("jane"), KeyID: 9}); err == nil {
		t.Fatal("a writer for another actor was let through")
	}
	w, err := h.Writer(ActorRef{Name: ref.Name, ActorID: ref.ActorID, KeyID: ref.KeyID})
	if err != nil {
		t.Fatalf("the handle's own actor: %v", err)
	}
	w.Close()
}

// TestSignWithRefusesASecondActor: one actor per graph in a process, set
// before the graph opens.
func TestSignWithRefusesASecondActor(t *testing.T) {
	root, _ := signedRoot(t)
	none := func() (signer.Provider, error) { return nil, errors.New("unused") }
	if _, err := SignWith(root, none); err == nil {
		t.Fatal("a second actor for one graph was let through")
	}
	other := t.TempDir()
	h := openOrFail(t, other, OpenOptions{})
	defer func() { _ = h.Release() }()
	if _, err := SignWith(other, none); err == nil {
		t.Fatal("an actor set while its graph is open was let through")
	}
}

// TestActorResolveErrorFailsTheOpen: a writable open whose actor cannot be
// resolved does not fall back to unsigned commits.
func TestActorResolveErrorFailsTheOpen(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	remove, err := SignWith(root, func() (signer.Provider, error) { return nil, errors.New("no key here") })
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	if h, err := Open(root); err == nil || !strings.Contains(err.Error(), "no key here") {
		_ = h.Release()
		t.Fatalf("open: %v", err)
	}
	// A read-only open does not sign, so it does not resolve an actor.
	if h, err := OpenWith(root, OpenOptions{ReadOnly: true}); err != nil && !errors.Is(err, ErrNoGraph) {
		t.Fatalf("read-only open: %v", err)
	} else if err == nil {
		_ = h.Release()
	}
}

// lastCommit returns the offset and payload length of the log's last batch
// commit record.
func lastCommit(t *testing.T, path string) (int64, uint32) {
	t.Helper()
	info, err := graphenedisk.InspectWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(info.Records) - 1; i >= 0; i-- {
		if r := info.Records[i]; r.TypeName == "batch-commit" {
			return r.Offset, r.Length
		}
	}
	t.Fatal("the log holds no commit")
	return 0, 0
}

// walRecordHeader is a log record's type byte and length.
const walRecordHeader = 1 + 4

// reframe rewrites the checksum of the record at off with payload length n,
// as graphene's v2 framing computes it, so a change to its payload reads as
// that change and not as a torn record.
func reframe(b []byte, off int64, n uint32) {
	end := off + walRecordHeader + int64(n)
	binary.LittleEndian.PutUint32(b[end:], crc32.ChecksumIEEE(b[off:end]))
}

// TestFlippedSignatureFailsTheOpen: a commit whose signature does not verify
// under the keyring fails every open, read-only ones included.
func TestFlippedSignatureFailsTheOpen(t *testing.T) {
	root, _ := signedRoot(t)
	h := openOrFail(t, root, OpenOptions{})
	writeSigned(t, h, 2)
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(util.GraphPath(root), logFileName)
	off, n := lastCommit(t, path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[off+walRecordHeader+int64(n)-1] ^= 1 // the signature's last byte
	reframe(b, off, n)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ro := range []bool{true, false} {
		h, err := OpenWith(root, OpenOptions{ReadOnly: ro})
		if !errors.Is(err, signing.ErrBadSignature) {
			if err == nil {
				_ = h.Release()
			}
			t.Fatalf("read-only %v: open: %v, want a bad signature", ro, err)
		}
	}
}

// captureErr collects what the console writes to stderr until the test ends.
func captureErr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	console.Configure(console.Out(), &buf)
	t.Cleanup(console.Reset)
	return &buf
}

// TestUnknownKeyWarns: a commit signed by a key the keyring does not hold,
// such as one from another database, is a warning; with signed commits
// required, it fails the open.
func TestUnknownKeyWarns(t *testing.T) {
	root, _ := signedRoot(t)
	h := openOrFail(t, root, OpenOptions{})
	writeSigned(t, h, 2)
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(root, cnst.KEYSDIR)
	if err := os.Rename(keys, keys+".aside"); err != nil {
		t.Fatal(err)
	}
	stderr := captureErr(t)
	h = openOrFail(t, root, OpenOptions{ReadOnly: true})
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "2 commits signed by keys its keyring does not hold") {
		t.Fatalf("stderr: %q", stderr.String())
	}

	setRequired(t, root)
	if h, err := OpenWith(root, OpenOptions{ReadOnly: true}); !errors.Is(err, signing.ErrUnknownKey) {
		if err == nil {
			_ = h.Release()
		}
		t.Fatalf("required, unknown key: %v", err)
	}
}

func setRequired(t *testing.T, root string) {
	t.Helper()
	dir := util.GraphPath(root)
	m, ok, err := schema.ReadMarker(dir)
	if err != nil || !ok {
		t.Fatalf("marker: %v %v", ok, err)
	}
	m.Signing.Required = true
	if err := schema.WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
}

// TestUnsignedLogOpens: a store written before signing, with no keyring,
// opens and takes signed commits after its unsigned ones.
func TestUnsignedLogOpens(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	h := openOrFail(t, root, OpenOptions{})
	writeSigned(t, h, 2) // no SignWith: unattributed and unsigned
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if ls := logStatus(t, root); ls.Commits != 2 || ls.Signed != 0 {
		t.Fatalf("unsigned log: %+v", ls)
	}
	p := resolveActor(t, root, signer.DatabaseActor)
	remove, err := SignWith(root, func() (signer.Provider, error) { return p, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	h = openOrFail(t, root, OpenOptions{})
	writeSigned(t, h, 1)
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if ls := logStatus(t, root); ls.Commits != 3 || ls.Signed != 1 {
		t.Fatalf("mixed log: %+v", ls)
	}
}

// TestRequiredRefusesUnsigned: with signed commits required, an unsigned
// commit, a record outside any commit, and an open with no actor to sign are
// each refused.
func TestRequiredRefusesUnsigned(t *testing.T) {
	t.Run("unsigned commit", func(t *testing.T) {
		cleanRegistry(t)
		root := t.TempDir()
		h := openOrFail(t, root, OpenOptions{})
		writeSigned(t, h, 1)
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
		setRequired(t, root)
		if h, err := OpenWith(root, OpenOptions{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "1 unsigned commits") {
			if err == nil {
				_ = h.Release()
			}
			t.Fatalf("open: %v", err)
		}
	})
	t.Run("bare record", func(t *testing.T) {
		root, _ := signedRoot(t)
		h := openOrFail(t, root, OpenOptions{})
		writeSigned(t, h, 1)
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
		setRequired(t, root)
		appendBareRecord(t, filepath.Join(util.GraphPath(root), logFileName))
		if ls := logStatus(t, root); ls.Loose != 1 {
			t.Fatalf("the bare record is not loose: %+v", ls)
		}
		if h, err := OpenWith(root, OpenOptions{ReadOnly: true}); err == nil || !strings.Contains(err.Error(), "1 records outside any commit") {
			if err == nil {
				_ = h.Release()
			}
			t.Fatalf("open: %v", err)
		}
	})
	t.Run("no actor", func(t *testing.T) {
		root, _ := signedRoot(t)
		h := openOrFail(t, root, OpenOptions{})
		writeSigned(t, h, 1)
		if err := RequireSigned(h); err != nil {
			t.Fatal(err)
		}
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
		// Signed by its actor: it still opens, writable and read-only.
		h = openOrFail(t, root, OpenOptions{})
		writeSigned(t, h, 1)
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
		h = openOrFail(t, root, OpenOptions{ReadOnly: true})
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
		cleanSignings(t, root)
		if h, err := Open(root); err == nil || !strings.Contains(err.Error(), "no actor to sign") {
			if err == nil {
				_ = h.Release()
			}
			t.Fatalf("writable open with no actor: %v", err)
		}
	})
}

// cleanSignings forgets the actor set for root.
func cleanSignings(t *testing.T, root string) {
	t.Helper()
	key, err := registryKey(util.GraphPath(root))
	if err != nil {
		t.Fatal(err)
	}
	signMu.Lock()
	delete(signings, key)
	signMu.Unlock()
}

// appendBareRecord appends a node record outside any batch, as graphene's
// AddNode writes one: no commit frames it, so no signature covers it.
func appendBareRecord(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	payload := bytes.Repeat([]byte{0}, 24)
	rec := make([]byte, walRecordHeader+len(payload)+4)
	rec[0] = 0x01
	binary.LittleEndian.PutUint32(rec[1:5], uint32(len(payload)))
	copy(rec[walRecordHeader:], payload)
	reframe(rec, 0, uint32(len(payload)))
	if _, err := f.Write(rec); err != nil {
		t.Fatal(err)
	}
}

// TestRequireSignedRefusesAnUnsignedLog: the marker is set only when the log
// holds nothing a signature does not cover.
func TestRequireSignedRefusesAnUnsignedLog(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	h := openOrFail(t, root, OpenOptions{})
	writeSigned(t, h, 1)
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	p := resolveActor(t, root, signer.DatabaseActor)
	remove, err := SignWith(root, func() (signer.Provider, error) { return p, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	h = openOrFail(t, root, OpenOptions{})
	defer func() { _ = h.Release() }()
	if err := RequireSigned(h); err == nil || !strings.Contains(err.Error(), "1 unsigned commits") {
		t.Fatalf("require signed: %v", err)
	}
	// Compacted, the unsigned commit is in a segment and the log is empty.
	if _, err := Compact(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if err := RequireSigned(h); err != nil {
		t.Fatal(err)
	}
	m, _, err := schema.ReadMarker(util.GraphPath(root))
	if err != nil || !m.Signing.Required {
		t.Fatalf("marker: %+v %v", m.Signing, err)
	}
}

// TestSignedCompaction: a compaction by a signing handle attests its image as
// the actor, and the retired segment keeps the signed commits.
func TestSignedCompaction(t *testing.T) {
	root, p := signedRoot(t)
	h := openOrFail(t, root, OpenOptions{})
	writeSigned(t, h, 3)
	if _, err := Compact(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	st, ok := h.Graph().GraphStore.(*graphenedisk.Store)
	if !ok {
		t.Fatalf("store is %T", h.Graph().GraphStore)
	}
	att, err := st.SnapshotAttestation()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if att.ActorID != p.Actor().ActorID || att.KeyID != p.Actor().KeyID {
		t.Fatalf("attested by actor %d key %d, want %+v", att.ActorID, att.KeyID, p.Actor())
	}
	if err := graphenedisk.VerifyAttestation(p.Verifier(), att); err != nil {
		t.Fatalf("attestation: %v", err)
	}

	segs, err := graphenedisk.ListSegments(util.GraphPath(root))
	if err != nil {
		t.Fatal(err)
	}
	signed := 0
	for _, s := range segs {
		info, err := graphenedisk.InspectWAL(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range info.Records {
			if r.TypeName == "batch-commit" && r.Length >= signedCommitPayload {
				signed++
			}
		}
	}
	if signed < 3 {
		t.Fatalf("segments hold %d signed commits, want at least 3", signed)
	}
	// It reopens, its image checked against nothing but its own log.
	h = openOrFail(t, root, OpenOptions{ReadOnly: true})
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
}
