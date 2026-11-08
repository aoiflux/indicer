//go:build !windows

package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadRefusesWhatOthersCanChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode os.FileMode
		ok   bool
	}{{0o600, true}, {0o644, true}, {0o400, true}, {0o664, false}, {0o646, false}, {0o666, false}} {
		if err := os.Chmod(p, tc.mode); err != nil {
			t.Fatal(err)
		}
		var c testConfig
		_, err := Load(p, &c)
		if tc.ok != (err == nil) {
			t.Errorf("mode %04o: got %v", tc.mode, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "run chmod go-w on it")) {
			t.Errorf("mode %04o: the error says how to fix it: %v", tc.mode, err)
		}
	}
}

func TestCheckAccessOwner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := *info.Sys().(*syscall.Stat_t)
	other := fakeInfo{FileInfo: info, sys: &st}
	st.Uid = uint32(os.Getuid()) + 1
	if os.Getuid() == 0 {
		st.Uid = 1
	}
	if msg := checkAccess(other); !strings.Contains(msg, "not to you") {
		t.Fatalf("another account's file: %q", msg)
	}
	st.Uid = 0
	if msg := checkAccess(fakeInfo{FileInfo: info, sys: &st, perm: 0o644}); msg != "" {
		t.Fatalf("root's file: %q", msg)
	}
}

type fakeInfo struct {
	os.FileInfo
	sys  *syscall.Stat_t
	perm os.FileMode
}

func (f fakeInfo) Sys() any          { return f.sys }
func (f fakeInfo) Mode() os.FileMode { return f.perm }

func TestLoadRefusesAFIFOWithoutWaiting(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		var c testConfig
		_, err := Load(p, &c)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.HasSuffix(err.Error(), "is not a regular file") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Load waited for a writer")
	}
}
