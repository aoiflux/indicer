// Package dbopen is the one way to open a DUES database.
//
// Open resolves the data root, takes the root lock, reads (or for a legacy
// database infers) the manifest, checks the password before badger is
// touched, opens badger with the options the manifest fixes, and checks the
// manifest against the marker stored inside badger. With Recover, a writable
// open then finishes or fails the ingests a crash left incomplete. A
// read-only open never writes: no manifest, no marker, no recovery, and no
// database is created.
package dbopen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"indicer/internal/diskspace"
	"indicer/internal/lifecycle"
	"indicer/internal/osdirs"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/fsx"
	"indicer/lib/graph/signer"
	"indicer/lib/logging"
	"indicer/lib/store"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

// Mode says how a database is opened.
type Mode int

const (
	// ReadOnly opens an existing database for reading. Several read-only
	// opens may share a database; none may share it with a writer.
	ReadOnly Mode = iota
	// ReadWrite opens a database for writing, creating it if missing.
	ReadWrite
)

// LockName is the root lock file. It covers the whole data root: badger,
// blobs, graph and full-text index.
const LockName = ".dues.lock"

// Requested is the layout the command line asks for. For a new database it
// is the layout; for an existing one, a field the user set explicitly must
// match the manifest, and the rest are ignored.
type Requested struct {
	ChunkKiB  int
	ChunkHash string
	Container bool
	// Quick creates a database without badger table compression.
	Quick bool
	// Set holds the flag names the user set explicitly (cnst.Flag*).
	Set map[string]bool
}

// Request describes an open.
type Request struct {
	// Path is the data root, or its kvdb directory. Empty means ~/.dues.
	Path  string
	Mode  Mode
	Creds dbmeta.Credentials
	// Layout is the requested layout; zero fields take the defaults.
	Layout Requested
	// KDF overrides dbmeta.DefaultKDF for a new encrypted database (tests).
	KDF *dbmeta.KDFParams
	// WrittenBy names the program in a new manifest.
	WrittenBy string
	// TempDir is where a read-only open of a database on write-protected
	// media makes its temporary badger directory (--temp-dir). Empty means
	// osdirs.Temp.
	TempDir string
	// Recover makes a writable open finish or fail, before it returns, the
	// ingests a crash left incomplete (store.RecoverIncompleteIngests).
	// Repair, which inspects them first, and the compression product leave
	// it off. A read-only open never recovers (CFG-11).
	Recover bool
	// Tuning is how the engine runs on the database (engine.For). Nil takes
	// cnst.DefaultTuning.
	Tuning *cnst.Tuning
	// Actor signs the graph commits a writable open makes, and the packages
	// it exports. Nil is the database's own actor.
	Actor *Actor
}

// Handle is an open database.
type Handle struct {
	DB       *badger.DB
	Root     string
	Manifest dbmeta.Manifest
	ReadOnly bool
	// Created is set when this open created the database.
	Created bool
	// Notices are one-line messages for the user (an unencrypted database,
	// an ignored password, a skipped lock).
	Notices []string

	lock *fsx.Lock
	// shadow is the temporary badger directory a read-only open of
	// write-protected media used (newShadow), removed at close.
	shadow string
	// tempDir is Request.TempDir, where a shadow goes.
	tempDir string
	// tuning is Request.Tuning, or cnst.DefaultTuning.
	tuning cnst.Tuning
	// atClose runs when the handle closes (AtClose).
	atClose []func()

	// keys are a writable open's own copy of the badger and blob keys, kept
	// for Resume and zeroed at close; the actor's is actorKey. suspension is
	// set while suspended.
	keys       dbmeta.Keys
	suspension *Suspended

	// actorKey wraps the database actor's key (dbmeta.Keys.Actor); nil for
	// an unencrypted database. actor is Request.Actor. unsign undoes
	// graph.SignWith. provider is what Signer resolved.
	actorKey []byte
	actor    *Actor
	unsign   func()
	signMu   sync.Mutex
	provider signer.Provider
}

