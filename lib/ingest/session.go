package ingest

import (
	"errors"
	"sync"
	"time"

	"indicer/lib/dbio"
	"indicer/lib/enrichment"
	"indicer/lib/fts"
	duesgraph "indicer/lib/graph"
	"indicer/lib/logging"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// Session is what a run of ingests shares: the database, and the full-text
// index and graph its features write. A command that stores a folder, or
// reindexes many evidence, opens each of those once, when the first evidence
// needs it, and keeps it until Close; opened per evidence, the full-text index
// was opened and closed once per file and the graph replayed its whole log
// each time. A Session is safe for concurrent use. Its transactions stay one
// per evidence: it shares resources, not commits.
type Session struct {
	db *badger.DB

	mu     sync.Mutex
	text   *fts.Index
	graph  *enrichment.GrapheneRepository
	opens  Opens
	closed bool
}

// Opens counts what a session opened: each store at most once.
type Opens struct {
	Text  int // the full-text index
	Graph int // the graph store
}

// errSessionClosed is the error of a session used after Close.
var errSessionClosed = errors.New("ingest: the session is closed")

// NewSession starts a session on db. It opens nothing until an ingest needs
// it.
func NewSession(db *badger.DB) *Session { return &Session{db: db} }

// DB is the session's database.
func (s *Session) DB() *badger.DB { return s.db }

// Opens reports what the session opened so far.
func (s *Session) Opens() Opens {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}

// Close releases the full-text index and the graph the session opened. The
// database stays open: it belongs to the caller. A second Close does nothing.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	var errs []error
	if s.text != nil {
		errs = append(errs, s.text.Release())
		s.text = nil
	}
	if s.graph != nil {
		errs = append(errs, s.graph.Close())
		s.graph = nil
	}
	return errors.Join(errs...)
}

// textIndex returns the session's full-text index, opening it the first
// time. Without create, a database with no index is fts.ErrIndexNotReady. A
// failed open is not kept: the next evidence tries again.
func (s *Session) textIndex(create bool) (*fts.Index, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return nil, errSessionClosed
	case s.text != nil:
		return s.text, nil
	}
	start := time.Now()
	x, err := fts.Acquire(s.db, create)
	if err != nil {
		return nil, err
	}
	s.text = x
	s.opens.Text++
	logging.GetLogger().Info("full-text index opened", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return x, nil
}

// graphStore returns the session's graph, opening it the first time. Without
// create, a database with no graph has none: nil, and nothing is created.
func (s *Session) graphStore(create bool) (*enrichment.GrapheneRepository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return nil, errSessionClosed
	case s.graph != nil:
		return s.graph, nil
	}
	root := dbio.KVDir(s.db)
	if !create && !duesgraph.Exists(root) {
		return nil, nil
	}
	start := time.Now()
	repo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		return nil, err
	}
	s.graph = repo
	s.opens.Graph++
	logging.GetLogger().Info("graph opened", zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	return repo, nil
}

// usable fails a session used after Close, before it writes anything.
func (s *Session) usable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errSessionClosed
	}
	return nil
}

// closeWith closes a one-shot session, adding its error to err.
func (s *Session) closeWith(err error) error {
	if cerr := s.Close(); cerr != nil {
		return errors.Join(err, cerr)
	}
	return err
}
