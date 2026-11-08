package diskspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
)

func TestFree(t *testing.T) {
	t.Parallel()
	n, err := Free(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no free space reported for the test temp directory")
	}
	if _, err := Free(filepath.Join(t.TempDir(), "missing", "dir")); err == nil {
		t.Fatal("Free succeeded for a path that does not exist")
	}
}

func TestIsNoSpace(t *testing.T) {
	t.Parallel()
	full := testutil.ErrNoSpace()
	if !IsNoSpace(full) || !IsNoSpace(fmt.Errorf("write blob: %w", &os.PathError{Op: "write", Path: "x", Err: full})) {
		t.Fatal("the platform disk-full error was not recognised")
	}
	if IsNoSpace(nil) || IsNoSpace(errors.New("disk full")) || IsNoSpace(os.ErrPermission) {
		t.Fatal("an unrelated error was recognised as disk-full")
	}
}

// TestIsNoSpaceOnRealDevice fills the tmpfs that scripts/wsl/mounts.sh
// provides, so the error comes from the kernel rather than a fake.
func TestIsNoSpaceOnRealDevice(t *testing.T) {
	t.Parallel()
	dir := testutil.ENOSPCDir.Require(t)
	f, err := os.CreateTemp(dir, "fill-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := 0; i < 1<<12; i++ {
		if _, err = f.Write(buf); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("wrote 4 GiB to the ENOSPC test mount without an error")
	}
	if !IsNoSpace(err) {
		t.Fatalf("real disk-full error %v not recognised", err)
	}
}
