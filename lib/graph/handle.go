package graph

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"indicer/internal/console"
	"indicer/lib/graph/migrate"
	"indicer/lib/graph/propindex"
	"indicer/lib/graph/schema"
	"indicer/lib/logging"
	"indicer/lib/util"

	graphenedb "github.com/aoiflux/graphene"
	graphenedisk "github.com/aoiflux/graphene/disk"
	graphstore "github.com/aoiflux/graphene/store"
	"go.uber.org/zap"
)

// ErrNoGraph is returned by a read-only open of a data root that has no graph
// store. Nothing is created: a read-only command must not leave a store
// behind (CFG-7).
var ErrNoGraph = errors.New("graph: no graph store")

// Handle is the process's single reference-counted grip on one graph store.
//
// Open returns the same Handle for the same directory, so a command that
// already holds one and then calls into code which opens "another" repository
// gets the store it already has rather than a second, independent view of the
// same files. graphene refuses a second writer since v0.9.0, so a second view
// would now fail rather than corrupt, but the code paths that nest opens would
// still fail.
type Handle struct {
	key      string
	dir      string
	graph    *graphenedb.Graph
	lock     *fileLock
	readOnly bool

	// actor signs this handle's commits; zero when they are not signed
	// (SignWith).
	actor ActorRef

	// replayWarning is set when the store's log makes every open slow.
	replayWarning string

	// stages carries a compaction's stages to its progress line. Only a handle
	// opened with OpenOptions.Stages has one.
	stages *stageSink

	// schema resolves DUES kinds to the raw type values this particular store
	// uses, which differ between a store written by graphene v0.1.3 and one
	// written by v0.4.0.
	schema     *schema.Registry
	vocabulary migrate.Vocabulary

	// writeMu serialises read-then-write sequences. The upsert the repositories
	// perform — look the node up by an indexed property, insert if absent — is
	// not atomic in graphene, so two goroutines can both miss and both insert.
	// graphene's own locking makes each individual call safe; it cannot make a
	// pair of them safe.
	writeMu sync.Mutex

	// identity caches "indexKey\x00value" -> NodeID for the life of the handle.
	//
	// It never needs invalidating within a process: graphene issues IDs
	// monotonically, never reuses them, and preserves them across compaction.
	// It exists because the enrichment write path looks the same evidence and
	// partition up once per file, which on a large image is the dominant cost
	// of an upsert. Deleting a node forgets it (forgetIdentity).
	identity sync.Map

	// refs is guarded by registryMu, not by an atomic, so that finding a live
	// handle and incrementing it is one step. With an atomic, a concurrent
	// Release could close the store between the map lookup and the increment.
	refs int
}

// lookupIdentity resolves a cached node identity.
func (h *Handle) lookupIdentity(cacheKey string) (graphstore.NodeID, bool) {
	if h == nil {
		return graphstore.InvalidNodeID, false
	}
	if cached, ok := h.identity.Load(cacheKey); ok {
		return cached.(graphstore.NodeID), true
	}
	return graphstore.InvalidNodeID, false
}

// rememberIdentity caches a node identity. Only durable identities are cached;
// see Batch.Commit.
func (h *Handle) rememberIdentity(cacheKey string, id graphstore.NodeID) {
	if h == nil || id == graphstore.InvalidNodeID {
		return
	}
	h.identity.Store(cacheKey, id)
}

// forgetIdentities drops every cached identity that resolves to one of ids.
func (h *Handle) forgetIdentities(ids map[graphstore.NodeID]struct{}) {
	if h == nil || len(ids) == 0 {
		return
	}
	h.identity.Range(func(key, value any) bool {
		if _, gone := ids[value.(graphstore.NodeID)]; gone {
			h.identity.Delete(key)
		}
		return true
	})
}

type OpenOptions struct {
	// ReadOnly takes shared locks and refuses writes, so several inspection
	// commands can run against one store at once. A missing store is
	// ErrNoGraph.
	ReadOnly bool

	// Stages has graphene report a compaction's stages, so Compact can show
	// them. graphene then reads the clock around every query and commit, so
	// only dues graph compact asks for it. It is ignored on a read-only open
	// and on a handle this process already holds.
	Stages bool
}

var (
	registryMu sync.Mutex
	registry   = map[string]*Handle{}

	// opening holds a channel per directory being opened, closed when the open
	// ends. The open itself replays the store's log, which can take minutes,
	// so it runs without registryMu: other stores' handles stay usable, and a
	// second open of the same directory waits for the first (GRA-6).
	opening = map[string]chan struct{}{}
)

