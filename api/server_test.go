package api

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"indicer/lib/dbopen"
)

// A server that cannot listen closes the database it opened, so the next
// open of it succeeds.
func TestServerReleasesDatabaseOnListenFailure(t *testing.T) {
	t.Parallel()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	root := filepath.Join(t.TempDir(), "db")
	err = Server(t.Context(), dbopen.Request{Path: root}, Options{Listen: []string{busy.Addr().String()}})
	if err == nil || !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("Server on a port in use: %v, want a listen failure", err)
	}
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatalf("the database after the listen failure: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}
