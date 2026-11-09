package graph

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"indicer/lib/graph/propindex"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/util"

	graphenedb "github.com/aoiflux/graphene"
	graphenedisk "github.com/aoiflux/graphene/disk"
	graphstore "github.com/aoiflux/graphene/store"
)

// storeWithNode creates a graph store under a new data root holding one node,
// and releases it.
func storeWithNode(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	w, err := h.Writer(ActorRef{})
	if err != nil {
		t.Fatal(err)
	}
	b := w.Batch()
	if _, err := b.Upsert(UpsertKey{Key: "probe", Value: "one"},
		[]graphstore.NodeType{graphstore.NodeTypeTag}, props.Map{"name": "one"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	return root
}

func nodeCount(t *testing.T, h *Handle) uint64 {
	t.Helper()
	n, err := h.Graph().NodeCount()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A read-only open of a data root without a graph is ErrNoGraph, and creates
// nothing: list and micro list used to create graph/ in a read-only listing.
func TestReadOnlyOpenOfMissingStoreCreatesNothing(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	_, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if !errors.Is(err, ErrNoGraph) {
		t.Fatalf("read-only open of a missing store: %v, want ErrNoGraph", err)
	}
	if _, err := os.Stat(util.GraphPath(root)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a read-only open created the graph directory (%v)", err)
	}
}

// GRA-21: read-only handles take graphene's shared lock, so another process
// can read the store at the same time.
func TestReadOnlyOpensShareTheStore(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)

	h, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if got := nodeCount(t, h); got != 1 {
		t.Fatalf("read-only handle sees %d nodes, want 1", got)
	}

	// Another process opening the store for reading: graphene's own read-only
	// open, plus the DUES lock taken shared.
	lock, err := acquireLock(util.GraphPath(root), true)
	if err != nil {
		t.Fatalf("a second shared DUES lock was refused: %v", err)
	}
	defer lock.release()
	other, err := graphenedb.OpenReadOnly(util.GraphPath(root))
	if err != nil {
		t.Fatalf("a second reader was refused while a read-only handle is open: %v", err)
	}
	other.Close()

	// A writer is still excluded.
	if w, err := graphenedb.Open(util.GraphPath(root)); err == nil {
		w.Close()
		t.Fatal("a writer opened the store while a read-only handle held it")
	}
}

// Read-only media: nothing under the graph directory can be opened for
// writing. graphene's shared lock opens its lock file for writing, so the open
// reads without a process lock, and writes nothing.
func TestReadOnlyOpenOnReadOnlyMedia(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)
	dir := util.GraphPath(root)
	before := dirSnapshot(t, dir)

	makeReadOnly(t, dir)
	if f, err := os.OpenFile(filepath.Join(dir, "graphene.lock"), os.O_RDWR, 0); err == nil {
		f.Close()
		t.Skip("file modes are not enforced for this user")
	}

	h, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open on read-only media: %v", err)
	}
	if got := nodeCount(t, h); got != 1 {
		t.Errorf("read-only handle sees %d nodes, want 1", got)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if after := dirSnapshot(t, dir); after != before {
		t.Errorf("a read-only open changed the store:\nbefore %s\nafter  %s", before, after)
	}
}

// makeReadOnly removes write permission from dir and the files in it, and
// restores it when the test ends.
func makeReadOnly(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(dir, e.Name()), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		for _, e := range entries {
			_ = os.Chmod(filepath.Join(dir, e.Name()), 0o644)
		}
	})
}

// dirSnapshot lists a directory's files with their sizes.
func dirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(e.Name() + "=" + strconv.FormatInt(info.Size(), 10) + " ")
	}
	return b.String()
}

// A store held by a writer that took only graphene's lock (an older DUES, or
// graphene's own tools) is reported as *ErrGraphLocked, not as a failed open.
func TestOpenOfLockedStoreIsErrGraphLocked(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)

	held, err := graphenedb.Open(util.GraphPath(root))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	for _, readOnly := range []bool{false, true} {
		_, err := OpenWith(root, OpenOptions{ReadOnly: readOnly})
		var locked *ErrGraphLocked
		if !errors.As(err, &locked) {
			t.Fatalf("read-only %v: %v (%T), want *ErrGraphLocked", readOnly, err, err)
		}
		if !errors.Is(err, graphenedisk.ErrStoreLocked) {
			t.Errorf("read-only %v: %v does not match disk.ErrStoreLocked", readOnly, err)
		}
	}
}

