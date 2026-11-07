package cli

import (
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/engine"
)

// TestCommonOpensWithTheCommandsTuning: a command's database runs with the
// tuning its flags gave (DBOptions.Tuning), and opening it changes no
// process-wide setting.
func TestCommonOpensWithTheCommandsTuning(t *testing.T) {
	tun := cnst.DefaultTuning()
	tun.LowResource, tun.CompressLevel, tun.HashStrategy, tun.StoreWorkers = true, "fast", cnst.AsyncHashStrategy, 3
	opts := optsFor(filepath.Join(t.TempDir(), "db"), 64, nil)
	opts.Tuning = &tun
	before := cnst.Globals()

	h, err := Common(t.Context(), opts, dbopen.ReadWrite, false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if got := engine.For(h.DB).Tuning(); got != tun {
		t.Fatalf("the database runs with %+v, want the command's %+v", got, tun)
	}
	if after := cnst.Globals(); after != before {
		t.Fatalf("the open changed the process-wide tuning from %+v to %+v", before, after)
	}
}
