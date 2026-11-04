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

// A bad preset is the only error: the compression level it never got to
// apply is not blamed.
func TestBadPresetBlamesOnlyThePreset(t *testing.T) {
	for _, raw := range []Raw{
		with(base(cnst.CmdList), func(r *Raw) { r.Preset = "turbo" }),
		with(base(cnst.CmdList), func(r *Raw) { r.Preset = "quick"; r.LowResourcePreset = true }),
	} {
		_, _, err := Resolve(raw, machine)
		if err == nil || strings.Contains(err.Error(), "--"+FlagCompressLevel) {
			t.Errorf("preset %q: %v", raw.Preset, err)
		}
	}
}

// A value from the config file says so.
func TestMessagesNameTheOrigin(t *testing.T) {
	origin := map[string]string{FlagStoreWorkers: "config file /c.json"}
	raw := with(base(cnst.CmdStore), func(r *Raw) { r.StoreWorkers = -1; r.Set = set(FlagStoreWorkers); r.Origin = origin })
	_, _, err := Resolve(raw, machine)
	if want := "--store-workers (set in config file /c.json): must be"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want %q", err, want)
	}
	raw = with(base(cnst.CmdRestore), func(r *Raw) {
		r.RestoreBufferMB = 300
		r.Origin = map[string]string{FlagRestoreBufferMB: "config file /c.json"}
	})
	_, issues, err := Resolve(raw, machine)
	if err != nil || len(issues) != 1 || !strings.HasPrefix(issues[0].String(), "--restore-buffer-mb (set in config file /c.json): ") {
		t.Fatalf("got %v, %v", issues, err)
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
	if w, q := cnst.DefaultStoreTuning(machine.CPUs, false, false); eff.StoreWorkers != w || eff.StoreQueue != q {
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

// TestTuningFitsTheDatabasesChunks: an existing database's chunks are what
// the workers and queues hold, whatever --chonksize says; its defaults that do
// not fit come down, and counts the user gave that do not fit are refused
// without asking for another chunk size, which the database fixed.
func TestTuningFitsTheDatabasesChunks(t *testing.T) {
	sys := Sys{CPUs: 8, AvailableRAM: 4 << 30} // a 1 GiB budget
	if eff, issues, err := Resolve(base(cnst.CmdStore), sys); err != nil || len(issues) != 0 {
		t.Fatalf("256 KiB chunks: %+v, %v, %v; want the defaults", eff, issues, err)
	}
	for _, cmd := range []string{cnst.CmdStore, cnst.CmdRestore} {
		eff, issues, err := Resolve(with(base(cmd), func(r *Raw) { r.DBChunkKiB = 65536 }), sys)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		w, q := eff.StoreWorkers, eff.StoreQueue
		if cmd == cnst.CmdRestore {
			w, q = eff.RestoreWorkers, eff.RestoreQueue
		}
		if w < 1 || int64(w+q)*64<<20 > 1<<30 {
			t.Fatalf("%s: %d workers and a queue of %d of 64 MiB chunks exceed the 1 GiB budget", cmd, w, q)
		}
		if len(issues) != 1 || !strings.Contains(issues[0].Message, "the database's 65536 KiB chunks") {
			t.Fatalf("%s: issues %v, want one naming the database's chunks", cmd, issues)
		}
	}
	_, _, err := Resolve(with(base(cnst.CmdStore), func(r *Raw) {
		r.DBChunkKiB, r.StoreWorkers, r.Set = 65536, 16, set(FlagStoreWorkers)
	}), sys)
	if err == nil || !strings.Contains(err.Error(), "the database's 65536 KiB chunks") || strings.Contains(err.Error(), FlagChunkSize) {
		t.Fatalf("--store-workers 16 over the budget: %v; want a refusal naming the database's chunks, not --%s", err, FlagChunkSize)
	}
}

func TestMachineReportsCPUs(t *testing.T) {
	if Machine().CPUs < 1 {
		t.Fatal("no CPUs reported")
	}
}

// TestHierarchicalRetired: D9 retires --hierarchical. It still parses, so
// scripts keep working, but it changes nothing and says so.
func TestHierarchicalRetired(t *testing.T) {
	for _, container := range []bool{false, true} {
		eff, issues, err := Resolve(with(base(cnst.CmdStore), func(r *Raw) {
			r.Hierarchical, r.Container = true, container
		}), machine)
		if err != nil {
			t.Fatal(err)
		}
		if eff.Container != container {
			t.Fatalf("--container=%v with --hierarchical: container %v", container, eff.Container)
		}
		if len(issues) != 1 || issues[0].Flag != FlagHierarchical || !strings.Contains(issues[0].Message, "no effect") {
			t.Fatalf("--hierarchical gave issues %v, want one notice that it has no effect", issues)
		}
	}
}

func TestRestoreClampsWithWarnings(t *testing.T) {
	eff, issues, err := Resolve(with(base(cnst.CmdRestore), func(r *Raw) {
		r.RestoreBufferMB = 4096
	}), machine)
	if err != nil {
		t.Fatal(err)
	}
	if eff.RestoreBufferMB != 256 || len(issues) != 1 {
		t.Fatalf("eff %+v issues %v; want clamped to 256 MB with a warning", eff, issues)
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