// GRA-6: a slow open (a long WAL replay) of one store does not stop the
// process using another, and concurrent opens of the slow store open it once.
func TestSlowOpenDoesNotBlockOtherStores(t *testing.T) {
	cleanRegistry(t)
	slow, fast := storeWithNode(t), storeWithNode(t)
	slowKey, err := registryKey(util.GraphPath(slow))
	if err != nil {
		t.Fatal(err)
	}

	entered, proceed := make(chan struct{}), make(chan struct{})
	var opens atomic.Int32
	orig := openStore
	openStore = func(dir string, readOnly bool, metrics graphstore.Metrics) (*graphenedb.Graph, error) {
		if key, _ := registryKey(dir); key == slowKey {
			if opens.Add(1) == 1 {
				close(entered)
			}
			<-proceed
		}
		return orig(dir, readOnly, metrics)
	}
	t.Cleanup(func() { openStore = orig })

	var wg sync.WaitGroup
	handles := make([]*Handle, 2)
	errs := make([]error, 2)
	for i := range handles {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			handles[i], errs[i] = Open(slow)
		}(i)
	}
	<-entered

	done := make(chan error, 1)
	go func() {
		h, err := Open(fast)
		if err == nil {
			_ = nodeCount(t, h)
			err = h.Release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		close(proceed)
		t.Fatal("opening one store waited for another store's open")
	}

	close(proceed)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open %d of the slow store: %v", i, err)
		}
	}
	if handles[0] != handles[1] {
		t.Error("concurrent opens of one store returned different handles")
	}
	if n := opens.Load(); n != 1 {
		t.Errorf("the slow store was opened %d times, want 1", n)
	}
	for _, h := range handles {
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

// GRA-8: each holder records itself, and a shared holder's release removes
// only its own record, not the other holders'.
func TestSharedHolderKeepsOtherHoldersInfo(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "graph")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	first, err := acquireLock(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireLock(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	if holder := readLockInfo(dir); holder == nil || holder.PID != os.Getpid() {
		t.Fatalf("after one shared holder released, the remaining holder is %+v", holder)
	}
}

// GRA-8: two spellings of one directory are one store.
func TestRegistryKeyFoldsSpellings(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()

	var other string
	switch runtime.GOOS {
	case "windows":
		other = strings.ToUpper(root)
	default:
		other = filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(root, other); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}
	h2, err := Open(other)
	if err != nil {
		t.Fatalf("open through %s: %v", other, err)
	}
	defer h2.Release()
	if h2 != h {
		t.Errorf("%s and %s produced different handles for one store", root, other)
	}
}

// GRA-7: an empty marker (a crash before the old marker write reached disk)
// counts as absent. It used to refuse every open of the store.
func TestEmptyMarkerCountsAsAbsent(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)
	marker := filepath.Join(util.GraphPath(root), schema.MarkerFileName)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open with an empty marker: %v", err)
	}
	if err := ro.Release(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(marker); err != nil || info.Size() != 0 {
		t.Fatalf("a read-only open rewrote the marker (%v)", err)
	}

	h, err := Open(root)
	if err != nil {
		t.Fatalf("writable open with an empty marker: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	if m, found, err := schema.ReadMarker(util.GraphPath(root)); err != nil || !found || m.SchemaVersion != schema.CurrentVersion {
		t.Fatalf("marker after a writable open: %+v, found %v, %v", m, found, err)
	}
}

// GRA-9: a store with no marker that holds only nodes of graphene's own types
// (micro-artefacts) has no labels to misread, so it opens.
func TestMarkerlessStoreOfBuiltinKindsOpens(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	dir := util.GraphPath(root)
	g, err := graphenedb.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.AddNodes([]*graphstore.Node{{Labels: []graphstore.NodeType{graphstore.NodeTypeMicroArtefact}}}); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}

	h, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("store holding only micro-artefacts: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
}

// No error advises a command that does not exist.
func TestNoAdviceToRunMissingCommands(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		schema.CheckLibraryBases(1, 1).Error(),
		propindex.ErrLegacyEncoding.Error(),
	} {
		if strings.Contains(msg, "graph migrate") {
			t.Errorf("error advises a command that does not exist: %s", msg)
		}
	}
}

// GRA-11: the marker and producer stamps name the graphene version this
// binary links, read from its build information.
func TestStampsNameLinkedGraphene(t *testing.T) {
	cleanRegistry(t)
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build information")
	}
	var version string
	for _, dep := range info.Deps {
		if dep.Path == "github.com/aoiflux/graphene" {
			version = dep.Version
			if dep.Replace != nil {
				version = dep.Replace.Version
			}
		}
	}
	if version == "" {
		t.Skip("graphene is not in the build information")
	}

	root := storeWithNode(t)
	m, _, err := schema.ReadMarker(util.GraphPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.WrittenBy, "graphene "+version) {
		t.Errorf("marker written_by %q does not name graphene %s", m.WrittenBy, version)
	}
	if got := DefaultProducer.String(); !strings.HasSuffix(got, "graphene@"+version) {
		t.Errorf("producer stamp %q does not name graphene@%s", got, version)
	}
}

// GRA-6: a log large enough to make every open slow is reported.
func TestLargeLogIsReported(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)

	origBytes := replayWarnBytes
	replayWarnBytes = 1
	t.Cleanup(func() { replayWarnBytes = origBytes })

	h, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if !strings.Contains(h.ReplayWarning(), "replay") {
		t.Errorf("no replay warning for a log over the threshold: %q", h.ReplayWarning())
	}
}
