package main

/*
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"context"
	"io"
	"os"
	"time"
	"unsafe"

	"go.uber.org/zap/zapcore"

	"indicer/internal/console"
	"indicer/internal/core/jsonbridge"
	"indicer/lib/logging"
	platformapi "indicer/pkg/api"
)

// The library runs inside a host process whose stdout and stderr are not
// DUES's to write to (FFI-3).
func init() {
	console.Quiet()
	logging.Init(logConfig(os.Getenv(logging.EnvDir)))
	logging.ReportPanics()
}

// logConfig chooses the library's log. With DUES_LOG_DIR set it logs there,
// as the command-line tools do. Otherwise it logs only warnings and errors
// (recovered panics among them), to the user cache directory, in a file
// created at the first such entry.
func logConfig(envDir string) logging.Config {
	cfg := logging.Config{Component: "dues-ffi", Stderr: io.Discard}
	if envDir != "" {
		cfg.Dir = envDir
		return cfg
	}
	cfg.MinLevel = zapcore.WarnLevel
	cfg.Lazy = true
	return cfg
}

const (
	emptyRequestJSON = "{}"
)

// DuesDispatchJSON runs one request envelope and returns the response
// envelope, which the caller frees with DuesFreeString. It never returns
// NULL and never lets a panic reach the host.
//
//export DuesDispatchJSON
func DuesDispatchJSON(requestJSON *C.char) (out *C.char) {
	defer func() {
		if recover() != nil {
			out = C.CString(jsonbridge.FallbackError)
		}
	}()
	request, ok := requestString(requestJSON)
	if !ok {
		return C.CString(jsonbridge.EncodeError(jsonbridge.TooLarge()))
	}
	return C.CString(dispatch(platformapi.DefaultDispatcher(), request))
}

// DuesShutdown cancels every async task, refuses every later request, and
// waits up to timeoutMS milliseconds (not at all when it is 0 or less) for
// the tasks to end. It returns a response envelope whose result counts the
// cancelled tasks and those still running; the caller frees it with
// DuesFreeString.
//
//export DuesShutdown
func DuesShutdown(timeoutMS C.longlong) (out *C.char) {
	defer func() {
		if recover() != nil {
			out = C.CString(jsonbridge.FallbackError)
		}
	}()
	return C.CString(shutdown(platformapi.DefaultDispatcher(), int64(timeoutMS)))
}

//export DuesFreeString
func DuesFreeString(ptr *C.char) {
	if ptr == nil {
		return
	}
	C.free(unsafe.Pointer(ptr))
}

// requestString copies a request, reading at most one byte past
// MaxRequestBytes, so an oversized or unterminated buffer is not copied.
func requestString(p *C.char) (string, bool) {
	if p == nil {
		return emptyRequestJSON, true
	}
	n := int(C.strnlen(p, C.size_t(jsonbridge.MaxRequestBytes+1)))
	if n > jsonbridge.MaxRequestBytes {
		return "", false
	}
	return C.GoStringN(p, C.int(n)), true
}

func dispatch(d *platformapi.Dispatcher, request string) string {
	return d.DispatchJSON(context.Background(), request)
}

func shutdown(d *platformapi.Dispatcher, timeoutMS int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(max(timeoutMS, 0))*time.Millisecond)
	defer cancel()
	res := d.Shutdown(ctx)
	_ = logging.Sync()
	return jsonbridge.EncodeOK(res)
}

func main() {}
