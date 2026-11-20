// Package crash makes every crash leave a report.
//
// Some failures cannot be recovered: a concurrent map write, a stack overflow,
// or a panic on a goroutine nothing guards. The runtime prints those to stderr
// and exits, and the trace is lost once the terminal closes. Setup sends the
// runtime's crash output to a file in the log directory, and Recover turns a
// panic on the main goroutine into a short message pointing at that file.
package crash

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"
)

var (
	mu   sync.Mutex
	file *os.File
	path string
)

// Setup creates <dir>/crash-<runID>.txt and registers it as the runtime's
// crash output. It returns the file's path. Call Finish on a clean exit, which
// removes the file when nothing was written to it.
func Setup(dir, runID string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "crash-"+runID+".txt")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		f.Close()
		os.Remove(p)
		return "", err
	}
	file, path = f, p
	return p, nil
}

// Path returns the crash file set up by Setup, or "".
func Path() string {
	mu.Lock()
	defer mu.Unlock()
	return path
}

// Finish detaches the crash file and deletes it if it is empty.
func Finish() {
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return
	}
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	info, err := file.Stat()
	file.Close()
	if err == nil && info.Size() == 0 {
		os.Remove(path)
	}
	file, path = nil, ""
}

// Write appends a report of a recovered panic to the crash file and returns
// the file's path, or "" when Setup was not called or the write failed.
func Write(v any, stack []byte) string {
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return ""
	}
	if _, err := fmt.Fprintf(file, "%s panic: %v\n\n%s\n", time.Now().UTC().Format(time.RFC3339), v, stack); err != nil {
		return ""
	}
	_ = file.Sync()
	return path
}

// Message is what the user sees after a crash. It names the run id so the
// matching log records can be found.
func Message(w io.Writer, v any, runID, crashPath string) {
	fmt.Fprintf(w, "\nDUES crashed: %v\n", v)
	if crashPath != "" {
		fmt.Fprintf(w, "Details: %s (run %s)\n", crashPath, runID)
	} else {
		fmt.Fprintf(w, "Run id: %s\n", runID)
	}
	fmt.Fprintln(w, "Please report this, attaching the details file.")
}
