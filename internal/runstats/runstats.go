// Package runstats samples what the Go runtime spent, for logs that compare
// one stretch of a run with another.
package runstats

import (
	"math"
	"runtime/metrics"

	"go.uber.org/zap"
)

// Runtime is the runtime's counters at one moment. Cumulative ones only
// mean something as the difference of two samples (Since).
type Runtime struct {
	// GCCycles and GCCPUSeconds are the collections finished so far and the
	// CPU time they took, the pauses included.
	GCCycles     uint64
	GCCPUSeconds float64
	// GCPauseSeconds is the time the world stood stopped for collections,
	// as near as the runtime's histogram tells it.
	GCPauseSeconds float64
	// HeapLive is what the last collection left on the heap, HeapGoal where
	// the next starts, and Mapped all the memory the runtime holds.
	HeapLive, HeapGoal, Mapped uint64
	Goroutines                 uint64
}

var names = []string{
	"/gc/cycles/total:gc-cycles",
	"/cpu/classes/gc/total:cpu-seconds",
	"/sched/pauses/total/gc:seconds",
	"/gc/heap/live:bytes",
	"/gc/heap/goal:bytes",
	"/memory/classes/total:bytes",
	"/sched/goroutines:goroutines",
}

// Sample reads the counters now.
func Sample() Runtime {
	s := make([]metrics.Sample, len(names))
	for i, n := range names {
		s[i].Name = n
	}
	metrics.Read(s)
	var r Runtime
	r.GCCycles = uint64Of(s[0])
	r.GCCPUSeconds = floatOf(s[1])
	r.GCPauseSeconds = histogramSum(s[2])
	r.HeapLive = uint64Of(s[3])
	r.HeapGoal = uint64Of(s[4])
	r.Mapped = uint64Of(s[5])
	r.Goroutines = uint64Of(s[6])
	return r
}

// Since is what the runtime spent from before to r: the cumulative counters
// as differences, the others as r has them.
func (r Runtime) Since(before Runtime) Runtime {
	r.GCCycles -= min(before.GCCycles, r.GCCycles)
	r.GCCPUSeconds = max(0, r.GCCPUSeconds-before.GCCPUSeconds)
	r.GCPauseSeconds = max(0, r.GCPauseSeconds-before.GCPauseSeconds)
	return r
}

// Fields are r as log fields, each name prefixed with prefix.
func (r Runtime) Fields(prefix string) []zap.Field {
	return []zap.Field{
		zap.Uint64(prefix+"gc_cycles", r.GCCycles),
		zap.Int64(prefix+"gc_cpu_ms", int64(r.GCCPUSeconds*1000)),
		zap.Int64(prefix+"gc_pause_us", int64(r.GCPauseSeconds*1e6)),
		zap.Uint64(prefix+"heap_live_bytes", r.HeapLive),
		zap.Uint64(prefix+"heap_goal_bytes", r.HeapGoal),
		zap.Uint64(prefix+"mapped_bytes", r.Mapped),
		zap.Uint64(prefix+"goroutines", r.Goroutines),
	}
}

func uint64Of(s metrics.Sample) uint64 {
	if s.Value.Kind() == metrics.KindUint64 {
		return s.Value.Uint64()
	}
	return 0
}

func floatOf(s metrics.Sample) float64 {
	if s.Value.Kind() == metrics.KindFloat64 {
		return s.Value.Float64()
	}
	return 0
}

// histogramSum estimates a histogram's total from each bucket's count at the
// bucket's lower bound, so it never overstates.
func histogramSum(s metrics.Sample) float64 {
	if s.Value.Kind() != metrics.KindFloat64Histogram {
		return 0
	}
	h := s.Value.Float64Histogram()
	var sum float64
	for i, n := range h.Counts {
		if n == 0 {
			continue
		}
		low := h.Buckets[i]
		if math.IsInf(low, 1) {
			continue
		}
		if math.IsInf(low, -1) || math.IsNaN(low) || low < 0 {
			low = 0
		}
		sum += float64(n) * low
	}
	return sum
}
