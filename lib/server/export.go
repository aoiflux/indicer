package server

// Exports. A token with the export scope asks for a package of the
// database: the server pauses the database, writes <id>.duesarc beside its
// sidecar <id>.json in the export folder, and serves it to that token until
// it expires. docs/SERVER_API.md describes the protocol.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"indicer/internal/configfile"
	"indicer/internal/diskspace"
	"indicer/internal/duesarc"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/dbpackage"
	"indicer/lib/fsx"

	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The ranges of the export settings.
const (
	DefaultExportKeepHours = 24
	MinExportKeepHours     = 1
	MaxExportKeepHours     = 30 * 24
	MinDrainTimeout        = time.Second
	MaxDrainTimeout        = time.Hour
)

// Export states.
const (
	exportStatePausing   = "pausing"
	exportStatePackaging = "packaging"
	exportStateDone      = "done"
	exportStateFailed    = "failed"
	exportStateCancelled = "cancelled"
)

const (
	// partialSuffix marks a package written and synced but not yet published:
	// it gets its name only after its sidecar exists.
	partialSuffix   = ".partial"
	sidecarSuffix   = ".json"
	exportsLockName = ".dues-exports.lock"
	sidecarVersion  = 1
	sidecarIndent   = "  "
	// maxSidecarSize bounds the read of a sidecar; one is a few hundred bytes.
	maxSidecarSize   = 64 << 10
	pathValueID      = "id"
	downloadPath     = "/web/export/download/"
	exportPauseFor   = "export "
	logFieldExportID = "export_id"

	headerETag               = "ETag"
	headerReprDigest         = "Repr-Digest"
	headerContentDisposition = "Content-Disposition"
	headerRange              = "Range"
	contentTypeZip           = "application/zip"
	dispositionAttachment    = "attachment"
	dispositionFilename      = "filename"
	etagFormat               = `"sha256-%x"`
	reprDigestFormat         = "sha-256=:%s:"
)

// Reasons in the "export removed" record.
const (
	removedExpired   = "expired"
	removedCancelled = "cancelled"
	removedLeftOver  = "left by an earlier run"
	removedOrphan    = "orphan"
	removedInvalid   = "unreadable sidecar"
)

const (
	msgExportRunning     = "an export is running, or the database is resuming after one; one export runs at a time"
	msgExportsKept       = "%d exports are kept, the most this server keeps; cancel one, or wait for one to expire"
	msgExportIDRequired  = "export_id is required"
	msgExportNotFound    = "export not found or expired"
	msgExportNotDone     = "the export is %s; only a written export can be downloaded"
	msgExportDownloading = "the export is being downloaded; cancel it once the download ends"
	msgDownloadsBusy     = "%d downloads are running; retry when one ends"
	msgExportNoRoom      = "%d bytes are free for exports; this one needs about %d"
	msgExportSizing      = "the free space for the export could not be checked"
	msgExportAbandoned   = "the request ended before the export started"
	msgExportPausing     = "another pause of the database is running; retry when it ends"
	msgExportBusy        = "work on the database did not stop in time; retry later"
	msgExportStopped     = "the server stopped before the export was written"
	msgExportUnclean     = "the database did not close cleanly and was recovered; export again"
	msgExportNoSpace     = "not enough free space for the package"
	msgExportInternal    = "the export could not be written (export %s)"
	msgExportUnreadable  = "the package could not be read (export %s)"
	msgExportNotRemoved  = "the package could not be removed (export %s); retry later"
)

var errExportCancelled = errors.New("the export was cancelled")

type exportJob struct {
	id, owner      string
	withoutDerived bool
	created        time.Time
	ctx            context.Context // nil for a job adopted at startup
	cancel         context.CancelCauseFunc
	written        atomic.Int64
	expected       atomic.Int64
	// The job's goroutine only.
	stats *duesarc.Stats
	began time.Time

	// Under exports.mu.
	state                  string
	finished, expires      time.Time
	size                   int64
	sum                    [sha256.Size]byte
	content, signedBy, kid string
	plaintext              []string
	errMsg                 string
	retryable              bool
	downloads              int
}

// exports runs the server's exports, one at a time, and keeps the packages
// they wrote.
type exports struct {
	s      *Server
	cfg    ExportConfig // Dir absolute; Keep and Drain filled
	lim    ExportLimits
	dir    string
	inRoot bool
	root   string    // for the sidecar's identity
	made   time.Time // the database manifest's Created
	lock   *fsx.Lock // nil when inRoot
	slots  chan struct{}

	mu   sync.Mutex
	busy bool
	jobs map[string]*exportJob
	// running is the export pausing or packaging, read without mu: upload
	// start reads it under its own lock, and mu is held across file I/O.
	running atomic.Pointer[exportJob]

	// Seams for tests.
	now        func() time.Time
	estimate   func(ctx context.Context, root string, withoutDerived bool) (uint64, error)
	checkSpace func(path string, need uint64) error
	export     func(context.Context, dbpackage.ExportOptions) (dbpackage.ExportResult, error)
	rename     func(src, dst string, overwrite bool) error
	open       func(name string) (io.ReadSeekCloser, error)

	reaperDone chan struct{}
}

