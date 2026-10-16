package appconfig

import (
	"errors"
	"math"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/cnst"
)

func base(cmd string) Raw {
	return Raw{Command: cmd, ChunkKiB: 256, CompressLevel: "best", ListStatus: "all", RankAlpha: 0.35, MicroTopK: 1}
}

var machine = Sys{CPUs: 8, AvailableRAM: 16 << 30}

func with(r Raw, f func(*Raw)) Raw { f(&r); return r }

func set(flags ...string) map[string]bool {
	m := map[string]bool{}
	for _, f := range flags {
		m[f] = true
	}
	return m
}

func TestResolveRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  Raw
		flag string
	}{
		{"zero chunk", with(base(cnst.CmdList), func(r *Raw) { r.ChunkKiB = 0 }), FlagChunkSize},
		{"negative chunk", with(base(cnst.CmdList), func(r *Raw) { r.ChunkKiB = -4 }), FlagChunkSize},
		{"huge chunk", with(base(cnst.CmdList), func(r *Raw) { r.ChunkKiB = 1 << 20 }), FlagChunkSize},
		{"odd chunk", with(base(cnst.CmdList), func(r *Raw) { r.ChunkKiB = 130 }), FlagChunkSize},
		{"bad level", with(base(cnst.CmdList), func(r *Raw) { r.CompressLevel = "ultra" }), FlagCompressLevel},
		{"bad preset", with(base(cnst.CmdList), func(r *Raw) { r.Preset = "turbo" }), FlagPreset},
		{"two presets", with(base(cnst.CmdList), func(r *Raw) { r.Preset = "quick"; r.LowResourcePreset = true }), FlagPreset},
		{"bad status", with(base(cnst.CmdList), func(r *Raw) { r.ListStatus = "done" }), FlagListStatus},
		{"NaN alpha", with(base(cnst.CmdSearch), func(r *Raw) { r.RankAlpha = math.NaN() }), FlagRankAlpha},
		{"Inf alpha", with(base(cnst.CmdSearch), func(r *Raw) { r.RankAlpha = math.Inf(1) }), FlagRankAlpha},
		{"negative alpha", with(base(cnst.CmdSearch), func(r *Raw) { r.RankAlpha = -1 }), FlagRankAlpha},
		{"near top-k", with(base("near in"), func(r *Raw) { r.NearTopK = MaxNearTopK + 1 }), FlagTopK},
		{"negative near top-k", with(base("near out"), func(r *Raw) { r.NearTopK = -1 }), FlagTopK},
		{"micro top-k", with(base("micro extract"), func(r *Raw) { r.MicroTopK = -1 }), FlagMicroTopK},
		{"negative workers", with(base(cnst.CmdStore), func(r *Raw) { r.StoreWorkers = -1 }), FlagStoreWorkers},
		{"too many workers", with(base(cnst.CmdStore), func(r *Raw) { r.StoreWorkers = 100000 }), FlagStoreWorkers},
		{"huge queue", with(base(cnst.CmdStore), func(r *Raw) { r.StoreQueue = MaxQueue + 1 }), FlagStoreQueue},
		{"restore workers", with(base(cnst.CmdRestore), func(r *Raw) { r.RestoreWorkers = -3 }), FlagRestoreWorkers},
		{"explicit over budget", with(base(cnst.CmdStore), func(r *Raw) {
			r.ChunkKiB = 65536
			r.StoreWorkers = 64
			r.Set = set(FlagStoreWorkers)
		}), FlagStoreWorkers},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Resolve(c.raw, machine)
			var usage *lifecycle.UsageError
			if !errors.As(err, &usage) || lifecycle.ExitCode(err) != lifecycle.ExitUsage {
				t.Fatalf("Resolve error %v; want a usage error (exit 2)", err)
			}
			var fe *FlagError
			if !errors.As(err, &fe) || fe.Flag != c.flag {
				t.Fatalf("error %v does not name --%s", err, c.flag)
			}
			if !strings.Contains(err.Error(), "--"+c.flag) {
				t.Fatalf("message %q does not name --%s", err, c.flag)
			}
		})
	}
}