// AtClose has fn run when h closes, after the database and its lock. A
// command uses it to undo, with the open, what it set up for the root.
func (h *Handle) AtClose(fn func()) {
	h.atClose = append(h.atClose, fn)
}

// runAtClose runs the AtClose functions, the last added first.
func (h *Handle) runAtClose() {
	for i := len(h.atClose) - 1; i >= 0; i-- {
		h.atClose[i]()
	}
	h.atClose = nil
}

// Errors.
var (
	// ErrNoDatabase: a read-only open found no database at the root.
	ErrNoDatabase = errors.New("no DUES database")
	// ErrInUse: another DUES process holds the database.
	ErrInUse = errors.New("database is in use by another DUES process")
	// ErrNeedsRecovery: the database was not closed cleanly and cannot be
	// repaired without write access.
	ErrNeedsRecovery = errors.New("database needs recovery")
	// ErrMarkerMismatch: the manifest file does not describe the database.
	ErrMarkerMismatch = errors.New("manifest does not match the database")
)

// homeDir is osdirs.Home; tests replace it.
var homeDir = osdirs.Home

// DefaultRoot is the data root used when no path is given, ~/.dues. It is
// not created. Without a home directory there is no default: the working
// directory would put evidence wherever the command happened to run.
func DefaultRoot() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("no default database path: %w; pass --dbpath", err)
	}
	return filepath.Join(home, ".dues"), nil
}

// ResolveRoot turns a --dbpath value into an absolute data root.
func ResolveRoot(path string) (string, error) {
	if path == "" {
		return DefaultRoot()
	}
	clean := filepath.Clean(path)
	if filepath.Base(clean) == cnst.KVDBDIR {
		clean = filepath.Dir(clean)
	}
	return filepath.Abs(clean)
}

// ChunkKiBOf is the chunk size of the database at path, from its manifest,
// for fitting a command's workers before it opens the database. It is 0 when
// no manifest can be read (a new database, one written before manifests, a
// damaged one): the open settles those.
func ChunkKiBOf(path string) int {
	root, err := ResolveRoot(path)
	if err != nil {
		return 0
	}
	m, err := dbmeta.Load(root)
	if err != nil {
		return 0
	}
	return m.Layout.ChunkKiB
}

// Open opens the database req describes.
func Open(ctx context.Context, req Request) (*Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := ResolveRoot(req.Path)
	if err != nil {
		return nil, err
	}
	h := &Handle{Root: root, ReadOnly: req.Mode == ReadOnly, tempDir: req.TempDir, tuning: cnst.DefaultTuning()}
	if req.Tuning != nil {
		h.tuning = *req.Tuning
	}
	if err := h.open(ctx, req); err != nil {
		h.release()
		return nil, err
	}
	logging.GetLogger().Info("database opened",
		zap.String("root", root), zap.Bool("read_only", h.ReadOnly), zap.Bool("created", h.Created),
		zap.String("encryption", h.Manifest.Encryption.Scheme), zap.Int("chunk_kib", h.Manifest.Layout.ChunkKiB),
		zap.String("storage", h.Manifest.Layout.Storage), zap.Strings("notices", h.Notices))
	if req.Recover && !h.ReadOnly {
		if err := h.recoverIngests(ctx); err != nil {
			return nil, errors.Join(err, h.Close())
		}
	}
	return h, nil
}

func (h *Handle) recoverIngests(ctx context.Context) error {
	report, err := store.RecoverIncompleteIngests(ctx, h.DB)
	if err != nil {
		return err
	}
	if report.MarkedFailed > 0 {
		logging.GetLogger().Warn("dbopen STARTUP_RECOVERY_MARKED_FAILED",
			zap.Int("marked_failed", report.MarkedFailed),
			zap.Int("incomplete_found", report.IncompleteFound),
		)
	}
	return nil
}

// containerAdviceKiB is the chunk size under which a new database that keeps
// a file per chunk is advised to use containers instead.
const containerAdviceKiB = 128

