package ingest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/lib/dbio"
	duesgraph "indicer/lib/graph"
	"indicer/lib/index"
	"indicer/lib/util"
)

// How long, and how often, a test checks that Close has started refusing.
const (
	closingTimeout = 30 * time.Second
	closingPoll    = time.Millisecond
)

// TestSessionOpensEachStoreOnce: a session that stores and then reindexes
// several evidence with the full-text and graph features opens the
// full-text index and the graph once each, not once per evidence. Close
// releases them; a closed session refuses work before writing anything.
func TestSessionOpensEachStoreOnce(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	s := NewSession(db)
	var ids [][]byte
	for _, name := range []string{"fat/fat12", "fat/fat16", "part/mbr_two"} {
		res, err := s.Run(t.Context(), Request{Path: fixture(t, name), Decode: true, Quiet: true, FTS: true, Enrich: true})
		if err != nil {
			t.Fatalf("store %s: %v", name, err)
		}
		ids = append(ids, res.EvidenceID)
	}
	for _, id := range ids {
		if _, err := s.Reindex(t.Context(), id, ReindexOptions{}); err != nil {
			t.Fatalf("reindex %x: %v", id, err)
		}
	}
	if got, want := s.Opens(), (Opens{Text: 1, Graph: 1}); got != want {
		t.Fatalf("opens = %+v, want %+v", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := s.Run(t.Context(), Request{Path: fixture(t, "fat/fat32"), Decode: true, Quiet: true}); !errors.Is(err, errSessionClosed) {
		t.Fatalf("run after close = %v, want %v", err, errSessionClosed)
	}

	// Released: a new session opens both again.
	again := NewSession(db)
	if _, err := again.Reindex(t.Context(), ids[0], ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, want := again.Opens(), (Opens{Text: 1, Graph: 1}); got != want {
		t.Fatalf("opens after a new session = %+v, want %+v", got, want)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSessionRetriesAFailedOpen: a store that fails to open is not kept as
// failed. The next evidence that needs it tries again, and opens it once the
// cause is gone (another process held it, say).
func TestSessionRetriesAFailedOpen(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	root := dbio.KVDir(db)
	blocked := []string{util.FTSPath(root), util.GraphPath(root)}
	for _, p := range blocked {
		if err := os.WriteFile(p, []byte("not a store"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := NewSession(db)
	defer s.Close()
	if _, err := s.textIndex(true); err == nil {
		t.Fatal("opened a full-text index that is a file")
	}
	if _, err := s.graphStore(true); err == nil {
		t.Fatal("opened a graph that is a file")
	}
	for _, p := range blocked {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if x, err := s.textIndex(true); err != nil || x == nil {
		t.Fatalf("full-text index after the cause went: %v, %v", x, err)
	}
	if g, err := s.graphStore(true); err != nil || g == nil {
		t.Fatalf("graph after the cause went: %v, %v", g, err)
	}
	if got, want := s.Opens(), (Opens{Text: 1, Graph: 1}); got != want {
		t.Fatalf("opens = %+v, want %+v", got, want)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSessionMakesNoIndexWithoutFiles: full-text indexing of an evidence
// with no indexed files neither opens nor creates the index, as before
// sessions; and a reindex of a database without a graph creates none.
func TestSessionMakesNoIndexWithoutFiles(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	path := testutil.WriteRandomFile(t, t.TempDir(), "noise.bin", 7, 300<<10)
	s := NewSession(db)
	res, err := s.Run(t.Context(), Request{Path: path, Decode: true, Quiet: true, FTS: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reindex(t.Context(), res.EvidenceID, ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := s.Opens(); got != (Opens{}) {
		t.Fatalf("opens = %+v, want none", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	root := dbio.KVDir(db)
	for _, p := range []string{util.FTSPath(root), util.GraphPath(root)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists (%v): the session created a store nothing needed", p, err)
		}
	}
}

// TestSessionCloseReportsAReleaseThatFailed: Close returns the error a
// store's release gave, and still lets go of everything: the session is
// closed, and a second Close has nothing left to release.
func TestSessionCloseReportsAReleaseThatFailed(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	s := NewSession(db)
	if _, err := s.textIndex(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.graphStore(true); err != nil {
		t.Fatal(err)
	}
	// Another holder of the graph releases one reference more than it took,
	// so the session's release of its own finds none left.
	h, err := duesgraph.Open(dbio.KVDir(db))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), "unbalanced Release") {
		t.Fatalf("close = %v, want the graph's release error", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close = %v, want nothing left to release", err)
	}
	if _, err := s.textIndex(true); !errors.Is(err, errSessionClosed) {
		t.Fatalf("full-text index after a failed close = %v, want %v", err, errSessionClosed)
	}
}

// TestSessionCloseWaitsForRunningCalls: Close refuses new calls at once but
// lets a running ingest finish, opening the stores it needs, and releases
// them only after it returns.
func TestSessionCloseWaitsForRunningCalls(t *testing.T) {
	t.Parallel()
	db := openDB(t)
	s := NewSession(db)
	stored, err := s.Run(t.Context(), Request{Path: fixture(t, "fat/fat12"), Decode: true, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}

	indexing, release := make(chan struct{}), make(chan struct{})
	s.passes.index = func(ctx context.Context, o index.Options) (index.Result, error) {
		close(indexing)
		<-release
		return index.Run(ctx, o)
	}
	type outcome struct {
		res Result
		err error
	}
	ran := make(chan outcome, 1)
	go func() {
		res, err := s.Run(t.Context(), Request{Path: fixture(t, "fat/fat16"), Decode: true, Quiet: true, FTS: true, Enrich: true})
		ran <- outcome{res, err}
	}()
	<-indexing
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	waitClosing(t, s)

	if _, err := s.Run(t.Context(), Request{Path: fixture(t, "fat/fat32"), Decode: true, Quiet: true}); !errors.Is(err, errSessionClosed) {
		t.Fatalf("run while closing = %v, want %v", err, errSessionClosed)
	}
	if _, err := s.Reindex(t.Context(), stored.EvidenceID, ReindexOptions{}); !errors.Is(err, errSessionClosed) {
		t.Fatalf("reindex while closing = %v, want %v", err, errSessionClosed)
	}
	if err := s.DeleteGraphIndex(stored.EvidenceID, true); !errors.Is(err, errSessionClosed) {
		t.Fatalf("graph delete while closing = %v, want %v", err, errSessionClosed)
	}
	select {
	case err := <-closed:
		t.Fatalf("close returned (%v) while an ingest was running", err)
	default:
	}

	close(release)
	if got := <-ran; got.err != nil {
		t.Fatalf("the running ingest: %v", got.err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, want := s.Opens(), (Opens{Text: 1, Graph: 1}); got != want {
		t.Fatalf("opens = %+v, want %+v: the running ingest's stores", got, want)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.text != nil || s.graph != nil {
		t.Fatal("close kept a store open")
	}
}

// waitClosing waits until s refuses new calls.
func waitClosing(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.Now().Add(closingTimeout)
	for {
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("close did not start refusing calls in %v", closingTimeout)
		}
		time.Sleep(closingPoll)
	}
}
