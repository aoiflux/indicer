package graph

import (
	"fmt"
	"path/filepath"
	"sync"

	"indicer/lib/util"

	graphenedb "github.com/aoiflux/graphene"
)

// Handle is the process's single reference-counted grip on one graph store.
//
// Open returns the same Handle for the same directory, so a command that
// already holds one and then calls into code which opens "another" repository
// gets the store it already has rather than a second, independent view of the
// same files. See the package doc for why a second view is corruption rather
// than merely waste.
type Handle struct {
	key      string
	dir      string
	graph    *graphenedb.Graph
	lock     *fileLock
	readOnly bool

	// writeMu serialises read-then-write sequences. The upsert the repositories
	// perform — look the node up by an indexed property, insert if absent — is
	// not atomic in graphene, so two goroutines can both miss and both insert.
	// graphene's own locking makes each individual call safe; it cannot make a
	// pair of them safe.
	writeMu sync.Mutex

	// refs is guarded by registryMu, not by an atomic, so that finding a live
	// handle and incrementing it is one step. With an atomic, a concurrent
	// Release could close the store between the map lookup and the increment.
	refs int
}

type OpenOptions struct {
	// ReadOnly takes a shared lock and refuses writes, so several inspection
	// commands can run against one store at once.
	ReadOnly bool
}

var (
	registryMu sync.Mutex
	registry   = map[string]*Handle{}
)

// Open returns the handle for root's graph directory, opening the store on the
// first call and incrementing the reference count on every subsequent one.
//
// Each successful Open must be balanced by exactly one Release.
func Open(root string) (*Handle, error) { return OpenWith(root, OpenOptions{}) }

func OpenWith(root string, opts OpenOptions) (*Handle, error) {
	dir := util.GraphPath(root)
	key, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("graph: resolve %s: %w", dir, err)
	}
	key = filepath.Clean(key)

	registryMu.Lock()
	defer registryMu.Unlock()

	if existing, ok := registry[key]; ok {
		// A read-only caller can share an existing writable handle; the reverse
		// cannot be allowed, because the shared lock already taken does not
		// exclude other processes from writing.
		if existing.readOnly && !opts.ReadOnly {
			return nil, fmt.Errorf(
				"graph: %s is already open read-only in this process; "+
					"cannot escalate to a writable handle", key)
		}
		existing.refs++
		return existing, nil
	}

	lock, err := acquireLock(key, opts.ReadOnly)
	if err != nil {
		return nil, err
	}

	store, err := graphenedb.Open(key)
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("graph: open %s: %w", key, err)
	}

	handle := &Handle{
		key:      key,
		dir:      key,
		graph:    store,
		lock:     lock,
		readOnly: opts.ReadOnly,
		refs:     1,
	}
	registry[key] = handle
	return handle, nil
}

// Release drops one reference. The store is closed, and the lock released, when
// the last one goes.
func (h *Handle) Release() error {
	if h == nil {
		return nil
	}

	registryMu.Lock()
	if h.refs <= 0 {
		registryMu.Unlock()
		return fmt.Errorf("graph: unbalanced Release on %s (reference count is already zero)", h.key)
	}
	h.refs--
	if h.refs > 0 {
		registryMu.Unlock()
		return nil
	}
	// Drop the registry entry while still holding the lock so that a concurrent
	// Open cannot hand out a handle whose store is midway through closing.
	delete(registry, h.key)
	store, lock := h.graph, h.lock
	h.graph, h.lock = nil, nil
	registryMu.Unlock()

	var closeErr error
	if store != nil {
		closeErr = store.Close()
	}
	lockErr := lock.release()

	if closeErr != nil {
		return fmt.Errorf("graph: close %s: %w", h.key, closeErr)
	}
	return lockErr
}

// Graph exposes the underlying store.
//
// This is the seam that will narrow as the rest of the plan lands: today the
// repositories drive graphene directly through it, and later they will go
// through Writer/Batch instead. It returns nil after the final Release, which
// callers reach only by using a handle they no longer own.
func (h *Handle) Graph() *graphenedb.Graph {
	if h == nil {
		return nil
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	return h.graph
}

// Dir is the absolute graph directory, for diagnostics and error messages.
func (h *Handle) Dir() string {
	if h == nil {
		return ""
	}
	return h.dir
}

// ReadOnly reports whether this handle was opened for inspection only.
func (h *Handle) ReadOnly() bool { return h != nil && h.readOnly }

// ExclusiveWrite runs fn with the handle's writer lock held.
//
// Wrap any sequence that reads the graph and then writes based on what it read.
// Holding it around a pure read is unnecessary; holding it around an upsert is
// what makes the upsert one operation instead of two.
func (h *Handle) ExclusiveWrite(fn func() error) error {
	if h == nil {
		return fmt.Errorf("graph: ExclusiveWrite on a nil handle")
	}
	if h.readOnly {
		return fmt.Errorf("graph: %s is open read-only; writes are refused", h.key)
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	return fn()
}

// openHandleCount reports how many distinct stores are open. Tests use it to
// assert that nested opens collapse to one.
func openHandleCount() int {
	registryMu.Lock()
	defer registryMu.Unlock()
	return len(registry)
}

// refCountFor reports the reference count for a handle, for tests.
func refCountFor(h *Handle) int {
	registryMu.Lock()
	defer registryMu.Unlock()
	return h.refs
}
