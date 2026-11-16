package graph

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/util"

	graphenedb "github.com/aoiflux/graphene"
	graphenedisk "github.com/aoiflux/graphene/disk"
)

// cleanRegistry fails the test if a previous case leaked a handle, and clears
// the registry so cases cannot influence each other through it.
func cleanRegistry(t *testing.T) {
	t.Helper()
	if n := openHandleCount(); n != 0 {
		t.Fatalf("registry not empty at start of test: %d handle(s) leaked", n)
	}
	t.Cleanup(func() {
		if n := openHandleCount(); n != 0 {
			registryMu.Lock()
			for key, h := range registry {
				t.Errorf("leaked handle %s with %d reference(s)", key, h.refs)
				_ = h.graph.Close()
				_ = h.lock.release()
				delete(registry, key)
			}
			registryMu.Unlock()
		}
	})
}

// TestOpen_SameDirectoryReturnsSameHandle is the core of the two-writer fix:
// nested opens must be the same store, not two views of one directory.
func TestOpen_SameDirectoryReturnsSameHandle(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	outer, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Release()

	inner, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Release()

	if outer != inner {
		t.Error("nested Open returned a different *Handle")
	}
	if outer.Graph() != inner.Graph() {
		t.Error("nested Open returned a different underlying *graphene.Graph")
	}
	if got := openHandleCount(); got != 1 {
		t.Errorf("open store count = %d, want 1", got)
	}
	if got := refCountFor(outer); got != 2 {
		t.Errorf("reference count = %d, want 2", got)
	}
}

// ResolveDataRoot maps <root>/kvdb back to <root>, so two spellings of the same
// data root must not produce two stores.
func TestOpen_EquivalentRootsShareOneHandle(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	viaRoot, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer viaRoot.Release()

	viaKVDB, err := Open(filepath.Join(root, cnst.KVDBDIR))
	if err != nil {
		t.Fatal(err)
	}
	defer viaKVDB.Release()

	if viaRoot != viaKVDB {
		t.Errorf("%q and %q produced different handles; both resolve to %q",
			root, filepath.Join(root, cnst.KVDBDIR), util.GraphPath(root))
	}
}

func TestRelease_ClosesOnceAtZero(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if first.Graph() == nil {
		t.Fatal("store closed while a reference was still outstanding")
	}
	if got := openHandleCount(); got != 1 {
		t.Errorf("open store count after partial release = %d, want 1", got)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if got := openHandleCount(); got != 0 {
		t.Errorf("open store count after final release = %d, want 0", got)
	}
	if first.Graph() != nil {
		t.Error("Graph() still returns a store after the final Release")
	}
}

func TestRelease_UnbalancedIsReported(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(); err == nil {
		t.Error("a second Release returned nil; an unbalanced release must be reported, " +
			"not silently close a store another caller reopened")
	}
}

// Reopening after the last Release must work: commands run in sequence.
func TestOpen_AfterFullReleaseReopens(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(root)
	if err != nil {
		t.Fatalf("reopening after full release: %v", err)
	}
	defer second.Release()
	if second.Graph() == nil {
		t.Error("reopened handle has no store")
	}
}

// TestLock_ContendedDirectoryIsRefused exercises the advisory lock directly.
//
// Handle.Open dedupes in-process, so the lock is only reachable this way. Both
// flock and LockFileEx scope a lock to the open file description rather than to
// the process, so a second acquisition contends here exactly as it would from a
// second dues invocation.
func TestLock_ContendedDirectoryIsRefused(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "graph")

	held, err := acquireLock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()

	_, err = acquireLock(dir, false)
	if err == nil {
		t.Fatal("a second exclusive lock on the same directory succeeded")
	}

	var locked *ErrGraphLocked
	if !errors.As(err, &locked) {
		t.Fatalf("got %v (%T), want *ErrGraphLocked", err, err)
	}
	if locked.Holder == nil {
		t.Error("lock error does not name the holding process")
	} else if locked.Holder.PID == 0 {
		t.Error("lock error names pid 0")
	}
	if msg := locked.Error(); msg == "" {
		t.Error("empty lock error message")
	}
}

func TestLock_ReleasedLockCanBeRetaken(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "graph")

	first, err := acquireLock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}

	second, err := acquireLock(dir, false)
	if err != nil {
		t.Fatalf("could not retake a released lock: %v", err)
	}
	if err := second.release(); err != nil {
		t.Fatal(err)
	}
}

func TestLock_SharedLocksCoexist(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "graph")
	// A shared lock is taken on a store that exists.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	first, err := acquireLock(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	second, err := acquireLock(dir, true)
	if err != nil {
		t.Fatalf("two shared locks must coexist: %v", err)
	}
	defer second.release()
}

// TestGraphene_SecondWriterRefused pins the graphene behaviour lock.go and the
// handle registry were written around. Up to v0.4.0 a second Open of the same
// directory succeeded, and the two stores kept separate overlays and indexes
// while appending to one WAL. Since v0.9.0 graphene takes its own store lock,
// so a second writer is refused. If this starts failing, graphene has dropped
// that lock and the registry in handle.go is load-bearing again.
func TestGraphene_SecondWriterRefused(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "graph")

	first, err := graphenedb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := graphenedb.Open(dir)
	if err == nil {
		second.Close()
		t.Fatal("graphene opened a second writer on a store that is already open")
	}
	if !errors.Is(err, graphenedisk.ErrStoreLocked) {
		t.Fatalf("second Open failed with %v, want ErrStoreLocked", err)
	}
}

func TestExclusiveWrite_Serialises(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	// inFlight would exceed 1 if the critical sections overlapped. It is only
	// ever mutated under the lock ExclusiveWrite holds, so the check is sound
	// without additional synchronisation.
	var (
		wg       sync.WaitGroup
		inFlight int
		maxSeen  int
	)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = handle.ExclusiveWrite(func() error {
				inFlight++
				if inFlight > maxSeen {
					maxSeen = inFlight
				}
				inFlight--
				return nil
			})
		}()
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Errorf("observed %d concurrent writers inside ExclusiveWrite, want 1", maxSeen)
	}
}

func TestExclusiveWrite_RefusedOnReadOnlyHandle(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)

	handle, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	if err := handle.ExclusiveWrite(func() error { return nil }); err == nil {
		t.Error("ExclusiveWrite succeeded on a read-only handle")
	}
}

func TestOpen_ReadOnlyHandleWillNotEscalate(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)

	readOnly, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Release()

	if _, err := Open(root); err == nil {
		t.Error("a writable Open succeeded against an existing read-only handle; " +
			"the shared lock it holds does not exclude other processes from writing")
	}
}

// TestIsOpen: a graph counts as open from the start of its open to its last
// release, under either spelling of its root.
func TestIsOpen(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	kvdb := filepath.Join(root, cnst.KVDBDIR)
	if IsOpen(root) {
		t.Fatal("a graph never opened is open")
	}
	key, err := registryKey(util.GraphPath(root))
	if err != nil {
		t.Fatal(err)
	}
	registryMu.Lock()
	opening[key] = make(chan struct{})
	registryMu.Unlock()
	busy := IsOpen(kvdb)
	registryMu.Lock()
	delete(opening, key)
	registryMu.Unlock()
	if !busy {
		t.Error("a graph being opened is not open")
	}

	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if !IsOpen(kvdb) {
		t.Error("an open graph is not open")
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if IsOpen(root) {
		t.Error("a released graph is still open")
	}
}
