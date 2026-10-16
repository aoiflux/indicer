package logging

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const logExt = ".ndjson"

// openFile creates a new log file. Tests replace it to inject write failures.
var openFile = func(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
}

// rotatingWriter appends to one file per process and starts a new one when the
// current file reaches maxBytes. The first write error disables it for the
// rest of the process, with a single warning, so a full disk cannot turn every
// log call into an error or a stderr flood.
type rotatingWriter struct {
	mu       sync.Mutex
	dir      string
	base     string // <component>-<timestamp>-<pid>
	maxBytes int64
	keep     int
	stderr   io.Writer
	f        io.WriteCloser
	path     string
	size     int64
	seq      int
	failed   bool
}

// openRotating creates dir if needed, prunes old logs and opens a fresh file.
func openRotating(dir, component string, maxBytes int64, keep int, stderr io.Writer) (*rotatingWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w := &rotatingWriter{
		dir:      dir,
		base:     fmt.Sprintf("%s-%s-%d", component, time.Now().UTC().Format("20060102T150405Z"), os.Getpid()),
		maxBytes: maxBytes,
		keep:     keep,
		stderr:   stderr,
	}
	if err := w.openNext(); err != nil {
		return nil, err
	}
	return w, nil
}

// openNext opens the next file in the sequence, skipping names that already
// exist (another Init in the same second, or a reused pid).
func (w *rotatingWriter) openNext() error {
	for tries := 0; ; tries++ {
		name := w.base + logExt
		if w.seq > 0 {
			name = fmt.Sprintf("%s.%d%s", w.base, w.seq, logExt)
		}
		w.seq++
		path := filepath.Join(w.dir, name)
		f, err := openFile(path)
		if errors.Is(err, fs.ErrExist) && tries < 100 {
			continue
		}
		if err != nil {
			return err
		}
		w.f, w.path, w.size = f, path, 0
		pruneLogs(w.dir, w.keep, path)
		return nil
	}
}

// Path returns the file currently written.
func (w *rotatingWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed {
		return len(p), nil
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		old := w.f
		if err := w.openNext(); err != nil {
			w.fail(err)
			return len(p), nil
		}
		_ = old.Close()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		w.fail(err)
	}
	return len(p), nil
}

func (w *rotatingWriter) fail(err error) {
	w.failed = true
	fmt.Fprintf(w.stderr, "dues: warning: logging stopped, cannot write %s: %v\n", w.path, err)
}

func (w *rotatingWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s, ok := w.f.(interface{ Sync() error }); ok && !w.failed {
		return s.Sync()
	}
	return nil
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// pruneLogs deletes the oldest DUES log files in dir until at most keep remain,
// never touching the file being written or anything that is not a log file.
func pruneLogs(dir string, keep int, current string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type logFile struct {
		path string
		mod  time.Time
	}
	var logs []logFile
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), logExt) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, logFile{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(logs) <= keep {
		return
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].mod.Before(logs[j].mod) })
	for _, l := range logs[:len(logs)-keep] {
		if l.path != current {
			_ = os.Remove(l.path)
		}
	}
}