func (h *Handle) open(ctx context.Context, req Request) error {
	exists := dbmeta.HasDatabase(h.Root)
	if !exists && req.Mode == ReadOnly {
		return fmt.Errorf("%w at %s", ErrNoDatabase, h.Root)
	}
	if exists && req.Mode == ReadOnly && !hasRecords(h.Root) {
		// A manifest without badger's files: creating the database, or
		// resetting it, stopped part way. A writable open finishes creating it.
		return fmt.Errorf("%w at %s: it holds no stored data (creating or resetting it did not finish)", ErrNoDatabase, h.Root)
	}
	if !exists {
		for _, dir := range []string{h.Root, filepath.Join(h.Root, cnst.KVDBDIR), filepath.Join(h.Root, cnst.BLOBSDIR)} {
			if err := os.MkdirAll(dir, cnst.DirPerm); err != nil {
				return err
			}
		}
	}
	if err := h.takeLock(); err != nil {
		return err
	}
	if !h.ReadOnly {
		// Before anything is written: badger crashes the process when the
		// disk fills under it (diskspace.Reserve).
		if err := diskspace.Check(h.Root, 0); err != nil {
			return err
		}
	}
	// Another process may have created the database while we waited.
	exists = dbmeta.HasDatabase(h.Root)

	m, keys, err := h.loadManifest(ctx, req, exists)
	if err != nil {
		return err
	}
	if h.DB == nil {
		if err := h.openBadger(m, keys); err != nil {
			return err
		}
	}
	h.Manifest = m
	// A failed open releases the handle, which unregisters the engine.
	if err := h.register(keys); err != nil {
		return err
	}
	if l := m.Layout; h.Created && l.ChunkKiB < containerAdviceKiB && l.Storage == dbmeta.StorageFile {
		h.notice("the chunk size is %d KiB (under %d KiB); --container packs small chunks into container files of up to 1 GB, which suits filesystems better", l.ChunkKiB, containerAdviceKiB)
	}

	if !h.ReadOnly {
		// A copy: a legacy database's keys may be the caller's raw key, which
		// zeroing at close must not touch.
		h.keys = dbmeta.Keys{Badger: bytes.Clone(keys.Badger), Blob: bytes.Clone(keys.Blob)}
		h.sweepTemps(false)
	}
	return h.startSigning(req, keys)
}

// register checks the open badger against the manifest and registers what
// the database's readers and writers look up by it.
func (h *Handle) register(keys dbmeta.Keys) error {
	if err := h.checkMarker(); err != nil {
		return err
	}
	eng, err := engine.Register(h.DB, h.Manifest.Layout, h.tuning)
	if err != nil {
		return err
	}
	codec, err := codecFor(h.Manifest.Layout, keys, eng.Encoder())
	if err != nil {
		return err
	}
	blobcodec.Register(h.DB, codec)
	dbmeta.RegisterManifest(h.DB, h.Manifest)
	h.keepBlockIndexLookup()
	return nil
}

// hasRecords reports whether the root's badger directory holds a database.
func hasRecords(root string) bool {
	_, err := os.Stat(filepath.Join(root, cnst.KVDBDIR, "MANIFEST"))
	return err == nil
}

// sweepTemps removes the temporary files a DUES process killed while writing
// in the root left there: the manifest's, its mirror's, the graph marker's,
// replaced blobs', and the part files of uploads a server was receiving. A
// writable open holds the root lock exclusively, so no other DUES process is
// writing them, whatever their age. keepUploads keeps the part files, for a
// server resuming with uploads still coming in.
func (h *Handle) sweepTemps(keepUploads bool) {
	for _, dir := range []string{h.Root, filepath.Join(h.Root, cnst.KVDBDIR), filepath.Join(h.Root, cnst.BLOBSDIR), filepath.Join(h.Root, cnst.GRAPHDIR)} {
		if n, err := fsx.SweepTemps(dir, 0); err != nil {
			logging.GetLogger().Warn("dbopen temp sweep failed", zap.String("dir", dir), zap.Error(err))
		} else if n > 0 {
			logging.GetLogger().Info("dbopen removed stale temp files", zap.String("dir", dir), zap.Int("count", n))
		}
	}
	if keepUploads {
		return
	}
	parts, _ := filepath.Glob(filepath.Join(h.Root, cnst.UploadsDir, "*.part"))
	for _, p := range parts {
		if err := fsx.Remove(p); err != nil {
			logging.GetLogger().Warn("dbopen could not remove an upload part file", zap.String("path", p), zap.Error(err))
		}
	}
}

