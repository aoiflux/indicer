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
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// DefaultMaxBytes is the size at which a log file is rotated.
	DefaultMaxBytes = 50 << 20
	// DefaultKeep is how many log files are kept per directory, the current
	// one included.
	DefaultKeep = 10
	// EnvDir names the environment variable that sets the log directory.
	EnvDir = "DUES_LOG_DIR"
)

// Config controls Init.
type Config struct {
	Component string // file prefix and "component" field; "dues" when empty
	Dir       string // explicit directory (--log-dir); empty to resolve one
	Debug     bool
	MaxBytes  int64     // rotation size; DefaultMaxBytes when zero
	Keep      int       // files kept; DefaultKeep when zero
	Stderr    io.Writer // where warnings about logging itself go; os.Stderr when nil
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

// dirCandidates lists where a log may go, in order. Tests replace it.
var dirCandidates = func(explicit string) []string {
	var dirs []string
	if explicit != "" {
		dirs = append(dirs, explicit)
	}
	if env := os.Getenv(EnvDir); env != "" {
		dirs = append(dirs, env)
	}
	if cache, err := os.UserCacheDir(); err == nil {
		dirs = append(dirs, filepath.Join(cache, "dues", "logs"))
	}
	user := "user"
	if u := os.Getenv("USER"); u != "" {
		user = u
	} else if u := os.Getenv("USERNAME"); u != "" {
		user = u
	}
	return append(dirs, filepath.Join(os.TempDir(), "dues-"+user, "logs"))
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
	for _, dir := range dirCandidates(cfg.Dir) {
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
		st.out = out
		st.info.Dir = out.dir
		st.info.Path = out.Path()
		st.logger = newLogger(out, cfg.Debug).With(
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

func newLogger(w zapcore.WriteSyncer, debug bool) *zap.Logger {
	level := zapcore.InfoLevel
	if debug {
		level = zapcore.DebugLevel
	}
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