// exportSidecar is <id>.json: who made the package, of which database, and
// until when it is kept. It holds a token's label, never a token.
type exportSidecar struct {
	Version             int       `json:"version"`
	ExportID            string    `json:"export_id"`
	Owner               string    `json:"owner"`
	DatabaseRoot        string    `json:"database_root"`
	DatabaseCreated     time.Time `json:"database_created"`
	CreatedAt           time.Time `json:"created_at"`
	DoneAt              time.Time `json:"done_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	WithoutDerived      bool      `json:"without_derived"`
	Size                int64     `json:"size"`
	SHA256              string    `json:"sha256"`
	ContentSHA256       string    `json:"content_sha256"`
	SignedBy            string    `json:"signed_by,omitempty"`
	KeyID               string    `json:"key_id,omitempty"`
	PlaintextComponents []string  `json:"plaintext_components,omitempty"`
}

// exportStatus is what status says about an export. Its errors name no
// server path: Export's notices, which may, go to the log only.
type exportStatus struct {
	ExportID            string    `json:"export_id"`
	State               string    `json:"state"`
	WithoutDerived      bool      `json:"without_derived"`
	CreatedAt           time.Time `json:"created_at"`
	BytesWritten        int64     `json:"bytes_written"`
	BytesExpected       int64     `json:"bytes_expected,omitempty"`
	Size                int64     `json:"size,omitempty"`
	SHA256              string    `json:"sha256,omitempty"`
	ContentSHA256       string    `json:"content_sha256,omitempty"`
	SignedBy            string    `json:"signed_by,omitempty"`
	KeyID               string    `json:"key_id,omitempty"`
	PlaintextComponents []string  `json:"plaintext_components,omitempty"`
	Download            string    `json:"download,omitempty"`
	ExpiresAt           time.Time `json:"expires_at,omitzero"`
	Error               string    `json:"error,omitempty"`
	Retryable           bool      `json:"retryable,omitempty"`
}

type exportIDReq struct {
	ExportID string `json:"export_id"`
}

// byteCounter counts what the package writer writes, for progress.
type byteCounter struct{ n *atomic.Int64 }

func (c byteCounter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// CheckExportKeepHours checks --export-keep-hours.
func CheckExportKeepHours(h int) error {
	if h < MinExportKeepHours || h > MaxExportKeepHours {
		return fmt.Errorf("must be from %d to %d hours, not %d", MinExportKeepHours, MaxExportKeepHours, h)
	}
	return nil
}

// ParseDrainTimeout parses --export-drain-timeout, a Go duration such as
// 90s or 10m.
func ParseDrainTimeout(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 90s or 10m", s)
	}
	return d, checkDrain(d)
}

func checkDrain(d time.Duration) error {
	if d < MinDrainTimeout || d > MaxDrainTimeout {
		return fmt.Errorf("must be from %v to %v, not %v", MinDrainTimeout, MaxDrainTimeout, d)
	}
	return nil
}

// CheckExportDir checks --export-dir against the data root, by its spelling,
// before the database opens: "" is the root's exports folder; any other
// folder is absolute, and inside the root only that folder, spelled exactly,
// or one below it may hold packages, which leave it out. The server checks
// again with links resolved when it starts.
func CheckExportDir(root, dir string) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("export folder %q is not an absolute path", dir)
	}
	rel, err := filepath.Rel(root, filepath.Clean(dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	if top, _, _ := strings.Cut(filepath.ToSlash(rel), "/"); top != cnst.ExportsDir {
		return fmt.Errorf("export folder %s is inside the database %s; only its %s folder, or one in it, may hold exports", dir, root, cnst.ExportsDir)
	}
	return nil
}

func (e ExportConfig) check() error {
	if e.Dir != "" && !filepath.IsAbs(e.Dir) {
		return fmt.Errorf("export folder %q is not an absolute path", e.Dir)
	}
	if e.Keep != 0 {
		if e.Keep%time.Hour != 0 {
			return fmt.Errorf("export keep time %v is not a whole number of hours", e.Keep)
		}
		if err := CheckExportKeepHours(int(e.Keep / time.Hour)); err != nil {
			return fmt.Errorf("export keep time: %w", err)
		}
	}
	if e.Drain != 0 {
		if err := checkDrain(e.Drain); err != nil {
			return fmt.Errorf("export drain timeout: %w", err)
		}
	}
	return nil
}

// validExportID reports whether id is a UUID in the one form the server
// makes, so a file name built from it is one of the server's.
func validExportID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// newExports prepares the export folder and takes up what an earlier run
// left there (sweep).
func newExports(s *Server, h *dbopen.Handle) (*exports, error) {
	cfg := s.cfg.Exports
	cfg.Keep = cmp.Or(cfg.Keep, DefaultExportKeepHours*time.Hour)
	cfg.Drain = cmp.Or(cfg.Drain, DefaultDrainTimeout)
	lim, def := s.cfg.Limits.Export, DefaultExportLimits()
	lim.MaxDownloads = cmp.Or(lim.MaxDownloads, def.MaxDownloads)
	lim.DownloadIdle = cmp.Or(lim.DownloadIdle, def.DownloadIdle)
	lim.SizingBudget = cmp.Or(lim.SizingBudget, def.SizingBudget)
	lim.MaxKept = cmp.Or(lim.MaxKept, def.MaxKept)
	lim.DoneTTL = cmp.Or(lim.DoneTTL, def.DoneTTL)
	lim.ReapInterval = cmp.Or(lim.ReapInterval, def.ReapInterval)
	dir := cmp.Or(cfg.Dir, filepath.Join(s.root, cnst.ExportsDir))
	cfg.Dir = dir
	if err := CheckExportDir(s.root, dir); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	if err := os.MkdirAll(dir, cnst.DirPerm); err != nil {
		return nil, fmt.Errorf("server: export folder: %w", err)
	}
	inRoot, err := exportFolderInRoot(s.root, dir)
	if err != nil {
		return nil, err
	}
	ex := &exports{
		s: s, cfg: cfg, lim: lim, dir: dir, inRoot: inRoot, root: s.root, made: h.Manifest.Created,
		slots: make(chan struct{}, lim.MaxDownloads),
		jobs:  map[string]*exportJob{},
		now:   time.Now, estimate: dbpackage.EstimateNeed, checkSpace: diskspace.Check, export: dbpackage.Export,
		rename: fsx.ReplaceFile,
		open:   func(name string) (io.ReadSeekCloser, error) { return os.Open(name) },
	}
	// The root's lock already keeps a folder inside it to this server.
	if !inRoot {
		if ex.lock, err = lockExportFolder(dir); err != nil {
			return nil, err
		}
	}
	notes, err := ex.sweep()
	if err != nil {
		return nil, errors.Join(err, ex.lock.Unlock())
	}
	s.Notes = append(s.Notes, notes...)
	for _, c := range s.cfg.Tokens {
		if c.scopes&ScopeExport != 0 {
			s.Notes = append(s.Notes, fmt.Sprintf("exports go to %s and are kept %d h", dir, int(cfg.Keep/time.Hour)))
			break
		}
	}
	return ex, nil
}

// exportFolderInRoot checks the export folder with links resolved, so a
// link outside the root that leads into it is refused: a package must never
// hold packages.
func exportFolderInRoot(root, dir string) (bool, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false, fmt.Errorf("server: data root: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, fmt.Errorf("server: export folder: %w", err)
	}
	inRoot, err := dbpackage.CheckOutputDir(realRoot, realDir)
	if err != nil {
		return false, fmt.Errorf("server: %w", err)
	}
	return inRoot, nil
}

// lockExportFolder holds a folder outside the root for this server: another
// server sharing it would sweep this one's files. Packages of an
// unencrypted database hold evidence in the clear, so a folder another
// account can change, and swap a package in, is refused.
func lockExportFolder(dir string) (*fsx.Lock, error) {
	root, err := databaseAbove(dir)
	if err != nil {
		return nil, err
	}
	if root != "" {
		return nil, fmt.Errorf("server: export folder %s is inside the database %s; use a folder outside every database", dir, root)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("server: export folder: %w", err)
	}
	if msg := configfile.CheckAccess(info); msg != "" {
		return nil, fmt.Errorf("server: export folder %s %s", dir, msg)
	}
	lock, err := fsx.TryLock(filepath.Join(dir, exportsLockName), false)
	if errors.Is(err, fsx.ErrLocked) {
		return nil, fmt.Errorf("server: export folder %s is in use by another DUES server", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("server: export folder: %w", err)
	}
	return lock, nil
}

// databaseAbove is the data root that holds dir, found by the lock file
// every opened database keeps, or "". That root's server sweeps its own
// exports folder without the folder lock, so the two would remove each
// other's files.
func databaseAbove(dir string) (string, error) {
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("server: export folder: %w", err)
	}
	for {
		info, err := os.Lstat(filepath.Join(p, dbopen.LockName))
		switch {
		case err == nil && info.Mode().IsRegular():
			return p, nil
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("server: export folder: %w", err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", nil
		}
		p = parent
	}
}

func (ex *exports) file(id, suffix string) string {
	return filepath.Join(ex.dir, id+suffix)
}

// exportIDOf is the export a file name of the folder belongs to: <id> plus
// suffix, with a canonical id.
func exportIDOf(name, suffix string) (string, bool) {
	id, ok := strings.CutSuffix(name, suffix)
	return id, ok && validExportID(id)
}

// sweep takes up what an earlier run left in the folder. No export runs
// yet, so unfinished packages and their temporary files go, as do halves of
// pairs; finished pairs of this database are served again until they
// expire; another database's are left alone, as is any other file.
func (ex *exports) sweep() ([]string, error) {
	entries, err := os.ReadDir(ex.dir)
	if err != nil {
		return nil, fmt.Errorf("server: export folder: %w", err)
	}
	log := ex.s.log
	removed, kept, foreign, failed := 0, 0, 0, 0
	drop := func(id, reason string, names ...string) {
		for _, n := range names {
			if !ex.removeFile(id, filepath.Join(ex.dir, n)) {
				failed++
				return
			}
		}
		log.Info("export removed", zap.String(logFieldExportID, id), zap.String("reason", reason))
		if reason != removedExpired {
			removed++
		}
	}
	pkgs := map[string]os.DirEntry{}
	sides := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if target, ok := fsx.TempTarget(name); ok {
			id, ours := exportIDOf(target, dbpackage.Ext+partialSuffix)
			if !ours {
				id, ours = exportIDOf(target, sidecarSuffix)
			}
			if ours {
				drop(id, removedLeftOver, name)
			}
			continue
		}
		if id, ok := exportIDOf(name, dbpackage.Ext+partialSuffix); ok {
			drop(id, removedLeftOver, name)
			continue
		}
		if id, ok := exportIDOf(name, dbpackage.Ext); ok {
			pkgs[id] = e
			continue
		}
		if id, ok := exportIDOf(name, sidecarSuffix); ok {
			sides[id] = true
		}
	}
	for id := range sides {
		if _, ok := pkgs[id]; !ok {
			drop(id, removedOrphan, id+sidecarSuffix)
		}
	}
	now := ex.now()
	for id, e := range pkgs {
		pkg, side := id+dbpackage.Ext, id+sidecarSuffix
		if !sides[id] {
			drop(id, removedOrphan, pkg)
			continue
		}
		info, err := e.Info()
		if err != nil {
			log.Warn("export not read", zap.String(logFieldExportID, id), zap.Error(err))
			drop(id, removedInvalid, pkg, side)
			continue
		}
		sc, err := readSidecar(filepath.Join(ex.dir, side))
		var sum [sha256.Size]byte
		if err == nil {
			sum, err = sc.check(id, info.Size())
		}
		switch {
		case err != nil:
			log.Warn("export sidecar not valid", zap.String(logFieldExportID, id), zap.Error(err))
			drop(id, removedInvalid, pkg, side)
		case !ex.ours(sc):
			foreign++
		case !now.Before(sc.ExpiresAt):
			drop(id, removedExpired, pkg, side)
		default:
			ex.adopt(sc, sum)
			kept++
		}
	}
	var notes []string
	if removed > 0 {
		notes = append(notes, fmt.Sprintf("removed %d unfinished or orphaned exports left by an earlier run", removed))
	}
	if kept > 0 {
		notes = append(notes, fmt.Sprintf("kept %d exports of an earlier run until they expire", kept))
	}
	if foreign > 0 {
		notes = append(notes, fmt.Sprintf("left %d exports of another database in %s", foreign, ex.dir))
	}
	if failed > 0 {
		notes = append(notes, fmt.Sprintf("could not remove %d exports left in %s; see the log", failed, ex.dir))
	}
	return notes, nil
}

func readSidecar(path string) (exportSidecar, error) {
	var sc exportSidecar
	info, err := os.Stat(path)
	if err != nil {
		return sc, err
	}
	if info.Size() > maxSidecarSize {
		return sc, fmt.Errorf("%s is larger than %d bytes", path, maxSidecarSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	if err := json.Unmarshal(data, &sc); err != nil {
		return sc, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

// check reports whether the sidecar describes the package of id, of size
// bytes, as this version writes it, and returns the package's SHA-256.
func (sc exportSidecar) check(id string, size int64) ([sha256.Size]byte, error) {
	sum, err := decodeSum(sc.SHA256)
	switch {
	case sc.Version != sidecarVersion:
		return sum, fmt.Errorf("sidecar version %d, not %d", sc.Version, sidecarVersion)
	case sc.ExportID != id:
		return sum, fmt.Errorf("sidecar names export %q", sc.ExportID)
	case sc.Size != size:
		return sum, fmt.Errorf("the package holds %d bytes; its sidecar says %d", size, sc.Size)
	case err != nil:
		return sum, err
	case checkLabel(sc.Owner) != nil:
		return sum, errors.New("the sidecar names no valid owner")
	case sc.DoneAt.IsZero() || sc.ExpiresAt.IsZero():
		return sum, errors.New("the sidecar has no done or expiry time")
	}
	return sum, nil
}

func decodeSum(s string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(sum) {
		return sum, fmt.Errorf("sha256 %q is not %d bytes of hex", s, len(sum))
	}
	copy(sum[:], b)
	return sum, nil
}

// ours reports whether the sidecar names this database. The manifest has no
// ID; the root, compared as a file so another spelling matches, tells an
// imported copy, which keeps its source's creation time, from its source.
func (ex *exports) ours(sc exportSidecar) bool {
	if !sc.DatabaseCreated.Equal(ex.made) {
		return false
	}
	theirs, err := os.Stat(sc.DatabaseRoot)
	if err != nil {
		return false
	}
	mine, err := os.Stat(ex.root)
	return err == nil && os.SameFile(theirs, mine)
}

// adopt serves a package an earlier run wrote, to the same token label.
func (ex *exports) adopt(sc exportSidecar, sum [sha256.Size]byte) {
	j := &exportJob{
		id: sc.ExportID, owner: sc.Owner, withoutDerived: sc.WithoutDerived, created: sc.CreatedAt,
		state: exportStateDone, finished: sc.DoneAt, expires: sc.ExpiresAt, size: sc.Size, sum: sum,
		content: sc.ContentSHA256, signedBy: sc.SignedBy, kid: sc.KeyID, plaintext: sc.PlaintextComponents,
	}
	j.written.Store(sc.Size)
	ex.jobs[j.id] = j
}

// removeFile removes one file of an export, and logs a failure.
func (ex *exports) removeFile(id, path string) bool {
	if err := fsx.Remove(path); err != nil {
		ex.s.log.Warn("export file not removed", zap.String(logFieldExportID, id), zap.String("file", filepath.Base(path)), zap.Error(err))
		return false
	}
	return true
}

// removeFiles removes a written package, then its sidecar, so a crash
// between them leaves a sidecar the next start removes, never a package
// without one. Once the package is gone the export is gone: a sidecar that
// stays is logged and left to that sweep, as it is never served alone.
// Under mu.
func (ex *exports) removeFiles(j *exportJob, reason string) error {
	pkg := ex.file(j.id, dbpackage.Ext)
	if !ex.removeFile(j.id, pkg) {
		return fmt.Errorf("export %s: the package was not removed", j.id)
	}
	ex.removeFile(j.id, ex.file(j.id, sidecarSuffix))
	ex.s.log.Info("export removed", zap.String(logFieldExportID, j.id), zap.String("reason", reason))
	return nil
}

// kept is how many written packages are kept. Under mu.
func (ex *exports) kept() int {
	n := 0
	for _, j := range ex.jobs {
		if j.state == exportStateDone {
			n++
		}
	}
	return n
}

// outstanding is what a running export into the data root has still to
// write, which uploads must leave free: the root's volume holds both. A nil
// exports has none.
func (ex *exports) outstanding() int64 {
	if ex == nil || !ex.inRoot {
		return 0
	}
	j := ex.running.Load()
	if j == nil {
		return 0
	}
	return max(j.expected.Load()-j.written.Load(), 0)
}

func (ex *exports) get(id string) *exportJob {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.jobs[id]
}

func (ex *exports) setState(j *exportJob, state string) {
	ex.mu.Lock()
	j.state = state
	ex.mu.Unlock()
}

// start begins an export for the token of the request ctx belongs to. It
// sizes the database first, and refuses one its folder has no room for.
func (ex *exports) start(ctx context.Context, withoutDerived bool) (*exportJob, *uploadError) {
	if ex.s.draining.Load() {
		return nil, uploadErr(http.StatusServiceUnavailable, msgShuttingDown)
	}
	ex.mu.Lock()
	if ex.busy {
		ex.mu.Unlock()
		return nil, uploadErr(http.StatusConflict, msgExportRunning)
	}
	if n := ex.kept(); n >= ex.lim.MaxKept {
		ex.mu.Unlock()
		return nil, uploadErr(http.StatusConflict, msgExportsKept, n)
	}
	ex.busy = true
	ex.mu.Unlock()

	id := uuid.NewString()
	need, err := ex.size(ctx, id, withoutDerived)
	if err == nil {
		err = ex.checkSpace(ex.dir, need)
	}
	if err != nil {
		ex.mu.Lock()
		ex.busy = false
		ex.mu.Unlock()
		if low, ok := errors.AsType[*diskspace.LowSpaceError](err); ok {
			return nil, uploadErr(http.StatusInsufficientStorage, msgExportNoRoom, low.Free, low.Need+diskspace.Reserve)
		}
		// The client went away, or the server is stopping: no fault of the
		// sizing, and nothing to log.
		if ctx.Err() != nil {
			if ex.s.draining.Load() {
				return nil, uploadErr(http.StatusServiceUnavailable, msgShuttingDown)
			}
			return nil, uploadErr(http.StatusServiceUnavailable, msgExportAbandoned)
		}
		ex.s.log.Error("export sizing failed", zap.String(logFieldExportID, id), zap.String("request_id", RequestID(ctx)), zap.Error(err))
		return nil, uploadErr(http.StatusInternalServerError, msgExportSizing)
	}

	jctx, cancel := context.WithCancelCause(ex.s.base)
	j := &exportJob{
		id: id, owner: credentialOf(ctx).label, withoutDerived: withoutDerived, created: ex.now(),
		ctx: jctx, cancel: cancel, state: exportStatePausing,
	}
	j.expected.Store(int64(need))
	ex.mu.Lock()
	ex.jobs[id] = j
	ex.mu.Unlock()
	ex.running.Store(j)
	ex.s.log.Info("export started", zap.String(logFieldExportID, id), zap.String("request_id", RequestID(ctx)), bearerField(j.owner),
		zap.Bool("without_derived", withoutDerived), zap.Uint64("bytes_expected", need))
	ex.s.goWork(func(context.Context) { ex.run(j) })
	return j, nil
}

// size estimates the room a package needs within SizingBudget; past it, it
// gives 0, and Export's own check after the suspend is the guard.
func (ex *exports) size(ctx context.Context, id string, withoutDerived bool) (uint64, error) {
	began := time.Now()
	sctx, cancel := context.WithTimeout(ctx, ex.lim.SizingBudget)
	defer cancel()
	need, err := ex.estimate(sctx, ex.s.root, withoutDerived)
	if err != nil && ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
		ex.s.log.Warn("export sizing skipped", zap.String(logFieldExportID, id), zap.Int64("elapsed_ms", time.Since(began).Milliseconds()))
		return 0, nil
	}
	return need, err
}

// run pauses the database, writes the package, and resumes it. busy stays
// set until the resume is over: a second pause could not begin before.
func (ex *exports) run(j *exportJob) {
	defer j.cancel(nil)
	j.began = time.Now()
	err := ex.s.whilePaused(j.ctx, pauseOptions{For: exportPauseFor + j.id, Drain: ex.cfg.Drain, Quiesced: ex.countStats(j)},
		func(held *dbopen.Suspended) error { return ex.write(j, held) })
	ex.finish(j, err)
}

// countStats counts the evidence on the idle database before it is
// suspended: a suspended one cannot be read.
func (ex *exports) countStats(j *exportJob) func(ctx context.Context, db *badger.DB) error {
	return func(ctx context.Context, db *badger.DB) error {
		stats, err := dbpackage.EvidenceStats(ctx, db)
		if err != nil {
			return fmt.Errorf("counting the evidence: %w", err)
		}
		j.stats = stats
		return nil
	}
}

// write packages the suspended database as <id>.duesarc.partial, which
// Export writes whole and synced, then publishes it.
func (ex *exports) write(j *exportJob, held *dbopen.Suspended) error {
	ex.setState(j, exportStatePackaging)
	// Someone may have removed the folder, which Export needs inside the root.
	if err := os.MkdirAll(ex.dir, cnst.DirPerm); err != nil {
		return fmt.Errorf("export folder: %w", err)
	}
	partial := ex.file(j.id, dbpackage.Ext+partialSuffix)
	sum := sha256.New()
	res, err := ex.export(j.ctx, dbpackage.ExportOptions{
		Held: held, Stats: j.stats, Output: partial, WithoutDerived: j.withoutDerived, Creator: ex.cfg.Creator,
		Tee: io.MultiWriter(sum, byteCounter{&j.written}),
	})
	if err != nil {
		ex.removeFile(j.id, partial)
		return err
	}
	// The ETag and the digest come from the Tee: they must describe the
	// file served.
	if n := j.written.Load(); n != res.Bytes {
		ex.removeFile(j.id, partial)
		return fmt.Errorf("the package holds %d bytes, but %d were written", res.Bytes, n)
	}
	// The package may hold evidence in the clear, and the folder may be one
	// other accounts can read.
	if err := os.Chmod(partial, cnst.BlobPerm); err != nil {
		ex.removeFile(j.id, partial)
		return fmt.Errorf("export package: %w", err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], sum.Sum(nil))
	return ex.publish(j, res, digest, partial)
}

// publish writes the sidecar, then gives the package its name. Under mu, as
// cancel is, so a cancel lands wholly before it, leaving no file, or after
// it, removing both.
func (ex *exports) publish(j *exportJob, res dbpackage.ExportResult, sum [sha256.Size]byte, partial string) error {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if j.ctx.Err() != nil {
		ex.removeFile(j.id, partial)
		return context.Cause(j.ctx)
	}
	done := ex.now()
	m := res.Manifest
	sc := exportSidecar{
		Version: sidecarVersion, ExportID: j.id, Owner: j.owner, DatabaseRoot: ex.root, DatabaseCreated: ex.made,
		CreatedAt: j.created.UTC(), DoneAt: done.UTC(), ExpiresAt: done.Add(ex.cfg.Keep).UTC(),
		WithoutDerived: j.withoutDerived, Size: res.Bytes, SHA256: hex.EncodeToString(sum[:]), ContentSHA256: m.ContentSHA256,
		PlaintextComponents: m.Database.PlaintextComponents,
	}
	if sig := m.Signature; sig != nil {
		sc.SignedBy, sc.KeyID = sig.Actor, sig.KeyID
	}
	data, err := json.MarshalIndent(sc, "", sidecarIndent)
	if err != nil {
		ex.removeFile(j.id, partial)
		return fmt.Errorf("export sidecar: %w", err)
	}
	side := ex.file(j.id, sidecarSuffix)
	if err := fsx.WriteFileAtomic(side, data, cnst.BlobPerm); err != nil {
		ex.removeFile(j.id, partial)
		return err
	}
	if err := ex.rename(partial, ex.file(j.id, dbpackage.Ext), false); err != nil {
		ex.removeFile(j.id, partial)
		ex.removeFile(j.id, side)
		return err
	}
	j.state, j.finished, j.expires = exportStateDone, done, sc.ExpiresAt
	ex.running.CompareAndSwap(j, nil)
	j.size, j.sum, j.content = res.Bytes, sum, m.ContentSHA256
	j.signedBy, j.kid, j.plaintext = sc.SignedBy, sc.KeyID, sc.PlaintextComponents
	ex.s.log.Info("export written", zap.String(logFieldExportID, j.id), bearerField(j.owner), zap.Int64("bytes", res.Bytes),
		zap.String("sha256", sc.SHA256), zap.String("content_sha256", m.ContentSHA256), zap.Int("files", len(m.Files)),
		zap.String("signed_by", sc.SignedBy), zap.String("key_id", sc.KeyID), zap.Int64("duration_ms", time.Since(j.began).Milliseconds()),
		zap.Time("expires_at", sc.ExpiresAt), zap.Strings("notices", res.Notices), zap.Strings("warnings", res.Warnings))
	return nil
}

// finish records how a job ended, once whilePaused has returned. A job
// already written stays done: an error then is the resume's, which the
// pause logged.
func (ex *exports) finish(j *exportJob, err error) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	ex.busy = false
	ex.running.CompareAndSwap(j, nil)
	stage := j.state
	if stage != exportStatePausing && stage != exportStatePackaging {
		return
	}
	state, msg, retryable := ex.failure(j, err)
	j.state, j.errMsg, j.retryable, j.finished = state, msg, retryable, ex.now()
	log := ex.s.log
	if state == exportStateCancelled {
		log.Info("export cancelled", zap.String(logFieldExportID, j.id), bearerField(j.owner), zap.String("stage", stage))
		return
	}
	fields := []zap.Field{zap.String(logFieldExportID, j.id), bearerField(j.owner), zap.Error(err), zap.Bool("retryable", retryable),
		zap.Int64("duration_ms", time.Since(j.began).Milliseconds())}
	if retryable || msg == msgExportNoSpace {
		log.Warn("export failed", fields...)
		return
	}
	log.Error("export failed", fields...)
}

// failure maps how a job ended to its state and the message its status
// gives. Cancel comes first: Export returns a plain context.Canceled.
func (ex *exports) failure(j *exportJob, err error) (state, msg string, retryable bool) {
	cause := context.Cause(j.ctx)
	switch {
	case errors.Is(cause, errExportCancelled):
		return exportStateCancelled, "", false
	case errors.Is(err, errPausing):
		return exportStateFailed, msgExportPausing, true
	case errors.Is(err, errPauseStuck):
		return exportStateFailed, msgExportBusy, true
	case errors.Is(err, errStopping), errors.Is(cause, errStopping):
		return exportStateFailed, msgExportStopped, true
	case errors.Is(err, dbopen.ErrNeedsRecovery):
		return exportStateFailed, msgExportUnclean, true
	case diskspace.IsNoSpace(err):
		return exportStateFailed, msgExportNoSpace, false
	}
	return exportStateFailed, fmt.Sprintf(msgExportInternal, j.id), false
}

// cancelJob stops a running export, or removes a written one, unless it is
// being downloaded. A failed or cancelled one is left as it is.
func (ex *exports) cancelJob(j *exportJob) (exportStatus, int, *uploadError) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	switch j.state {
	case exportStatePausing, exportStatePackaging:
		j.cancel(errExportCancelled)
		return ex.statusLocked(j), http.StatusAccepted, nil
	case exportStateDone:
		if j.downloads > 0 {
			return exportStatus{}, 0, uploadErr(http.StatusConflict, msgExportDownloading)
		}
		if err := ex.removeFiles(j, removedCancelled); err != nil {
			return exportStatus{}, 0, uploadErr(http.StatusInternalServerError, msgExportNotRemoved, j.id)
		}
		j.state, j.finished = exportStateCancelled, ex.now()
	}
	return ex.statusLocked(j), http.StatusOK, nil
}

// beginDownload admits a download of a written package; a GET takes one of
// MaxDownloads slots, a HEAD none. end must be called once it is over;
// meanwhile the package is never removed.
func (ex *exports) beginDownload(j *exportJob, counted bool) (end func(), uerr *uploadError) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.jobs[j.id] != j {
		return nil, uploadErr(http.StatusNotFound, msgExportNotFound)
	}
	if j.state != exportStateDone {
		return nil, uploadErr(http.StatusConflict, msgExportNotDone, j.state)
	}
	if counted {
		select {
		case ex.slots <- struct{}{}:
		default:
			return nil, uploadErr(http.StatusTooManyRequests, msgDownloadsBusy, cap(ex.slots))
		}
	}
	j.downloads++
	return func() {
		ex.mu.Lock()
		j.downloads--
		ex.mu.Unlock()
		if counted {
			<-ex.slots
		}
	}, nil
}

func (ex *exports) status(j *exportJob) exportStatus {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.statusLocked(j)
}

// statusLocked is status under mu.
func (ex *exports) statusLocked(j *exportJob) exportStatus {
	st := exportStatus{
		ExportID: j.id, State: j.state, WithoutDerived: j.withoutDerived, CreatedAt: j.created.UTC(),
		BytesWritten: j.written.Load(), BytesExpected: j.expected.Load(),
	}
	switch j.state {
	case exportStateDone:
		st.Size, st.SHA256, st.ContentSHA256 = j.size, hex.EncodeToString(j.sum[:]), j.content
		st.SignedBy, st.KeyID, st.PlaintextComponents = j.signedBy, j.kid, j.plaintext
		st.Download = downloadPath + j.id
		st.ExpiresAt = j.expires.UTC()
	case exportStateFailed, exportStateCancelled:
		st.Error, st.Retryable = j.errMsg, j.retryable
		st.ExpiresAt = j.finished.Add(ex.lim.DoneTTL).UTC()
	}
	return st
}

// startReaper removes expired packages, and forgets ended exports, until
// ctx ends.
func (ex *exports) startReaper(ctx context.Context) {
	ex.reaperDone = make(chan struct{})
	go func() {
		defer close(ex.reaperDone)
		t := time.NewTicker(ex.lim.ReapInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ex.reap()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// reap removes packages past their expiry, unless one is being downloaded
// (Windows cannot remove an open file), and forgets failed and cancelled
// exports DoneTTL after they ended. A package not removed is tried again at
// the next round.
func (ex *exports) reap() {
	now := ex.now()
	ex.mu.Lock()
	defer ex.mu.Unlock()
	for id, j := range ex.jobs {
		switch j.state {
		case exportStateDone:
			if j.downloads == 0 && !now.Before(j.expires) && ex.removeFiles(j, removedExpired) == nil {
				delete(ex.jobs, id)
			}
		case exportStateFailed, exportStateCancelled:
			if now.Sub(j.finished) > ex.lim.DoneTTL {
				delete(ex.jobs, id)
			}
		}
	}
}

// stop waits for the reaper, and gives the folder up after a clean stop. An
// unclean one keeps it locked: an export may still be writing there.
func (ex *exports) stop(idle bool) {
	if ex.reaperDone != nil {
		<-ex.reaperDone
	}
	if !idle {
		return
	}
	if err := ex.lock.Unlock(); err != nil {
		ex.s.log.Warn("export folder not unlocked", zap.Error(err))
	}
}

// HTTP endpoints.

func (s *Server) exportRoutes() {
	lim := s.exports.lim
	js := route{maxBody: s.cfg.Limits.MaxJSONBody, read: jsonRouteTimeout, write: jsonRouteTimeout, scope: ScopeExport}
	// Start answers once the root is sized, within SizingBudget.
	start := js
	start.write = lim.SizingBudget + jsonRouteTimeout
	s.handle("POST /web/export/start", start, http.HandlerFunc(s.handleExportStart))
	s.handle("POST /web/export/status", js, http.HandlerFunc(s.handleExportStatus))
	s.handle("POST /web/export/cancel", js, http.HandlerFunc(s.handleExportCancel))
	// No read deadline: over HTTP/1.1 one would cancel a long download
	// through the server's background read. The GET pattern serves HEAD too.
	s.handle(http.MethodGet+" "+downloadPath+"{"+pathValueID+"}", route{scope: ScopeExport, idleWrite: lim.DownloadIdle},
		http.HandlerFunc(s.handleExportDownload))
}

func (s *Server) stopExports(idle bool) {
	if s.exports != nil {
		s.exports.stop(idle)
	}
}

// exportOf finds the export a request names, or answers 400 or 404. An
// export another token started is answered as unknown, so ids are no oracle.
func (s *Server) exportOf(w http.ResponseWriter, r *http.Request, id string) *exportJob {
	if id == "" {
		httpError(w, r, http.StatusBadRequest, msgExportIDRequired)
		return nil
	}
	var j *exportJob
	if validExportID(id) {
		j = s.exports.get(id)
	}
	// Labels are unique without regard to case, and a restart may respell
	// the label of an adopted export's owner.
	if who := credentialOf(r.Context()).label; j != nil && !strings.EqualFold(j.owner, who) {
		s.log.Warn("export of another token", zap.String("request_id", RequestID(r.Context())), zap.String(logFieldExportID, j.id),
			bearerField(who))
		j = nil
	}
	if j == nil {
		httpError(w, r, http.StatusNotFound, msgExportNotFound)
	}
	return j
}

// handleExportStart starts an export of the database.
//
//	POST /web/export/start {"without_derived": false} → 202 exportStatus
func (s *Server) handleExportStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WithoutDerived bool `json:"without_derived"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	j, uerr := s.exports.start(r.Context(), req.WithoutDerived)
	if uerr != nil {
		writeUploadError(w, r, uerr)
		return
	}
	writeJSON(w, http.StatusAccepted, s.exports.status(j))
}