func (h *Handle) notice(format string, args ...any) {
	h.Notices = append(h.Notices, fmt.Sprintf(format, args...))
}

func (h *Handle) takeLock() error {
	l, err := fsx.TryLock(filepath.Join(h.Root, LockName), h.ReadOnly)
	switch {
	case err == nil:
		h.lock = l
		return nil
	case errors.Is(err, fsx.ErrLocked):
		return fmt.Errorf("%w (%s)", ErrInUse, h.Root)
	case h.ReadOnly && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)):
		// Read-only media: the lock file cannot be created, and nothing can
		// write here anyway (CFG-12).
		h.notice("data root %s is not writable; opening without a lock", h.Root)
		return nil
	}
	return err
}

// loadManifest returns the manifest and keys: from the manifest file, by
// inference for a legacy database, or newly made for a new one.
func (h *Handle) loadManifest(ctx context.Context, req Request, exists bool) (dbmeta.Manifest, dbmeta.Keys, error) {
	m, err := dbmeta.Load(h.Root)
	switch {
	case err == nil:
		if err := checkRequested(h.Root, m, req.Layout); err != nil {
			return m, dbmeta.Keys{}, err
		}
		keys, err := dbmeta.DeriveKeys(m.Encryption, req.Creds)
		if err != nil {
			return m, keys, h.keyError(err, req.Creds)
		}
		if !m.Encrypted() && req.Creds.IsSet() {
			h.notice("database %s is not encrypted; the password was not used", h.Root)
		}
		return m, keys, nil
	case !errors.Is(err, dbmeta.ErrNoManifest):
		return m, dbmeta.Keys{}, fmt.Errorf("reading the manifest of %s: %w", h.Root, err)
	case exists:
		return h.inferLegacy(ctx, req)
	}
	return h.create(req)
}

func (h *Handle) keyError(err error, creds dbmeta.Credentials) error {
	switch {
	case errors.Is(err, dbmeta.ErrPasswordRequired), errors.Is(err, dbmeta.ErrWrongPassword) && !creds.IsSet():
		return &lifecycle.UsageError{Err: &credentialError{
			msg: fmt.Sprintf("database %s is encrypted; give its password with --password-file or --password-stdin", h.Root), cause: dbmeta.ErrPasswordRequired}}
	case errors.Is(err, dbmeta.ErrWrongPassword):
		return &lifecycle.UsageError{Err: &credentialError{
			msg: fmt.Sprintf("wrong password for database %s", h.Root), cause: dbmeta.ErrWrongPassword}}
	}
	return err
}

// credentialError is a usage message that still matches the dbmeta error it
// stands for, so a caller other than the CLI can tell a missing password from
// a wrong one.
type credentialError struct {
	msg   string
	cause error
}

func (e *credentialError) Error() string { return e.msg }
func (e *credentialError) Unwrap() error { return e.cause }

