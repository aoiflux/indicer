// Package appconfig validates the command line and works out the settings a
// command runs with.
//
// Resolve is a pure function of the flags and the machine, so every rule is
// table-tested. A value that would crash or exhaust the machine later (a zero
// chunk size divides by zero, a huge one deadlocks badger, unbounded workers
// exhaust memory) is rejected here, before any database is opened, with a
// message naming the flag.
package appconfig

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"

	"indicer/internal/lifecycle"
	"indicer/lib/cnst"

	"github.com/shirou/gopsutil/v3/mem"
)

// Flag names, as the user types them.
const (
	FlagChunkSize       = cnst.FlagChonkSize
	FlagPreset          = cnst.FlagPreset
	FlagQuickMode       = cnst.FlagQuickMode
	FlagPerformanceMode = cnst.FlagPerformanceMode
	FlagLowResourceMode = cnst.FlagLowResourceMode
	FlagLow             = cnst.FlagLowResource
	FlagQuick           = cnst.FlagFastMode
	FlagContainer       = cnst.FlagContainerMode
	FlagHierarchical    = cnst.FlagHierarchicalIndex
	FlagCompressLevel   = cnst.FlagCompressLevel
	FlagStoreWorkers    = cnst.FlagStoreWorkers
	FlagStoreQueue      = cnst.FlagStoreQueue
	FlagRestoreWorkers  = cnst.FlagRestoreWorkers
	FlagRestoreQueue    = cnst.FlagRestoreQueue
	FlagRestoreBufferMB = cnst.FlagRestoreBufferMB
	FlagListStatus      = cnst.FlagListStatus
	FlagRankAlpha       = cnst.FlagRankAlpha
	FlagTopK            = cnst.FlagTopK
	FlagMicroTopK       = "top-k"
)

// Limits.
const (
	MinChunkKiB  = 4
	MaxChunkKiB  = 65536
	MaxQueue     = 65536
	MaxNearTopK  = 1000
	MaxMicroTopK = 1_000_000
	// minMemoryBudget is the in-flight chunk budget on any machine, however
	// little memory it reports.
	minMemoryBudget = 256 << 20
)

// Raw is the command line as parsed: every value, plus which flags the user
// set explicitly. Defaults matter: --quick-mode=false is a flag the user set
// to false, not a request for the quick preset.
type Raw struct {
	Command string // kingpin's full command, e.g. "store" or "near in"

	ChunkKiB int

	Preset            string
	QuickModePreset   bool
	PerformancePreset bool
	LowResourcePreset bool

	Low, Quick, Container, Hierarchical bool
	CompressLevel                       string

	StoreWorkers, StoreQueue int

	RestoreWorkers, RestoreQueue int
	RestoreBufferMB              int

	ListStatus string
	RankAlpha  float64
	NearTopK   int
	MicroTopK  int

	// Set holds the long names of the flags the user set explicitly.
	Set map[string]bool
}

// Sys describes the machine.
type Sys struct {
	CPUs         int
	AvailableRAM uint64 // 0 when unknown
}

// Machine describes the machine DUES is running on.
func Machine() Sys {
	sys := Sys{CPUs: runtime.NumCPU()}
	if vm, err := mem.VirtualMemory(); err == nil {
		sys.AvailableRAM = vm.Available
	}
	return sys
}

// Effective is what the command runs with.
type Effective struct {
	Preset string

	ChunkKiB              int
	Low, Quick, Container bool
	CompressLevel         string

	StoreWorkers, StoreQueue     int
	RestoreWorkers, RestoreQueue int
	RestoreBufferMB              int

	ListStatus string
}

// Issue is a setting Resolve adjusted rather than rejected. It is shown to the
// user as a warning.
type Issue struct {
	Flag    string
	Message string
}

func (i Issue) String() string {
	if i.Flag == "" {
		return i.Message
	}
	return "--" + i.Flag + ": " + i.Message
}

// FlagError is one rejected flag value.
type FlagError struct {
	Flag    string
	Message string
}

func (e *FlagError) Error() string { return "--" + e.Flag + ": " + e.Message }

// Resolve validates raw and returns the effective settings, the adjustments
// made, and an error wrapping a *lifecycle.UsageError (exit code 2) that lists
// every rejected flag.
func Resolve(raw Raw, sys Sys) (Effective, []Issue, error) {
	r := resolver{raw: raw, sys: sys}
	r.resolve()
	if len(r.errs) > 0 {
		return Effective{}, nil, &lifecycle.UsageError{Err: errors.Join(r.errs...)}
	}
	return r.eff, r.issues, nil
}

type resolver struct {
	raw    Raw
	sys    Sys
	eff    Effective
	issues []Issue
	errs   []error
}

func (r *resolver) set(flag string) bool { return r.raw.Set[flag] }