// openStore opens graphene's store. It is a variable so tests can slow it.
//
// A read-only open takes graphene's shared lock. graphene opens its lock file
// for writing even to share it, so on media nothing can be written to the
// shared open fails; the store is then read without a process lock, which is
// safe there because no writer can exist (CFG-12).
//
// opts are storeOptions with the open's signing and metrics added.
var openStore = func(dir string, opts graphenedisk.Options) (*graphenedb.Graph, error) {
	g, err := graphenedb.OpenWithOptions(dir, opts)
	if err != nil && opts.ReadOnly && isReadOnlyMedia(err) {
		opts.LiveReader = true
		return graphenedb.OpenWithOptions(dir, opts)
	}
	return g, err
}

// storeOptions are graphene's options for every open: Open's and
// OpenReadOnly's, keeping every log segment a compaction retires. graphene
// keeps none by default, and a segment holds each commit's actor, time and
// signature, which a compacted image does not.
func storeOptions(readOnly bool) graphenedisk.Options {
	opts := graphenedisk.Options{Retention: graphenedisk.RetentionPolicy{MaxSegments: math.MaxInt32}}
	if readOnly {
		opts.ReadOnly = true
		opts.Constraints = graphenedisk.ConstraintDrop
	}
	return opts
}

// Open returns the handle for root's graph directory, opening the store on the
// first call and incrementing the reference count on every subsequent one.
//
// Each successful Open must be balanced by exactly one Release.
func Open(root string) (*Handle, error) { return OpenWith(root, OpenOptions{}) }

func OpenWith(root string, opts OpenOptions) (*Handle, error) {
	dir := util.GraphPath(root)
	key, err := registryKey(dir)
	if err != nil {
		return nil, fmt.Errorf("graph: resolve %s: %w", dir, err)
	}

	for {
		registryMu.Lock()
		if existing, ok := registry[key]; ok {
			// A read-only caller can share an existing writable handle; the
			// reverse cannot be allowed, because the shared lock already taken
			// does not exclude other processes from writing.
			if existing.readOnly && !opts.ReadOnly {
				registryMu.Unlock()
				return nil, fmt.Errorf(
					"graph: %s is already open read-only in this process; "+
						"cannot escalate to a writable handle", existing.dir)
			}
			existing.refs++
			registryMu.Unlock()
			return existing, nil
		}
		if wait, ok := opening[key]; ok {
			registryMu.Unlock()
			<-wait
			continue
		}
		done := make(chan struct{})
		opening[key] = done
		registryMu.Unlock()

		handle, err := openHandle(dir, key, opts)

		registryMu.Lock()
		delete(opening, key)
		if err == nil {
			registry[key] = handle
		}
		registryMu.Unlock()
		close(done)
		return handle, err
	}
}

// registryKey names a graph directory so that two spellings of it are one
// key: symlinks are resolved, and on Windows case is folded (GRA-8).
func registryKey(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	key := filepath.Clean(abs)
	// The graph directory may not exist yet; resolve the deepest part that
	// does and keep the rest as written.
	var rest []string
	for probe := key; ; {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			key = filepath.Join(append([]string{resolved}, rest...)...)
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		rest = append([]string{filepath.Base(probe)}, rest...)
		probe = parent
	}
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key, nil
}