// create makes the manifest for a new database and writes it before badger
// exists: a database whose keys depend on a salt must never exist without it.
func (h *Handle) create(req Request) (dbmeta.Manifest, dbmeta.Keys, error) {
	r := req.Layout
	kdf := dbmeta.DefaultKDF
	if req.KDF != nil {
		kdf = *req.KDF
	}
	enc, keys, err := dbmeta.NewEncryption(req.Creds, kdf)
	if err != nil {
		return dbmeta.Manifest{}, keys, err
	}
	l := dbmeta.Layout{
		ChunkKiB:          r.ChunkKiB,
		ChunkHash:         r.ChunkHash,
		Storage:           storageOf(r.Container),
		BlobEncoding:      blobcodec.V2,
		ValueEncoding:     dbmeta.EncodingZstd,
		BadgerCompression: dbmeta.EncodingZstd,
	}
	if l.ChunkKiB == 0 {
		l.ChunkKiB = 256
	}
	if l.ChunkHash == "" {
		l.ChunkHash = dbmeta.HashBLAKE3
	}
	if r.Quick {
		l.BadgerCompression = dbmeta.EncodingNone
	}
	m := dbmeta.Manifest{
		Format: dbmeta.FormatName, Version: dbmeta.FormatVersion, Created: time.Now().UTC(),
		WrittenBy: req.WrittenBy, Layout: l, Encryption: enc,
	}
	if err := m.Validate(); err != nil {
		return m, keys, err
	}
	if err := dbmeta.Save(h.Root, m); err != nil {
		return m, keys, err
	}
	h.Created = true
	if !m.Encrypted() {
		h.notice("created database %s without encryption (no password given)", h.Root)
	}
	return m, keys, nil
}

// storageOf is the storage of a new database. Hierarchical storage is retired
// (D9); databases that have it keep it.
func storageOf(container bool) string {
	if container {
		return dbmeta.StorageContainer
	}
	return dbmeta.StorageFile
}