func (r *resolver) reject(flag, format string, args ...any) {
	r.errs = append(r.errs, &FlagError{Flag: flag, Message: fmt.Sprintf(format, args...)})
}

func (r *resolver) warn(flag, format string, args ...any) {
	r.issues = append(r.issues, Issue{Flag: flag, Message: fmt.Sprintf(format, args...)})
}

func (r *resolver) resolve() {
	raw := r.raw
	if r.sys.CPUs < 1 {
		r.sys.CPUs = 1
	}

	// Chunk size: a zero divides by zero, a negative one panics and a huge
	// one exhausts memory or stalls badger (CFG-5).
	r.eff.ChunkKiB = raw.ChunkKiB
	switch {
	case raw.ChunkKiB < MinChunkKiB || raw.ChunkKiB > MaxChunkKiB:
		r.reject(FlagChunkSize, "%d KiB is out of range; use %d to %d", raw.ChunkKiB, MinChunkKiB, MaxChunkKiB)
	case raw.ChunkKiB%4 != 0:
		r.reject(FlagChunkSize, "%d KiB is not a multiple of 4", raw.ChunkKiB)
	}

	r.resolvePreset()
	r.resolveCompressLevel()

	r.eff.Container = raw.Container
	if raw.Hierarchical {
		// D9: chunk locations always go to the database now; databases
		// written in hierarchical mode stay readable.
		r.warn(FlagHierarchical, "is retired and has no effect; use --%s for container storage", FlagContainer)
	}

	switch raw.Command {
	case cnst.CmdStore, cnst.CmdServer:
		r.resolveStoreTuning()
	case cnst.CmdTui:
		// The TUI stores and restores, with default tuning only.
		r.resolveStoreTuning()
		r.resolveRestoreTuning()
	case cnst.CmdRestore:
		r.resolveRestoreTuning()
	case cnst.CmdList:
		r.resolveListStatus()
	case cnst.CmdSearch:
		if math.IsNaN(raw.RankAlpha) || math.IsInf(raw.RankAlpha, 0) || raw.RankAlpha < 0 {
			r.reject(FlagRankAlpha, "must be a finite number >= 0, got %v", raw.RankAlpha)
		}
	case cnst.CmdNear + " " + cnst.SubCmdIn, cnst.CmdNear + " " + cnst.SubCmdOut:
		if raw.NearTopK < 0 || raw.NearTopK > MaxNearTopK {
			r.reject(FlagTopK, "must be 0 (automatic) to %d, got %d", MaxNearTopK, raw.NearTopK)
		}
	case cnst.CmdMicro + " extract":
		if raw.MicroTopK < 0 || raw.MicroTopK > MaxMicroTopK {
			r.reject(FlagMicroTopK, "must be 0 (all) to %d, got %d", MaxMicroTopK, raw.MicroTopK)
		}
	}
}

// resolvePreset picks at most one preset and applies its defaults to every
// setting the user did not set explicitly.
func (r *resolver) resolvePreset() {
	raw := r.raw
	var chosen []string
	if strings.TrimSpace(raw.Preset) != "" {
		p := normalizePreset(raw.Preset)
		if p == "" {
			r.reject(FlagPreset, "unknown preset %q; use %s, %s or %s", raw.Preset, cnst.PresetQuick, cnst.PresetPerformance, cnst.PresetLowResource)
			return
		}
		chosen = append(chosen, p)
	}
	if raw.QuickModePreset {
		chosen = append(chosen, cnst.PresetQuick)
	}
	if raw.PerformancePreset {
		chosen = append(chosen, cnst.PresetPerformance)
	}
	if raw.LowResourcePreset {
		chosen = append(chosen, cnst.PresetLowResource)
	}
	for _, p := range chosen[min(1, len(chosen)):] {
		if p != chosen[0] {
			r.reject(FlagPreset, "conflicting presets %s and %s; choose one", chosen[0], p)
			return
		}
	}

	r.eff.Low, r.eff.Quick = raw.Low, raw.Quick
	r.eff.CompressLevel = strings.ToLower(strings.TrimSpace(raw.CompressLevel))
	if len(chosen) == 0 {
		return
	}
	r.eff.Preset = chosen[0]

	low, quick, level := false, false, "best"
	switch r.eff.Preset {
	case cnst.PresetQuick:
		quick, level = true, "fast"
	case cnst.PresetLowResource:
		low, level = true, "default"
	}
	if !r.set(FlagLow) {
		r.eff.Low = low
	}
	if !r.set(FlagQuick) {
		r.eff.Quick = quick
	}
	if !r.set(FlagCompressLevel) {
		r.eff.CompressLevel = level
	}
}

func normalizePreset(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case cnst.PresetQuick:
		return cnst.PresetQuick
	case cnst.PresetPerformance:
		return cnst.PresetPerformance
	case "low", "low-resource", "lowresource":
		return cnst.PresetLowResource
	}
	return ""
}

