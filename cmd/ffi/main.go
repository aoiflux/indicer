package main

/*
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"indicer/internal/console"
	coreerr "indicer/internal/core/errors"
	"indicer/internal/core/jsonbridge"
	"indicer/internal/osdirs"
	"indicer/lib/fio"
	"indicer/lib/logging"
	platformapi "indicer/pkg/api"
)

// The library runs inside a host process whose stdout and stderr are not
// DUES's to write to (FFI-3). The log starts with DuesConfigure, or with the
// first request.
func init() {
	console.Quiet()
	logging.ReportPanics()
}

// libConfig is the document DuesConfigure takes. Both fields are optional.
type libConfig struct {
	LogDir  string `json:"log_dir"`
	TempDir string `json:"temp_dir"`
}

// lib is the library's configuration, fixed by DuesConfigure or else by the
// first request, and the dispatcher built from it. Once a dispatcher exists
// the configuration never changes: logging and the temporary directory are
// not safe to move under running tasks.
var lib struct {
	sync.Mutex
	configured bool
	dispatcher *platformapi.Dispatcher
}

// logConfig chooses the library's log. With a log directory it logs there,
// as the command-line tools do. Otherwise it logs only warnings and errors
// (recovered panics among them), to the user cache directory, in a file
// created at the first such entry.
func logConfig(dir string) logging.Config {
	cfg := logging.Config{Component: "dues-ffi", Stderr: io.Discard}
	if dir != "" {
		cfg.Dir = dir
		return cfg
	}
	cfg.MinLevel = zapcore.WarnLevel
	cfg.Lazy = true
	return cfg
}

// parseConfig reads a DuesConfigure document: one JSON object with only the
// known keys, absolute paths, and a temp_dir that is an existing directory.
func parseConfig(doc string) (libConfig, *coreerr.Error) {
	var c libConfig
	invalid := func(format string, args ...any) (libConfig, *coreerr.Error) {
		return libConfig{}, coreerr.New(coreerr.CodeInvalidRequest, "configuration: "+fmt.Sprintf(format, args...))
	}
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return invalid("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("data after the JSON object")
	}
	for _, p := range []struct{ name, dir string }{{"log_dir", c.LogDir}, {"temp_dir", c.TempDir}} {
		if p.dir != "" && !filepath.IsAbs(p.dir) {
			return invalid("%s %q is not an absolute path", p.name, p.dir)
		}
	}
	if c.TempDir != "" {
		info, err := os.Stat(c.TempDir)
		if err != nil {
			return invalid("temp_dir: %v", err)
		}
		if !info.IsDir() {
			return invalid("temp_dir %s is not a directory", c.TempDir)
		}
	}
	return c, nil
}

// configure applies a DuesConfigure document and returns the response
// envelope: the log and temporary directories in effect, or why the
// configuration was refused. A refused call changes nothing.
func configure(doc string) string {
	c, cerr := parseConfig(doc)
	if cerr != nil {
		return jsonbridge.EncodeError(cerr)
	}
	lib.Lock()
	defer lib.Unlock()
	switch {
	case lib.configured:
		return refuse("already_configured", "the library is already configured")
	case lib.dispatcher != nil:
		return refuse("requests_started", "requests have already run with the default configuration")
	}
	temp := setUp(c, logging.FromConfig)
	lib.configured = true
	return jsonbridge.EncodeOK(map[string]string{"log_dir": logging.Current().Dir, "temp_dir": temp})
}

func refuse(reason, message string) string {
	return jsonbridge.EncodeError(coreerr.NewWithDetails(coreerr.CodeInvalidRequest, message, map[string]string{"reason": reason}))
}

// dispatcher returns the library's dispatcher, setting up the default
// configuration when DuesConfigure did not run first.
func dispatcher() *platformapi.Dispatcher {
	lib.Lock()
	defer lib.Unlock()
	if lib.dispatcher == nil {
		setUp(libConfig{}, logging.FromDefault)
	}
	return lib.dispatcher
}

// setUp starts the log, sets the temporary directory, builds the dispatcher
// and logs the "settings" record. It runs once, with lib locked, and
// returns the temporary directory in effect ("" when there is none).
func setUp(c libConfig, from string) string {
	logging.Init(logConfig(c.LogDir))
	// Restores share one container cache per process, so its spill
	// directory is a process setting.
	fio.SetContainerSpillDir(c.TempDir)
	lib.dispatcher = platformapi.NewDefaultDispatcher(platformapi.Options{TempDir: c.TempDir})

	source := func(v string) string {
		if v != "" {
			return from
		}
		return logging.FromDefault
	}
	temp, err := osdirs.TempOr(c.TempDir)
	tempSetting := logging.Setting("temp_dir", temp, source(c.TempDir))
	if err != nil {
		tempSetting = logging.Setting("temp_dir", "", source(c.TempDir), zap.Error(err))
	}
	logging.GetLogger().Info("settings", logging.DirSetting("log_dir", c.LogDir, source(c.LogDir)), tempSetting)
	return temp
}

const (
	emptyRequestJSON = "{}"
)

// DuesConfigure sets the library's log and temporary directories from a
// JSON object, {"log_dir": "...", "temp_dir": "..."}, both optional and
// absolute. Call it once, before any other function; without it the first
// request sets up the defaults. It returns a response envelope, which the
// caller frees with DuesFreeString: the directories in effect, or an
// invalid_request error (reason already_configured or requests_started when
// it came too late). It never returns NULL and never lets a panic reach the
// host.
//
//export DuesConfigure
func DuesConfigure(configJSON *C.char) (out *C.char) {
	defer func() {
		if recover() != nil {
			out = C.CString(jsonbridge.FallbackError)
		}
	}()
	doc, ok := requestString(configJSON)
	if !ok {
		return C.CString(jsonbridge.EncodeError(jsonbridge.TooLarge()))
	}
	return C.CString(configure(doc))
}

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
	return C.CString(dispatch(dispatcher(), request))
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
	return C.CString(shutdown(dispatcher(), int64(timeoutMS)))
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