func TestResolveReportsEveryBadFlag(t *testing.T) {
	raw := with(base(cnst.CmdList), func(r *Raw) { r.ChunkKiB = 0; r.CompressLevel = "x"; r.ListStatus = "y" })
	_, _, err := Resolve(raw, machine)
	for _, f := range []string{FlagChunkSize, FlagCompressLevel, FlagListStatus} {
		if !strings.Contains(err.Error(), "--"+f) {
			t.Errorf("error does not mention --%s: %v", f, err)
		}
	}
}

func TestResolveAcceptsDefaults(t *testing.T) {
	for _, cmd := range []string{cnst.CmdStore, cnst.CmdRestore, cnst.CmdList, cnst.CmdSearch, "near in", "micro extract", cnst.CmdServer, cnst.CmdTui} {
		if _, _, err := Resolve(base(cmd), machine); err != nil {
			t.Errorf("%s with defaults: %v", cmd, err)
		}
	}
	for _, kib := range []int{MinChunkKiB, 64, 4096, MaxChunkKiB} {
		if _, _, err := Resolve(with(base(cnst.CmdStore), func(r *Raw) { r.ChunkKiB = kib }), machine); err != nil {
			t.Errorf("chunk %d KiB rejected: %v", kib, err)
		}
	}
}

// TestPresetHonoursExplicitFalse is the regression for --quick-mode=false
// activating the quick preset: only a flag set to true selects a preset.
func TestPresetHonoursExplicitFalse(t *testing.T) {
	raw := with(base(cnst.CmdStore), func(r *Raw) {
		r.QuickModePreset = false
		r.Set = set(FlagQuickMode)
	})
	eff, _, err := Resolve(raw, machine)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Preset != "" || eff.Quick {
		t.Fatalf("--quick-mode=false selected preset %q (quick=%v)", eff.Preset, eff.Quick)
	}
}

func TestPresetDefaultsYieldToExplicitFlags(t *testing.T) {
	eff, _, err := Resolve(with(base(cnst.CmdStore), func(r *Raw) { r.Preset = "quick" }), machine)
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Quick || eff.Low || eff.CompressLevel != "fast" || eff.Preset != cnst.PresetQuick {
		t.Fatalf("quick preset gave %+v", eff)
	}

	eff, _, err = Resolve(with(base(cnst.CmdStore), func(r *Raw) {
		r.Preset = "quick"
		r.Quick = false
		r.CompressLevel = "best"
		r.Set = set(FlagQuick, FlagCompressLevel)
	}), machine)
	if err != nil {
		t.Fatal(err)
	}
	if eff.Quick || eff.CompressLevel != "best" {
		t.Fatalf("explicit --quick=false and --compress-level=best were overridden: %+v", eff)
	}

	eff, _, err = Resolve(with(base(cnst.CmdStore), func(r *Raw) { r.LowResourcePreset = true }), machine)
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Low || eff.CompressLevel != "default" {
		t.Fatalf("low-resource preset gave %+v", eff)
	}

	// The same preset named twice is not a conflict.
	if _, _, err := Resolve(with(base(cnst.CmdStore), func(r *Raw) { r.Preset = "low"; r.LowResourcePreset = true }), machine); err != nil {
		t.Fatal(err)
	}
}

func TestStoreTuningFitsMemoryBudget(t *testing.T) {
	small := Sys{CPUs: 64, AvailableRAM: 512 << 20}
	eff, issues, err := Resolve(with(base(cnst.CmdStore), func(r *Raw) { r.ChunkKiB = 65536 }), small)
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(eff.StoreWorkers+eff.StoreQueue) * 64 << 20; got > minMemoryBudget {
		t.Fatalf("%d workers + %d queue x 64 MiB = %d MiB in flight, budget %d MiB",
			eff.StoreWorkers, eff.StoreQueue, got>>20, minMemoryBudget>>20)
	}
	if eff.StoreWorkers < 1 || eff.StoreQueue < eff.StoreWorkers {
		t.Fatalf("invalid tuning %d/%d", eff.StoreWorkers, eff.StoreQueue)
	}
	if len(issues) == 0 {
		t.Fatal("defaults were reduced without a warning")
	}

	eff, issues, err = Resolve(base(cnst.CmdStore), machine)
	if err != nil || len(issues) != 0 {
		t.Fatalf("defaults on a normal machine: %v %v", err, issues)
	}
	if w, q := cnst.DefaultStoreTuning(machine.CPUs, false, false, false); eff.StoreWorkers != w || eff.StoreQueue != q {
		t.Fatalf("tuning %d/%d, want the defaults %d/%d", eff.StoreWorkers, eff.StoreQueue, w, q)
	}
}