func (r *resolver) resolveCompressLevel() {
	switch r.eff.CompressLevel {
	case "fast", "default", "best":
	default:
		r.reject(FlagCompressLevel, "unknown level %q; use fast, default or best", r.raw.CompressLevel)
	}
}

// maxWorkers bounds an explicit worker count.
func (r *resolver) maxWorkers() int { return max(64, 8*r.sys.CPUs) }

// memoryBudget is how many bytes of chunks may be in flight at once.
func (r *resolver) memoryBudget() int64 {
	return max(int64(minMemoryBudget), int64(r.sys.AvailableRAM/4))
}

// checkCounts validates explicit worker and queue overrides.
func (r *resolver) checkCounts(workersFlag string, workers int, queueFlag string, queue int) bool {
	ok := true
	if workers < 0 || workers > r.maxWorkers() {
		r.reject(workersFlag, "must be 0 (automatic) to %d, got %d", r.maxWorkers(), workers)
		ok = false
	}
	if queue < 0 || queue > MaxQueue {
		r.reject(queueFlag, "must be 0 (automatic) to %d, got %d", MaxQueue, queue)
		ok = false
	}
	return ok
}

// fitBudget applies overrides to the defaults and keeps chunk x (workers +
// queue) within the memory budget. Defaults shrink to fit; values the user
// set explicitly that do not fit are rejected.
func (r *resolver) fitBudget(defW, defQ, workers, queue int, workersFlag, queueFlag string) (int, int) {
	w, q := defW, defQ
	if workers > 0 {
		w = workers
	}
	if queue > 0 {
		q = queue
	}
	w = max(w, 1)
	q = max(q, w)

	chunk := int64(r.eff.ChunkKiB) << 10
	if chunk <= 0 {
		return w, q
	}
	slots := r.memoryBudget() / chunk
	if int64(w+q) <= slots {
		return w, q
	}
	if workers > 0 || queue > 0 {
		r.reject(workersFlag, "%d workers and a queue of %d with %d KiB chunks need %d MiB in flight, over the %d MiB budget; lower --%s, --%s or --%s",
			w, q, r.eff.ChunkKiB, int64(w+q)*chunk>>20, r.memoryBudget()>>20, workersFlag, queueFlag, FlagChunkSize)
		return w, q
	}
	// The largest chunk (64 MiB) against the smallest budget still leaves four
	// slots, so this never goes below two workers and a queue of two.
	w = min(w, max(int(slots/2), 1))
	q = max(min(q, int(slots)-w), w)
	r.warn(FlagChunkSize, "%d KiB chunks: reduced to %d workers and a queue of %d to stay within %d MiB of memory",
		r.eff.ChunkKiB, w, q, r.memoryBudget()>>20)
	return w, q
}

func (r *resolver) resolveStoreTuning() {
	raw := r.raw
	if !r.checkCounts(FlagStoreWorkers, raw.StoreWorkers, FlagStoreQueue, raw.StoreQueue) {
		return
	}
	defW, defQ := cnst.DefaultStoreTuning(r.sys.CPUs, r.eff.Low, r.eff.Container)
	r.eff.StoreWorkers, r.eff.StoreQueue = r.fitBudget(defW, defQ, raw.StoreWorkers, raw.StoreQueue, FlagStoreWorkers, FlagStoreQueue)
}

func (r *resolver) resolveRestoreTuning() {
	raw := r.raw
	if !r.checkCounts(FlagRestoreWorkers, raw.RestoreWorkers, FlagRestoreQueue, raw.RestoreQueue) {
		return
	}
	defW, defQ := cnst.DefaultRestoreTuning(r.sys.CPUs, r.eff.Low)
	r.eff.RestoreWorkers, r.eff.RestoreQueue = r.fitBudget(defW, defQ, raw.RestoreWorkers, raw.RestoreQueue, FlagRestoreWorkers, FlagRestoreQueue)

	// The output buffer keeps its historical behaviour: out-of-range values
	// are clamped, with a warning.
	r.eff.RestoreBufferMB = raw.RestoreBufferMB
	switch {
	case raw.RestoreBufferMB < 0:
		r.eff.RestoreBufferMB = 0
		r.warn(FlagRestoreBufferMB, "negative; using the default")
	case raw.RestoreBufferMB > 256:
		r.eff.RestoreBufferMB = 256
		r.warn(FlagRestoreBufferMB, "%d MB is above the 256 MB maximum; using 256", raw.RestoreBufferMB)
	}
}

func (r *resolver) resolveListStatus() {
	s := strings.ToLower(strings.TrimSpace(r.raw.ListStatus))
	switch s {
	case "all", "completed", "pending", "failed":
		r.eff.ListStatus = s
	default:
		r.reject(FlagListStatus, "unknown status %q; use all, completed, pending or failed", r.raw.ListStatus)
	}
}