// inferLegacy reconstructs the manifest of a database written before
// manifests existed. A writable open persists it; a read-only one does not.
func (h *Handle) inferLegacy(ctx context.Context, req Request) (dbmeta.Manifest, dbmeta.Keys, error) {
	kvdb := filepath.Join(h.Root, cnst.KVDBDIR)
	enc, base, err := dbmeta.LegacyEncryption(kvdb)
	if err != nil {
		return dbmeta.Manifest{}, dbmeta.Keys{}, fmt.Errorf("%s: %w", h.Root, err)
	}
	keys, err := dbmeta.DeriveKeys(enc, req.Creds)
	if err != nil {
		return dbmeta.Manifest{}, keys, err
	}
	if enc.Scheme == dbmeta.SchemeSHA3Legacy {
		if err := dbmeta.CheckLegacyKey(kvdb, keys.Badger); err != nil {
			return dbmeta.Manifest{}, keys, h.keyError(err, req.Creds)
		}
	} else if req.Creds.IsSet() {
		h.notice("database %s is not encrypted (written in quick mode); the password was not used", h.Root)
	}

	// Inference reads badger; the handle stays open for the caller.
	if err := h.openBadger(dbmeta.Manifest{Layout: base}, keys); err != nil {
		return dbmeta.Manifest{}, keys, err
	}
	if err := ctx.Err(); err != nil {
		return dbmeta.Manifest{}, keys, err
	}
	l, inf := dbmeta.InferLayout(h.DB, h.Root, base)

	r := req.Layout
	if l.ChunkKiB == 0 {
		l.ChunkKiB = orDefault(r.ChunkKiB, 256)
	}
	if l.ChunkHash == "" {
		l.ChunkHash = orDefault(r.ChunkHash, dbmeta.HashBLAKE3)
	}
	m := dbmeta.Manifest{
		Format: dbmeta.FormatName, Version: dbmeta.FormatVersion, Created: inf.At,
		WrittenBy: req.WrittenBy, Layout: l, Encryption: enc, Inferred: &inf,
	}
	if err := m.Validate(); err != nil {
		return m, keys, fmt.Errorf("inferring the layout of %s: %w", h.Root, err)
	}
	// An explicit flag cannot override what the data shows, but it does
	// settle a field the data could not show.
	if err := checkRequested(h.Root, m, withoutUnverified(r, inf.Unverified)); err != nil {
		return m, keys, err
	}
	for _, f := range inf.Notes {
		h.notice("%s: %s", h.Root, f)
	}
	if !h.ReadOnly {
		if err := dbmeta.Save(h.Root, m); err != nil {
			return m, keys, err
		}
	}
	return m, keys, nil
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

func withoutUnverified(r Requested, unverified []string) Requested {
	set := map[string]bool{}
	for k, v := range r.Set {
		set[k] = v
	}
	if slices.Contains(unverified, "chunk_kib") {
		delete(set, cnst.FlagChonkSize)
	}
	if slices.Contains(unverified, "chunk_hash") {
		delete(set, cnst.FlagHashAlgo)
	}
	r.Set = set
	return r
}

// checkRequested rejects explicit flags that contradict an existing layout.
func checkRequested(root string, m dbmeta.Manifest, r Requested) error {
	var errs []error
	l := m.Layout
	if r.Set[cnst.FlagChonkSize] && r.ChunkKiB != l.ChunkKiB {
		errs = append(errs, fmt.Errorf("--%s %d: database %s uses %d KiB chunks; omit --%s",
			cnst.FlagChonkSize, r.ChunkKiB, root, l.ChunkKiB, cnst.FlagChonkSize))
	}
	if r.Set[cnst.FlagHashAlgo] && !strings.EqualFold(r.ChunkHash, l.ChunkHash) {
		errs = append(errs, fmt.Errorf("--%s %s: database %s uses %s; omit --%s",
			cnst.FlagHashAlgo, r.ChunkHash, root, l.ChunkHash, cnst.FlagHashAlgo))
	}
	if r.Set[cnst.FlagContainerMode] && r.Container != (l.Storage != dbmeta.StorageFile) {
		errs = append(errs, fmt.Errorf("--%s: database %s uses %s storage; omit --%s",
			cnst.FlagContainerMode, root, l.Storage, cnst.FlagContainerMode))
	}
	if len(errs) > 0 {
		return &lifecycle.UsageError{Err: errors.Join(errs...)}
	}
	return nil
}

func compressionOf(l dbmeta.Layout) options.CompressionType {
	switch l.BadgerCompression {
	case dbmeta.EncodingZstd:
		return options.ZSTD
	case dbmeta.EncodingSnappy:
		return options.Snappy
	}
	return options.None
}

func (h *Handle) badgerOptions(l dbmeta.Layout, keys dbmeta.Keys, readOnly bool) badger.Options {
	opts := dbio.BadgerOptions(filepath.Join(h.Root, cnst.KVDBDIR), keys.Badger, compressionOf(l), h.tuning)
	if readOnly {
		opts.ReadOnly = true
		// badger has no read-only directory lock on Windows; the shared root
		// lock keeps writers out instead.
		opts.BypassLockGuard = runtime.GOOS == "windows"
		// badger replaces a data key older than the rotation period and
		// writes the new one to KEYREGISTRY. A read-only open cannot, and
		// badger then ends the process (CFG-14). Writable opens rotate.
		opts.EncryptionKeyRotationDuration = math.MaxInt64
	}
	return opts
}

func (h *Handle) badgerError(err error) error {
	switch {
	case errors.Is(err, badger.ErrEncryptionKeyMismatch):
		return lifecycle.Usagef("wrong password for database %s", h.Root)
	case strings.Contains(err.Error(), "Another process is using this Badger database"),
		strings.Contains(err.Error(), "resource temporarily unavailable"):
		return fmt.Errorf("%w (%s)", ErrInUse, h.Root)
	}
	return fmt.Errorf("opening database %s: %w", h.Root, err)
}

// openBadger opens badger. A read-only open of a database that was not closed
// cleanly switches to a writable open, so badger can recover, when the root
// is writable.
func (h *Handle) openBadger(m dbmeta.Manifest, keys dbmeta.Keys) error {
	opts := h.badgerOptions(m.Layout, keys, h.ReadOnly)
	// A database whose process died still has its memtable logs, which
	// badger must truncate before it opens read-only. A read-only open that
	// fails on them leaves a log mapped, and Windows refuses to truncate a
	// mapped file, so the logs are looked for before badger is asked.
	if h.ReadOnly && hasMemtableLogs(opts.Dir) {
		if !h.upgradeLock() {
			return fmt.Errorf("%w: %s was not closed cleanly; run `dues repair --fix` with write access", ErrNeedsRecovery, h.Root)
		}
		h.notice("database %s was not closed cleanly; recovered it", h.Root)
		h.ReadOnly = false
		opts = h.badgerOptions(m.Layout, keys, false)
	}
	realDir := opts.Dir
	if h.ReadOnly && !discardWritable(opts.Dir) {
		shadow, copied, err := newShadow(opts.Dir, h.tempDir)
		if err != nil {
			return fmt.Errorf("opening %s from read-only media: %w", h.Root, err)
		}
		h.shadow = shadow
		opts.Dir, opts.ValueDir = shadow, shadow
		if copied > 0 {
			h.notice("database %s is on read-only media; %s of its index was copied to a temporary directory to open it", h.Root, humanize.IBytes(uint64(copied)))
		}
	}
	db, err := badger.Open(opts)
	if err != nil && h.ReadOnly && truncateNeeded(err) {
		if !h.upgradeLock() {
			return fmt.Errorf("%w: %s was not closed cleanly; run `dues repair --fix` with write access", ErrNeedsRecovery, h.Root)
		}
		h.notice("database %s was not closed cleanly; recovered it", h.Root)
		h.ReadOnly = false
		db, err = badger.Open(h.badgerOptions(m.Layout, keys, false))
	}
	if err != nil {
		return h.badgerError(err)
	}
	h.DB = db
	if h.shadow != "" {
		dbio.RegisterKVDir(db, realDir)
	}
	return nil
}

// discardWritable reports whether badger can open its DISCARD file for
// writing, which it does even for a read-only open: on write-protected media,
// or in a directory the user may only read, it cannot, and badger then ends
// the process (log.Fatalf) instead of returning an error. Opening the file
// for writing changes nothing in it.
func discardWritable(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, "DISCARD"), os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		// Every database badger has opened has one; without it badger
		// creates one, which needs the directory writable, and says so.
		return true
	}
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// newShadow makes a temporary badger directory holding every file of dir
// but DISCARD, so badger can open a read-only database whose own directory
// it cannot write: it gets a DISCARD of its own there. The files are linked
// where the platform allows it and copied otherwise; since badger v4.9.2 a
// read-only open opens its value logs read-only too, so links to them serve.
// copied counts the bytes copied. Removing the directory removes only the
// links and copies. The directory goes in tempDir, or osdirs.Temp when it
// is empty.
func newShadow(dir, tempDir string) (shadow string, copied int64, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, err
	}
	base, err := osdirs.TempOr(tempDir)
	if err != nil {
		return "", 0, err
	}
	shadow, err = os.MkdirTemp(base, "dues-readonly-")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(shadow)
		}
	}()
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || name == "DISCARD" || name == "LOCK" {
			continue
		}
		src, dst := filepath.Join(dir, name), filepath.Join(shadow, name)
		if os.Symlink(src, dst) == nil {
			continue
		}
		n, err := copyFile(src, dst)
		if err != nil {
			return "", 0, err
		}
		copied += n
	}
	return shadow, copied, nil
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// truncateNeeded reports whether badger refused an open because its log must
// be truncated, as after a crash. badger wraps the error with %+v outside its
// debug mode, so errors.Is cannot see it (CFG-13); its text is matched too.
func truncateNeeded(err error) bool {
	return errors.Is(err, badger.ErrTruncateNeeded) || strings.Contains(err.Error(), badger.ErrTruncateNeeded.Error())
}

