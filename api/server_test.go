package api

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/lifecycle"
	"indicer/lib/dbopen"
	"indicer/lib/server"
)

// A server that cannot listen closes the database it opened, and gives its
// export folder up, so the next server on both starts.
func TestServerReleasesDatabaseOnListenFailure(t *testing.T) {
	t.Parallel()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	root := filepath.Join(t.TempDir(), "db")
	opts := Options{Listen: []string{busy.Addr().String()}, Exports: server.ExportConfig{Dir: t.TempDir()}}
	for range 2 {
		err = Server(t.Context(), dbopen.Request{Path: root}, opts)
		if err == nil || !strings.Contains(err.Error(), busy.Addr().String()) {
			t.Fatalf("Server on a port in use: %v, want a listen failure", err)
		}
	}
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatalf("the database after the listen failure: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestServerRefusesExportDirInsideRoot: export settings the server may
// not use are a usage error found before the database is opened or
// created.
func TestServerRefusesExportDirInsideRoot(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		exports func(root string) server.ExportConfig
		msg     string
	}{
		{"inside the root", func(root string) server.ExportConfig { return server.ExportConfig{Dir: filepath.Join(root, "blob")} }, "inside the database"},
		{"relative", func(string) server.ExportConfig { return server.ExportConfig{Dir: "exports"} }, "not an absolute path"},
		{"kept too long", func(string) server.ExportConfig {
			return server.ExportConfig{Keep: (server.MaxExportKeepHours + 1) * time.Hour}
		}, "export keep time"},
		{"drain too short", func(string) server.ExportConfig { return server.ExportConfig{Drain: time.Millisecond} }, "export drain timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "db")
			err := Server(t.Context(), dbopen.Request{Path: root}, Options{Exports: c.exports(root)})
			if lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("error %v (exit %d), want a usage error containing %q", err, lifecycle.ExitCode(err), c.msg)
			}
			if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the database was created: %v", err)
			}
		})
	}
}
