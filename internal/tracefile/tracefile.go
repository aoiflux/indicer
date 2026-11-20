// Package tracefile writes a Go execution trace of the process to a file,
// for profiling a command with go tool trace.
package tracefile

import (
	"errors"
	"fmt"
	"os"
	"runtime/trace"
	"sync"
)

// ErrTracing is returned by Start while the process is being traced already.
var ErrTracing = errors.New("the process is being traced already")

// Start traces the process into a new file at path until stop is called. A
// file already at path is never replaced. stop ends the trace and closes the
// file; it may be called more than once, and returns the first call's error.
func Start(path string) (stop func() error, err error) {
	if trace.IsEnabled() {
		return nil, ErrTracing
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%s exists; give a new file", path)
		}
		return nil, err
	}
	if err := trace.Start(f); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		if trace.IsEnabled() {
			return nil, ErrTracing
		}
		return nil, err
	}
	var once sync.Once
	var stopErr error
	return func() error {
		once.Do(func() {
			trace.Stop()
			stopErr = errors.Join(f.Sync(), f.Close())
		})
		return stopErr
	}, nil
}