func openHandle(dir, key string, opts OpenOptions) (*Handle, error) {
	if opts.ReadOnly {
		info, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w at %s", ErrNoGraph, dir)
		}
		if err != nil {
			return nil, fmt.Errorf("graph: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("graph: %s is not a directory", dir)
		}
	}

	lock, err := acquireLock(dir, opts.ReadOnly)
	if err != nil {
		return nil, err
	}
	if err := checkImage(dir); err != nil {
		_ = lock.release()
		return nil, err
	}

	warning := replayWarningFor(dir)
	if warning != "" {
		logging.GetLogger().Warn("graph: slow open", zap.String("dir", dir), zap.String("warning", warning))
		console.Errorln("warning: " + warning)
	}

	signs, err := prepareSigning(dir, key, opts.ReadOnly)
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	storeOpts := storeOptions(opts.ReadOnly)
	signs.apply(&storeOpts)
	var stages *stageSink
	if opts.Stages && !opts.ReadOnly {
		stages = &stageSink{}
		storeOpts.Metrics = stages
	}
	store, err := openStore(dir, storeOpts)
	if err != nil {
		_ = lock.release()
		if errors.Is(err, graphenedisk.ErrStoreLocked) {
			return nil, &ErrGraphLocked{Dir: dir, Holder: readLockInfo(dir), err: err}
		}
		return nil, fmt.Errorf("graph: open %s: %w", dir, err)
	}
	signs.warnUnknown(dir)
	if !opts.ReadOnly {
		if err := stubImage(dir, store); err != nil {
			_ = store.Close()
			_ = lock.release()
			return nil, err
		}
	}

	vocabulary, kinds, err := resolveSchema(dir, store, opts.ReadOnly)
	if err != nil {
		_ = store.Close()
		_ = lock.release()
		return nil, err
	}

	// Declaring the ordered keys turns a range filter into two binary searches.
	// It is idempotent and covers only keys with no conflicting stored history,
	// so it is safe on a legacy store as well as a new one.
	if !opts.ReadOnly {
		if err := propindex.Declare(store, kinds); err != nil {
			_ = store.Close()
			_ = lock.release()
			return nil, err
		}
	}

	return &Handle{
		key:           key,
		dir:           dir,
		graph:         store,
		lock:          lock,
		readOnly:      opts.ReadOnly,
		actor:         signs.actor,
		replayWarning: warning,
		stages:        stages,
		schema:        kinds,
		vocabulary:    vocabulary,
		refs:          1,
	}, nil
}

// stubImage gives an empty store an empty image. graphene v0.1, which DUES
// v0.39 and older link, reads this release's log as an empty graph and
// appends to it in its own format; it refuses any image but its own version,
// so a store with this one fails loudly there instead of being misread.
// A store that holds anything, or already has an image, is left alone: its
// image comes from dues graph compact.
func stubImage(dir string, store *graphenedb.Graph) error {
	if _, err := os.Stat(filepath.Join(dir, ImageFileName)); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	nodes, err := store.NodeCount()
	if err != nil {
		return fmt.Errorf("graph: count nodes in %s: %w", dir, err)
	}
	edges, err := store.EdgeCount()
	if err != nil {
		return fmt.Errorf("graph: count edges in %s: %w", dir, err)
	}
	if nodes != 0 || edges != 0 {
		return nil
	}
	if err := store.Compact(); err != nil {
		return fmt.Errorf("graph: write the first image in %s: %w", dir, err)
	}
	return nil
}

// Every open replays the log written since the last compaction. Past these
// sizes that is slow enough, and needs enough memory, to say so, and a CLI
// command compacts the store when it is done (Config).
var (
	replayWarnBytes   int64 = 1 << 30
	replayWarnRecords int64 = 5_000_000
)

// minWALRecordBytes is the smallest record graphene's log holds: a type byte,
// a length and a checksum. A log shorter than replayWarnRecords of them
// cannot hold that many records, so it is not walked to count them.
const minWALRecordBytes = 9

// replayWarningFor describes what opening dir will cost, when that is worth
// a warning. The size is read from the file system; the records are counted
// only when the log could hold enough of them to matter.
func replayWarningFor(dir string) string {
	info, err := os.Stat(filepath.Join(dir, "graphene.wal"))
	if err != nil {
		return ""
	}
	size := info.Size()
	const advice = "; dues graph compact folds it into the image"
	if size >= replayWarnBytes {
		return fmt.Sprintf("the graph store at %s replays %d MiB of log at every open, "+
			"which is slow and needs memory in proportion%s", dir, size>>20, advice)
	}
	if size < replayWarnRecords*minWALRecordBytes {
		return ""
	}
	est, err := graphenedisk.PreflightOpen(dir)
	if err != nil {
		return ""
	}
	if records := est.WALRecords + est.WALRecordsBuffered; records > replayWarnRecords {
		return fmt.Sprintf("the graph store at %s replays %d log records at every open, "+
			"which is slow and needs memory in proportion%s", dir, records, advice)
	}
	return ""
}