// hasMemtableLogs reports whether the badger directory holds memtable logs.
// badger removes them when it closes, so they are left only by a process
// that did not close the database.
func hasMemtableLogs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".mem") {
			return true
		}
	}
	return false
}

// upgradeLock swaps the shared lock for an exclusive one. When another
// process holds the lock too, the shared lock is taken back and false
// returned.
func (h *Handle) upgradeLock() bool {
	if h.lock == nil {
		return false
	}
	if err := h.lock.Unlock(); err != nil {
		return false
	}
	h.lock = nil
	path := filepath.Join(h.Root, LockName)
	l, err := fsx.TryLock(path, false)
	if err != nil {
		if l, err := fsx.TryLock(path, true); err == nil {
			h.lock = l
		}
		return false
	}
	h.lock = l
	return true
}

// checkMarker compares the manifest with the copy inside badger, and writes
// that copy (and a missing manifest file) on a writable open.
func (h *Handle) checkMarker() error {
	var stored []byte
	err := h.DB.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(dbmeta.MarkerKey))
		if err != nil {
			return err
		}
		stored, err = item.ValueCopy(nil)
		return err
	})
	switch {
	case err == nil:
		var marker dbmeta.Manifest
		if err := json.Unmarshal(stored, &marker); err != nil {
			return fmt.Errorf("%w: unreadable marker in %s: %v", ErrMarkerMismatch, h.Root, err)
		}
		if marker.Version > dbmeta.FormatVersion {
			return fmt.Errorf("%s: %w", h.Root, dbmeta.ErrNewerFormat)
		}
		if !dbmeta.SameLayout(marker, h.Manifest) {
			return fmt.Errorf("%w: %s; restore %s from %s",
				ErrMarkerMismatch, h.Root, dbmeta.ManifestName, dbmeta.MirrorName)
		}
		return nil
	case !errors.Is(err, badger.ErrKeyNotFound):
		return err
	case h.ReadOnly:
		return nil
	}
	b, err := json.Marshal(h.Manifest)
	if err != nil {
		return err
	}
	return h.DB.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(dbmeta.MarkerKey), b)
	})
}

