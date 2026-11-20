package e2e

import (
	"os"
	"testing"

	"indicer/internal/testutil/fsfixture"
)

func fixtureNamed(t *testing.T, name string) fsfixture.Manifest {
	t.Helper()
	for _, m := range fsfixture.All(t) {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no fixture %q", name)
	return fsfixture.Manifest{}
}

func copyFile(t *testing.T, src, dst string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// fsfixtureImage returns a decompressed copy of the named fixture image.
func fsfixtureImage(t *testing.T, name string) string {
	t.Helper()
	return fsfixture.Open(t, fixtureNamed(t, name))
}