// resolveSchema classifies the store's label encoding and stamps the marker.
//
// This is the automatic half of the migration: a store written by graphene
// v0.1.3 is detected here and read through a registry that matches both
// encodings, with no user action and nothing rewritten.
func resolveSchema(dir string, store *graphenedb.Graph, readOnly bool) (migrate.Vocabulary, *schema.Registry, error) {
	report, err := migrate.Detect(dir, store)
	if err != nil {
		return report.Vocabulary, nil, err
	}

	kinds, err := migrate.RegistryFor(report)
	if err != nil {
		return report.Vocabulary, nil, err
	}

	if readOnly {
		return report.Vocabulary, kinds, nil
	}

	// Stamp the marker when it is absent or when what it records has changed.
	// Writing it on a legacy store is not a migration: it records what was
	// found so the next open does not have to probe, and records that reads
	// must stay widened.
	mixed := kinds.Legacy()
	image, imageChanged := adoptImage(dir, report.Marker.Image)
	if !report.HasMarker ||
		report.Marker.SchemaVersion != schema.CurrentVersion ||
		report.Marker.MixedVocabulary != mixed || imageChanged {

		marker := schema.NewMarker(writtenBy, mixed)
		marker.Image = image
		if report.HasMarker {
			marker.MigratedFrom = report.Marker.MigratedFrom
			if previous := report.Marker.SchemaVersion; previous != schema.CurrentVersion {
				marker.MigratedFrom = &previous
			}
			// Preserve declarations a later phase will have added.
			marker.OrderedNodeKeys = report.Marker.OrderedNodeKeys
			marker.OrderedEdgeKeys = report.Marker.OrderedEdgeKeys
			marker.DynamicNodeKinds = report.Marker.DynamicNodeKinds
			marker.Signing = report.Marker.Signing
		} else if report.Vocabulary == migrate.VocabLegacyV1 {
			legacy := 1
			marker.MigratedFrom = &legacy
		}
		if err := schema.WriteMarker(dir, marker); err != nil {
			return report.Vocabulary, nil, err
		}
	}

	return report.Vocabulary, kinds, nil
}

// Release drops one reference. The store is closed, and the lock released, when
// the last one goes. On a root configured for it (Configure), the last Release
// of a writable handle first compacts a log that has reached the threshold.
func (h *Handle) Release() error {
	if h == nil {
		return nil
	}

	registryMu.Lock()
	if h.refs <= 0 {
		registryMu.Unlock()
		return fmt.Errorf("graph: unbalanced Release on %s (reference count is already zero)", h.dir)
	}
	h.refs--
	if h.refs > 0 {
		registryMu.Unlock()
		return nil
	}
	// Drop the registry entry while still holding the lock so that a concurrent
	// Open cannot hand out a handle whose store is midway through closing. An
	// Open of the directory waits until the store is closed, compaction and
	// all, as it waits for another open.
	delete(registry, h.key)
	done := make(chan struct{})
	opening[h.key] = done
	registryMu.Unlock()
	defer func() {
		registryMu.Lock()
		delete(opening, h.key)
		registryMu.Unlock()
		close(done)
	}()

	h.compactIfDue()

	registryMu.Lock()
	store, lock := h.graph, h.lock
	h.graph, h.lock = nil, nil
	registryMu.Unlock()

	var closeErr error
	if store != nil {
		closeErr = store.Close()
	}
	lockErr := lock.release()

	if closeErr != nil {
		return fmt.Errorf("graph: close %s: %w", h.dir, closeErr)
	}
	return lockErr
}

// Graph exposes the underlying store.
//
// The repositories drive graphene directly through it for reads. It returns
// nil after the final Release, which callers reach only by using a handle they
// no longer own.
func (h *Handle) Graph() *graphenedb.Graph {
	if h == nil {
		return nil
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	return h.graph
}

// Schema resolves DUES kinds to the raw graphene type values this store uses.
// Never derive those values from graphene's CustomNodeType directly: that is
// what broke when the custom band moved.
func (h *Handle) Schema() *schema.Registry {
	if h == nil {
		return nil
	}
	return h.schema
}

// Vocabulary reports which label encodings this store was found to hold.
func (h *Handle) Vocabulary() migrate.Vocabulary {
	if h == nil {
		return migrate.VocabUnknown
	}
	return h.vocabulary
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

// ReplayWarning is the warning printed when the store was opened, or "" when
// its log is small enough not to need one.
func (h *Handle) ReplayWarning() string {
	if h == nil {
		return ""
	}
	return h.replayWarning
}

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
		return fmt.Errorf("graph: %s is open read-only; writes are refused", h.dir)
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

// Exists reports whether root has a graph store, without opening or creating
// one.
func Exists(root string) bool {
	info, err := os.Stat(util.GraphPath(root))
	return err == nil && info.IsDir()
}
