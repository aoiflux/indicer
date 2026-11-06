package runstats

import (
	"runtime"
	"testing"
)

// TestSampleCountsCollections: a forced collection shows in the difference
// of two samples, and the gauges read as the runtime has them.
func TestSampleCountsCollections(t *testing.T) {
	before := Sample()
	runtime.GC()
	d := Sample().Since(before)
	if d.GCCycles < 1 {
		t.Fatalf("%d collections between the samples, want at least 1", d.GCCycles)
	}
	if d.Goroutines < 1 || d.Mapped == 0 || d.HeapGoal == 0 {
		t.Fatalf("gauges unread: %+v", d)
	}
	if d.GCCPUSeconds < 0 || d.GCPauseSeconds < 0 {
		t.Fatalf("negative spend: %+v", d)
	}
	if len(d.Fields("x_")) != 7 {
		t.Fatalf("%d fields, want 7", len(d.Fields("x_")))
	}
}

// TestSinceNeverGoesNegative: samples given in the wrong order clamp at 0.
func TestSinceNeverGoesNegative(t *testing.T) {
	a := Runtime{GCCycles: 5, GCCPUSeconds: 2, GCPauseSeconds: 1}
	d := Runtime{}.Since(a)
	if d.GCCycles != 0 || d.GCCPUSeconds != 0 || d.GCPauseSeconds != 0 {
		t.Fatalf("got %+v, want zeros", d)
	}
}
