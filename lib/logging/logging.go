// Package logging is DUES's structured log.
//
// Each process writes its own NDJSON file, one JSON object per line, under a
// per-user log directory. Every record carries the run id, pid and component,
// so concurrent processes never share a file, and a crash log is never
// overwritten by the next start. Logging can never stop DUES from running: if
// no directory is writable, or the disk fills, it degrades to stderr warnings.
//
// Until Init is called, GetLogger returns a no-op logger, so library and FFI
// code never creates log files on its own.
package logging

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"indicer/internal/osdirs"
	"indicer/internal/safe"
)

const (
	// DefaultMaxBytes is the size at which a log file is rotated.
	DefaultMaxBytes = 50 << 20
	// DefaultKeep is how many log files are kept per directory, the current
	// one included.
	DefaultKeep = 10
)

// Config controls Init.
type Config struct {
	Component string // file prefix and "component" field; "dues" when empty
	Dir       string // explicit directory (--log-dir); empty to resolve one
	TempDir   string // --temp-dir, for the last-resort log directory; empty for osdirs.Temp
	Debug     bool
	MaxBytes  int64     // rotation size; DefaultMaxBytes when zero
	Keep      int       // files kept; DefaultKeep when zero
	Stderr    io.Writer // where warnings about logging itself go; os.Stderr when nil
	// MinLevel is the least severe level written (info when zero); Debug
	// lowers it to debug.
	MinLevel zapcore.Level
	// Lazy opens the log file at the first entry written instead of at Init,
	// so a library host that logs nothing gets no file.
	Lazy bool
}

// Info describes the log Init set up.
type Info struct {
	RunID string
	Dir   string // empty when logging fell back to stderr only
	Path  string // the current log file; empty when Dir is
}

type state struct {
	logger *zap.Logger
	sugar  *zap.SugaredLogger
	info   Info
	out    *rotatingWriter
}

var (
	current atomic.Pointer[state]
	initMu  sync.Mutex
	nop     = &state{logger: zap.NewNop(), sugar: zap.NewNop().Sugar()}
)

// dirCandidates lists where a log may go, in order: the explicit
// directory, the per-user cache, then a per-user directory under the
// temporary directory. Each is worked out only when the ones before it
// failed: the account name behind the last costs milliseconds on Windows,
// and nearly every start stops at the first. Tests replace it.
var dirCandidates = func(explicit, tempDir string) iter.Seq[string] {
	return func(yield func(string) bool) {
		if explicit != "" && !yield(explicit) {
			return
		}
		if dir, err := osdirs.LogDir(); err == nil && !yield(dir) {
			return
		}
		if temp, err := osdirs.TempOr(tempDir); err == nil {
			yield(filepath.Join(temp, "dues-"+tempUser(), "logs"))
		}
	}
}

// userName is osdirs.UserName; a test counts the calls.
var userName = osdirs.UserName

// tempUser names the per-user log directory under the temporary directory:
// the account name, or "user" when there is none usable as a file name.
func tempUser() string {
	name, err := userName()
	if err != nil || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "user"
	}
	return name
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Init sets up the process log and returns where it went. It never fails:
// when no directory is usable it warns once on cfg.Stderr and logs nothing.
// Calling Init again replaces the previous log.
func Init(cfg Config) Info {
	initMu.Lock()
	defer initMu.Unlock()

	if cfg.Component == "" {
		cfg.Component = "dues"
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = DefaultMaxBytes
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultKeep
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	runID := newRunID()

	var (
		out  *rotatingWriter
		errs []error
	)
	if cfg.Lazy {
		out = lazyRotating(slices.Collect(dirCandidates(cfg.Dir, cfg.TempDir)), cfg.Component, cfg.MaxBytes, cfg.Keep, cfg.Stderr)
	}
	for dir := range dirCandidates(cfg.Dir, cfg.TempDir) {
		if out != nil {
			break
		}
		w, err := openRotating(dir, cfg.Component, cfg.MaxBytes, cfg.Keep, cfg.Stderr)
		if err == nil {
			out = w
			break
		}
		errs = append(errs, err)
	}

	st := &state{info: Info{RunID: runID}}
	if out == nil {
		fmt.Fprintf(cfg.Stderr, "dues: warning: logging disabled, no writable log directory: %v\n", errors.Join(errs...))
		st.logger = zap.NewNop()
	} else {
		level := cfg.MinLevel
		if cfg.Debug {
			level = zapcore.DebugLevel
		}
		st.out = out
		st.info.Dir = out.Dir()
		st.info.Path = out.Path()
		st.logger = newLogger(out, level).With(
			zap.String("run_id", runID),
			zap.Int("pid", os.Getpid()),
			zap.String("component", cfg.Component),
		)
	}
	st.sugar = st.logger.Sugar()

	if prev := current.Swap(st); prev != nil && prev.out != nil {
		_ = prev.logger.Sync()
		_ = prev.out.Close()
	}
	if out != nil {
		_, _ = zap.RedirectStdLogAt(st.logger, zapcore.InfoLevel)
		log.SetFlags(0)
	}
	return st.info
}

func newLogger(w zapcore.WriteSyncer, level zapcore.Level) *zap.Logger {
	enc := zap.NewProductionEncoderConfig()
	enc.TimeKey = "timestamp"
	enc.EncodeTime = zapcore.ISO8601TimeEncoder
	enc.FunctionKey = zapcore.OmitKey
	core := redactingCore{zapcore.NewCore(zapcore.NewJSONEncoder(enc), w, level)}
	return zap.New(core,
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.PanicLevel),
		zap.ErrorOutput(zapcore.AddSync(io.Discard)),
	)
}