// TestIngestingCommandsFitMemoryBudget covers the TUI and the server, which
// store evidence with default tuning and have no tuning flags.
func TestIngestingCommandsFitMemoryBudget(t *testing.T) {
	small := Sys{CPUs: 64, AvailableRAM: 512 << 20}
	for _, cmd := range []string{cnst.CmdTui, cnst.CmdServer} {
		eff, issues, err := Resolve(with(base(cmd), func(r *Raw) { r.ChunkKiB = 65536 }), small)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if eff.StoreWorkers < 1 || int64(eff.StoreWorkers+eff.StoreQueue)*64<<20 > minMemoryBudget || len(issues) == 0 {
			t.Fatalf("%s: store tuning %d/%d (issues %v) exceeds the budget or was not reported",
				cmd, eff.StoreWorkers, eff.StoreQueue, issues)
		}
	}
	eff, _, _ := Resolve(with(base(cnst.CmdTui), func(r *Raw) { r.ChunkKiB = 65536 }), small)
	if eff.RestoreWorkers < 1 || int64(eff.RestoreWorkers+eff.RestoreQueue)*64<<20 > minMemoryBudget {
		t.Fatalf("tui restore tuning %d/%d exceeds the budget", eff.RestoreWorkers, eff.RestoreQueue)
	}
}

func TestMachineReportsCPUs(t *testing.T) {
	if Machine().CPUs < 1 {
		t.Fatal("no CPUs reported")
	}
}

func TestHierarchicalEnablesContainer(t *testing.T) {
	eff, issues, err := Resolve(with(base(cnst.CmdStore), func(r *Raw) { r.Hierarchical = true }), machine)
	if err != nil {
		t.Fatal(err)
	}
	if !eff.Container || len(issues) != 1 || issues[0].Flag != FlagHierarchical {
		t.Fatalf("hierarchical without container: eff %+v issues %v", eff, issues)
	}
}

func TestRestoreClampsWithWarnings(t *testing.T) {
	eff, issues, err := Resolve(with(base(cnst.CmdRestore), func(r *Raw) {
		r.RestoreProgressMs = 5
		r.RestoreBufferMB = 4096
	}), machine)
	if err != nil {
		t.Fatal(err)
	}
	if eff.RestoreProgressMs != 20 || eff.RestoreBufferMB != 256 || len(issues) != 2 {
		t.Fatalf("eff %+v issues %v; want clamped to 20 ms and 256 MB with two warnings", eff, issues)
	}
}

func FuzzResolve(f *testing.F) {
	f.Add("store", 256, 0, 0, "best", "all", 0.35, 0)
	f.Add("restore", 0, -1, 1<<30, "x", "pending", math.NaN(), -5)
	f.Fuzz(func(t *testing.T, cmd string, chunk, workers, queue int, level, status string, alpha float64, topk int) {
		raw := Raw{Command: cmd, ChunkKiB: chunk, StoreWorkers: workers, StoreQueue: queue,
			RestoreWorkers: workers, RestoreQueue: queue, CompressLevel: level, ListStatus: status,
			RankAlpha: alpha, NearTopK: topk, MicroTopK: topk}
		eff, _, err := Resolve(raw, Sys{CPUs: 4, AvailableRAM: 1 << 30})
		if err != nil {
			return
		}
		if eff.ChunkKiB < MinChunkKiB || eff.ChunkKiB > MaxChunkKiB {
			t.Fatalf("accepted chunk %d", eff.ChunkKiB)
		}
		if cmd == cnst.CmdStore && (eff.StoreWorkers < 1 || eff.StoreQueue < eff.StoreWorkers) {
			t.Fatalf("accepted store tuning %d/%d", eff.StoreWorkers, eff.StoreQueue)
		}
	})
}
