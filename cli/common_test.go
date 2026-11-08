package cli

import (
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/engine"
)

// TestCommonOpensWithTheCommandsTuning: a command's database runs with the
// tuning its flags gave (DBOptions.Tuning), and that tuning stays with it: a
// database opened beside it without one runs with the default tuning.
func TestCommonOpensWithTheCommandsTuning(t *testing.T) {
	tun := cnst.DefaultTuning()
	tun.LowResource, tun.CompressLevel, tun.HashStrategy, tun.StoreWorkers = true, "fast", cnst.AsyncHashStrategy, 3
	opts := optsFor(filepath.Join(t.TempDir(), "db"), 64, nil)
	opts.Tuning = &tun

	h, err := Common(t.Context(), opts, dbopen.ReadWrite, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if got := engine.For(h.DB).Tuning(); got != tun {
		t.Fatalf("the database runs with %+v, want the command's %+v", got, tun)
	}
	other, err := Common(t.Context(), optsFor(filepath.Join(t.TempDir(), "other"), 64, nil), dbopen.ReadWrite, false)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if got, want := engine.For(other.DB).Tuning(), cnst.DefaultTuning(); got != want {
		t.Fatalf("a database opened without a tuning runs with %+v, want the default %+v", got, want)
	}
}
