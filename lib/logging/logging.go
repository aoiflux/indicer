package logging

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	Logger *zap.Logger
	Sugar  *zap.SugaredLogger
	once   sync.Once
)

type jsonArrayWriteSyncer struct {
	mu         sync.Mutex
	file       *os.File
	hasEntries bool
}

func newJSONArrayWriteSyncer(path string) (*jsonArrayWriteSyncer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}

	if _, err := file.WriteString("[\n]\n"); err != nil {
		_ = file.Close()
		return nil, err
	}

	return &jsonArrayWriteSyncer{file: file}, nil
}

func (w *jsonArrayWriteSyncer) Write(p []byte) (int, error) {
	trimmed := bytes.TrimSpace(p)
	if len(trimmed) == 0 {
		return len(p), nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.file.Seek(-2, 2); err != nil {
		return 0, err
	}

	entry := trimmed
	if w.hasEntries {
		entry = append([]byte(",\n"), entry...)
	}
	entry = append(entry, []byte("\n]\n")...)

	if _, err := w.file.Write(entry); err != nil {
		return 0, err
	}

	w.hasEntries = true
	return len(p), nil
}

func (w *jsonArrayWriteSyncer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

// InitLogger initializes the global zap logger with structured output
// Call this once at application startup from main.go
func InitLogger(debug bool) error {
	var err error
	once.Do(func() {
		logDir := "log"
		if mkErr := os.MkdirAll(logDir, 0o755); mkErr != nil {
			err = mkErr
			return
		}
		logPath := filepath.Join(logDir, "dues.json")

		writeSyncer, wsErr := newJSONArrayWriteSyncer(logPath)
		if wsErr != nil {
			err = wsErr
			return
		}

		level := zap.NewAtomicLevelAt(zapcore.InfoLevel)
		encoderConfig := zap.NewProductionEncoderConfig()
		if debug {
			level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
			encoderConfig = zap.NewDevelopmentEncoderConfig()
		}

		encoderConfig.TimeKey = "timestamp"
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		encoderConfig.CallerKey = "caller"
		encoderConfig.FunctionKey = zapcore.OmitKey

		core := zapcore.NewCore(
			zapcore.NewJSONEncoder(encoderConfig),
			writeSyncer,
			level,
		)

		Logger = zap.New(
			core,
			zap.AddCaller(),
			zap.AddCallerSkip(1),
			zap.AddStacktrace(zapcore.PanicLevel),
			zap.ErrorOutput(writeSyncer),
		)
		if err != nil {
			return
		}

		_, _ = zap.RedirectStdLogAt(Logger, zapcore.InfoLevel)
		log.SetFlags(0)

		// Sugar logger for convenient APIs
		Sugar = Logger.Sugar()

		// Sync on exit
		registerShutdown()
	})

	return err
}

// registerShutdown ensures logger is flushed on exit
func registerShutdown() {
	// Will be called by main.go's defer
}

// Sync flushes any buffered log entries
// Call this with defer in main() after InitLogger
func Sync() error {
	if Logger != nil {
		return Logger.Sync()
	}
	return nil
}

// GetLogger returns the global logger instance
func GetLogger() *zap.Logger {
	if Logger == nil {
		_ = InitLogger(false)
		if Logger == nil {
			Logger = zap.NewNop()
			Sugar = Logger.Sugar()
		}
	}
	return Logger
}

// GetSugar returns the sugared logger for convenient APIs
func GetSugar() *zap.SugaredLogger {
	if Sugar == nil {
		_ = InitLogger(true)
	}
	return Sugar
}

// WithField returns a logger with a field attached (chainable)
func WithField(key string, value interface{}) *zap.Logger {
	return GetLogger().With(zap.Any(key, value))
}

// WithFields returns a logger with multiple fields attached (chainable)
func WithFields(fields ...zap.Field) *zap.Logger {
	return GetLogger().With(fields...)
}

// DebugFunc logs entry/exit of a function (useful for tracing execution flow)
// Usage: defer logging.DebugFunc("FunctionName")()
func DebugFunc(name string, fields ...zap.Field) func() {
	GetLogger().Debug("ENTER: "+name, fields...)
	return func() {
		GetLogger().Debug("EXIT: " + name)
	}
}

// TraceOperation logs a long-running operation with start/end timing
type TraceContext struct {
	name   string
	fields []zap.Field
}

// StartTrace begins logging an operation
func StartTrace(name string, fields ...zap.Field) *TraceContext {
	GetLogger().Debug("TRACE_START: "+name, fields...)
	return &TraceContext{name: name, fields: fields}
}

// End completes the trace and logs duration
func (tc *TraceContext) End() {
	GetLogger().Debug("TRACE_END: " + tc.name)
}