// handleExportStatus says where an export is.
//
//	POST /web/export/status {"export_id": "..."} → exportStatus
func (s *Server) handleExportStatus(w http.ResponseWriter, r *http.Request) {
	var req exportIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if j := s.exportOf(w, r, req.ExportID); j != nil {
		writeJSON(w, http.StatusOK, s.exports.status(j))
	}
}

// handleExportCancel stops a running export (202, as it ends later), or
// removes a written one (200).
//
//	POST /web/export/cancel {"export_id": "..."} → exportStatus
func (s *Server) handleExportCancel(w http.ResponseWriter, r *http.Request) {
	var req exportIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	j := s.exportOf(w, r, req.ExportID)
	if j == nil {
		return
	}
	st, code, uerr := s.exports.cancelJob(j)
	if uerr != nil {
		writeUploadError(w, r, uerr)
		return
	}
	writeJSON(w, code, st)
}

// handleExportDownload serves a written package. ServeContent answers
// ranges, conditional requests and HEAD; the ETag and Repr-Digest give the
// file's SHA-256, so a client checks what it saved without reading it twice.
//
//	GET|HEAD /web/export/download/{id}
func (s *Server) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue(pathValueID)
	if !validExportID(id) {
		httpError(w, r, http.StatusNotFound, msgExportNotFound)
		return
	}
	j := s.exportOf(w, r, id)
	if j == nil {
		return
	}
	ex := s.exports
	end, uerr := ex.beginDownload(j, r.Method == http.MethodGet)
	if uerr != nil {
		writeUploadError(w, r, uerr)
		return
	}
	defer end()
	f, err := ex.open(ex.file(j.id, dbpackage.Ext))
	if err != nil {
		s.log.Error("export not opened", zap.String(logFieldExportID, j.id), zap.String("request_id", RequestID(r.Context())), zap.Error(err))
		httpError(w, r, http.StatusInternalServerError, fmt.Sprintf(msgExportUnreadable, j.id))
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			s.log.Warn("export not closed", zap.String(logFieldExportID, j.id), zap.Error(err))
		}
	}()
	ex.mu.Lock()
	sum, written := j.sum, j.finished
	ex.mu.Unlock()
	h := w.Header()
	h.Set(headerETag, fmt.Sprintf(etagFormat, sum[:]))
	h.Set(headerReprDigest, fmt.Sprintf(reprDigestFormat, base64.StdEncoding.EncodeToString(sum[:])))
	h.Set(headerCacheControl, cacheNoStore)
	h.Set(headerContentType, contentTypeZip)
	h.Set(headerContentTypeOptions, noSniff)
	h.Set(headerContentDisposition, mime.FormatMediaType(dispositionAttachment, map[string]string{dispositionFilename: j.id + dbpackage.Ext}))
	http.ServeContent(w, r, "", written, f)
	if r.Method == http.MethodGet {
		s.log.Info("export downloaded", zap.String(logFieldExportID, j.id), zap.String("request_id", RequestID(r.Context())),
			bearerField(credentialOf(r.Context()).label), zap.Bool("range", r.Header.Get(headerRange) != ""))
	}
}