// InitLogger is the original entry point, kept for existing callers.
func InitLogger(debug bool) error {
	Init(Config{Debug: debug})
	return nil
}

func load() *state {
	if st := current.Load(); st != nil {
		return st
	}
	return nop
}

// GetLogger returns the process logger, or a no-op logger before Init.
func GetLogger() *zap.Logger { return load().logger }

// GetSugar returns the sugared process logger, or a no-op one before Init.
func GetSugar() *zap.SugaredLogger { return load().sugar }

// Current describes the log set up by the last Init, with the file currently
// written after any rotation; the zero Info before Init.
func Current() Info {
	st := load()
	info := st.info
	if st.out != nil {
		info.Path = st.out.Path()
		info.Dir = st.out.Dir()
	}
	return info
}

// Sync flushes buffered log entries.
func Sync() error { return load().logger.Sync() }

// Close flushes and closes the log file and reverts to the no-op logger.
func Close() error {
	initMu.Lock()
	defer initMu.Unlock()
	st := current.Swap(nil)
	if st == nil || st.out == nil {
		return nil
	}
	_ = st.logger.Sync()
	return st.out.Close()
}

// WithField returns a logger with a field attached.
func WithField(key string, value interface{}) *zap.Logger {
	return GetLogger().With(zap.Any(key, value))
}

// WithFields returns a logger with fields attached.
func WithFields(fields ...zap.Field) *zap.Logger {
	return GetLogger().With(fields...)
}

// Where a setting in a "settings" record came from.
const (
	FromFlag    = "flag"
	FromConfig  = "config"
	FromDefault = "default"
)

// Setting is one entry of a "settings" record: the value a setting has in
// this run and where it came from (FromFlag, FromConfig or FromDefault).
// Each program logs one such record once its configuration is read, so
// its log answers what a run was configured with. The core redacts only
// top-level fields, so the entry's own fields are redacted here.
func Setting(name string, value any, from string, extra ...zap.Field) zap.Field {
	fields := append([]zap.Field{zap.Any("value", value), zap.String("source", from)}, extra...)
	return zap.Dict(name, redactFields(fields)...)
}

// DirSetting is the "settings" entry for the log directory: the one in
// effect and, when the one given could not be written and logging fell
// back, the one given.
func DirSetting(name, given, from string) zap.Field {
	dir := Current().Dir
	if given != "" && !sameDir(dir, given) {
		return Setting(name, dir, from, zap.String("given", given))
	}
	return Setting(name, dir, from)
}

func sameDir(a, b string) bool {
	ai, errA := os.Stat(a)
	bi, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}

// DebugFunc logs entry to and exit from a function.
// Usage: defer logging.DebugFunc("FunctionName")()
func DebugFunc(name string, fields ...zap.Field) func() {
	GetLogger().Debug("ENTER: "+name, fields...)
	return func() {
		GetLogger().Debug("EXIT: " + name)
	}
}

// TraceContext logs the start and end of a long-running operation.
type TraceContext struct {
	name   string
	fields []zap.Field
}

// StartTrace begins logging an operation.
func StartTrace(name string, fields ...zap.Field) *TraceContext {
	GetLogger().Debug("TRACE_START: "+name, fields...)
	return &TraceContext{name: name, fields: fields}
}

// End logs the end of the operation.
func (tc *TraceContext) End() {
	GetLogger().Debug("TRACE_END: " + tc.name)
}

// ReportPanics logs every panic internal/safe recovers: the first at each
// site with its value and stack, later ones at the same site by their ID.
func ReportPanics() {
	safe.SetReporter(func(e *safe.PanicError, first bool) {
		if first {
			GetLogger().Error("recovered panic",
				zap.String("panic_id", e.ID), zap.String("site", e.Site.String()),
				zap.Any("value", e.Value), zap.ByteString("stack", e.Stack))
			return
		}
		GetLogger().Warn("recovered panic (repeat)",
			zap.String("panic_id", e.ID), zap.String("site", e.Site.String()))
	})
}
