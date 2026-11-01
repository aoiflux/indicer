package fsx

import (
	"os"
	"testing"
	"time"

	"indicer/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunChildScenarios(m, map[string]func(args []string) int{
		// Creates a delete-on-close temporary file in the directory args[0],
		// reports it, and waits to be killed.
		"hold-temp": func(args []string) int {
			f, err := CreateTempDeleteOnClose(args[0], "spill-*")
			if err != nil {
				return 1
			}
			if _, err := f.Write([]byte("evidence bytes")); err != nil {
				return 1
			}
			if err := testutil.NotifyPoint("created"); err != nil {
				return 1
			}
			time.Sleep(time.Minute)
			return 0
		},
	}))
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCreateTempDeleteOnClose(t *testing.T) {
	dir := t.TempDir()
	f, err := CreateTempDeleteOnClose(dir, "spill-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := f.ReadAt(b, 3); err != nil || string(b) != "3456" {
		t.Fatalf("ReadAt: %q, %v", b, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if names := dirEntries(t, dir); len(names) != 0 {
		t.Fatalf("closed temporary file still present: %v", names)
	}
}

// TestCreateTempDeleteOnCloseNeedsDir: an empty directory used to mean
// os.TempDir, which reads TMPDIR, TMP and TEMP.
func TestCreateTempDeleteOnCloseNeedsDir(t *testing.T) {
	if f, err := CreateTempDeleteOnClose("", "spill-*"); err == nil {
		_ = f.Close()
		t.Fatalf("CreateTempDeleteOnClose(\"\") created %s", f.Name())
	}
}

// TestCreateTempDeleteOnCloseAfterCrash checks that a process killed while
// it holds the file leaves nothing behind: the file can hold decompressed
// evidence.
func TestCreateTempDeleteOnCloseAfterCrash(t *testing.T) {
	dir := t.TempDir()
	child := testutil.StartChild(t, "hold-temp", dir)
	child.WaitForPoint(t, "created", 30*time.Second)
	child.Kill(t)
	child.Wait(t, 30*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for {
		names := dirEntries(t, dir)
		if len(names) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed process left %v", names)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
