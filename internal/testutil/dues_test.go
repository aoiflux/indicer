package testutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveBuilds: RemoveBuilds removes each build's folder and forgets
// the build, so the next BuildBinary builds again.
func TestRemoveBuilds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dues-bin-1")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "dues")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &build{path: bin}
	b.once.Do(func() {})
	const key = "fake|"
	buildMu.Lock()
	builds[key] = b
	buildMu.Unlock()

	if err := RemoveBuilds(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the build folder is still there: %v", err)
	}
	buildMu.Lock()
	defer buildMu.Unlock()
	if _, ok := builds[key]; ok {
		t.Error("the removed build is still remembered")
	}
}

// TestFailedBuildLeavesNothing: a build that fails removes its folder.
func TestFailedBuildLeavesNothing(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	if _, msg := buildBinary(temp, "./no/such/package", nil); msg == "" {
		t.Fatal("built a package that does not exist")
	}
	if left, err := os.ReadDir(temp); err != nil || len(left) != 0 {
		t.Errorf("a failed build left %v (%v)", left, err)
	}
}
