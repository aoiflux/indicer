package graph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"indicer/internal/appconfig"
	"indicer/internal/diskspace"
	"indicer/internal/faultinject"
	"indicer/internal/progress"
	"indicer/lib/graph/schema"

	"github.com/aoiflux/graphene/store"
	"github.com/dustin/go-humanize"
)

// compactSlack is room a compaction needs on the volume beyond the image it
// writes and the log it keeps: the image's temp file is fsynced before the
// old image is replaced, so both are on disk at once.
const compactSlack = 64 << 20

// freeMemory is the machine's available memory, 0 when unknown, and
// checkSpace the volume check; tests replace them.
var (
	freeMemory = func() uint64 { return appconfig.Machine().AvailableRAM }
	checkSpace = diskspace.Check
)

// ErrCompactMemory is returned when a compaction would need more memory than
// the machine has free. Nothing is changed.
var ErrCompactMemory = errors.New("graph: not enough free memory to compact the graph")

// CompactReport says what a compaction did.
type CompactReport struct {
	Dir string
	// LogBytes is the size of the log the compaction folded, which it kept as
	// Segment.
	LogBytes int64
	Segment  string
	// Image is the image now recorded in the marker, and ImageBytes its size.
	Image      schema.ImageRecord
	ImageBytes int64
	Took       time.Duration
}

// Compact folds h's log into its image, keeps the log as a retired segment,
// and records the image in the schema marker. Node and edge IDs do not
// change.
//
// It checks first that the volume can take the image beside the old one and
// that the machine has the memory to build it; either refusal changes
// nothing. Cancelling ctx stops the build and leaves the store as it was;
// once graphene installs the image, the compaction finishes.
//
// A handle opened with OpenOptions.Stages shows the build's stages on its
// progress line; any other shows the compaction as one step.
func Compact(ctx context.Context, h *Handle) (CompactReport, error) {
	rep := CompactReport{Dir: h.Dir()}
	if h.ReadOnly() {
		return rep, fmt.Errorf("graph: %s is open read-only; it cannot be compacted", h.Dir())
	}
	g := h.Graph()
	if g == nil {
		return rep, fmt.Errorf("graph: compact %s: the handle is released", h.Dir())
	}
	rep.LogBytes = fileSize(filepath.Join(h.dir, logFileName))
	need := fileSize(filepath.Join(h.dir, ImageFileName)) + rep.LogBytes + compactSlack
	if err := checkSpace(h.dir, uint64(need)); err != nil {
		return rep, fmt.Errorf("graph: not compacted: %w", err)
	}
	// The build holds a second image in memory beside the graph it is built
	// from, so it needs about twice what the graph holds now.
	if est, ok := g.EstimateResident(); ok {
		if free, want := freeMemory(), 2*uint64(max(est.Total, 0)); free != 0 && want > free {
			return rep, fmt.Errorf("%w: it needs about %s and %s is free; close other programs and run dues graph compact",
				ErrCompactMemory, humanize.IBytes(want), humanize.IBytes(free))
		}
	}

	start := time.Now()
	ph := progress.Start(ctx, "graph", progress.None, 0)
	ph.Item("folding the log into the image")
	if h.stages != nil {
		h.stages.watch(func(kind store.MetricKind) {
			switch kind {
			case store.MetricCompactPin:
				_ = faultinject.Hit(faultinject.GraphCompactPinned)
				ph.Item("building the image")
			case store.MetricCompactBuild:
				_ = faultinject.Hit(faultinject.GraphCompactBuilt)
				ph.Item("installing the image")
			case store.MetricCompactCommit:
				_ = faultinject.Hit(faultinject.GraphCompactCommitted)
				ph.Item("recording the image")
			}
		})
		defer h.stages.watch(nil)
	}
	err := h.ExclusiveWrite(func() error {
		if err := g.CompactCtx(ctx); err != nil {
			return err
		}
		if err := faultinject.Hit(faultinject.GraphMarkerImage); err != nil {
			return err
		}
		rec, err := recordImage(h.dir)
		if err != nil {
			return err
		}
		rep.Image = *rec
		return nil
	})
	if err != nil {
		ph.Done(err)
		return rep, fmt.Errorf("graph: compact %s: %w", h.Dir(), err)
	}
	rep.Took = time.Since(start)
	rep.ImageBytes = fileSize(filepath.Join(h.dir, ImageFileName))
	if rep.Image.SegmentSeq > 0 {
		rep.Segment = segmentName(rep.Image.SegmentSeq - 1)
	}
	ph.Done(nil)
	return rep, nil
}

// recordImage records dir's image in its schema marker, which a writable
// open has already written.
func recordImage(dir string) (*schema.ImageRecord, error) {
	m, found, err := schema.ReadMarker(dir)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("graph: %s has no schema marker", dir)
	}
	rec, changed := adoptImage(dir, m.Image)
	if rec == nil {
		return nil, fmt.Errorf("graph: the image in %s cannot be read", dir)
	}
	if changed {
		m.Image = rec
		if err := schema.WriteMarker(dir, m); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

// segmentName is the file a compaction retires log segment seq to.
func segmentName(seq uint64) string {
	return fmt.Sprintf("%s%06d%s", segmentPrefix, seq, segmentSuffix)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// stageSink passes the compaction stages graphene reports to whoever is
// watching. graphene takes its metrics sink when the store opens, and with
// one attached reads the clock around every query and commit, so only a
// handle opened with OpenOptions.Stages has one.
type stageSink struct {
	fn atomic.Pointer[func(store.MetricKind)]
}

// watch sets the function the stages go to; nil stops them.
func (s *stageSink) watch(fn func(store.MetricKind)) {
	if fn == nil {
		s.fn.Store(nil)
		return
	}
	s.fn.Store(&fn)
}

// Record is graphene's call. A stage that failed is not passed on: the
// compaction did not get past it.
func (s *stageSink) Record(m store.Metric) {
	switch m.Kind {
	case store.MetricCompactPin, store.MetricCompactBuild, store.MetricCompactCommit:
	default:
		return
	}
	if m.Err != nil {
		return
	}
	if fn := s.fn.Load(); fn != nil {
		(*fn)(m.Kind)
	}
}