// codecFor returns the blob codec of a database of layout l, compressing
// with enc, the database's encoder.
func codecFor(l dbmeta.Layout, keys dbmeta.Keys, enc *zstdx.Encoder) (blobcodec.Codec, error) {
	switch l.BlobEncoding {
	case blobcodec.LegacySealed:
		return blobcodec.NewLegacySealed(keys.Blob, enc), nil
	case blobcodec.LegacyRaw:
		return blobcodec.NewLegacyRaw(), nil
	case blobcodec.V2:
		return blobcodec.NewV2(keys.Blob, enc)
	}
	return nil, fmt.Errorf("unknown blob encoding %q", l.BlobEncoding)
}

func (h *Handle) release() {
	if h.DB != nil {
		unregister(h.DB)
		h.DB.Close()
		h.DB = nil
	}
	h.removeShadow()
	if h.lock != nil {
		h.lock.Unlock()
		h.lock = nil
	}
	h.forgetKeys()
	h.stopSigning()
	h.runAtClose()
}

func (h *Handle) forgetKeys() {
	clear(h.keys.Badger)
	clear(h.keys.Blob)
	h.keys = dbmeta.Keys{}
}

// unregister forgets what the open registered for db, as it closes.
func unregister(db *badger.DB) {
	blobcodec.Unregister(db)
	dbmeta.UnregisterLayout(db)
	engine.Unregister(db)
	dbio.UnregisterKVDir(db)
}

func (h *Handle) removeShadow() {
	if h.shadow != "" {
		_ = os.RemoveAll(h.shadow)
		h.shadow = ""
	}
}

// Close closes the database, then releases the root lock. On a suspended
// handle it ends the suspension, as Suspended.Release.
func (h *Handle) Close() error {
	if h == nil || h.DB == nil && h.suspension == nil {
		return nil
	}
	var err error
	if db := h.DB; db != nil {
		h.DB = nil
		unregister(db)
		err = db.Close()
	}
	h.suspension = nil
	h.removeShadow()
	if h.lock != nil {
		err = errors.Join(err, h.lock.Unlock())
		h.lock = nil
	}
	h.forgetKeys()
	h.stopSigning()
	h.runAtClose()
	return err
}
