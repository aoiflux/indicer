//go:build faultinject

package e2e

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"indicer/internal/faultinject"
)

// imageCounts reads the image's node and edge counts from graph status.
var imageCounts = regexp.MustCompile(`image\s+.*: ([\d,]+) nodes, ([\d,]+) edges`)

// graphImage runs graph status and returns its image counts and output.
func (l *crashLab) graphImage() ([]string, string) {
	l.t.Helper()
	out := l.must("graph", "status").Stdout
	m := imageCounts.FindStringSubmatch(out)
	if m == nil {
		l.t.Fatalf("graph status shows no image:\n%s", out)
	}
	return m[1:], out
}

// TestCrashDuringGraphCompaction kills dues at each step of a compaction:
// once the store is pinned, once the image is built, once graphene has
// installed it, and before the marker records it. What is left opens and
// lists as it did, and the next compaction leaves what one uninterrupted
// compaction leaves: the same nodes and edges, recorded in the marker, with
// the segments in one chain.
func TestCrashDuringGraphCompaction(t *testing.T) {
	ref := newCrashLab(t)
	for _, f := range ref.files {
		ref.must("store", f, "--enrich")
	}
	ref.must("graph", "compact")
	want, _ := ref.graphImage()

	for _, point := range []string{
		faultinject.GraphCompactPinned,
		faultinject.GraphCompactBuilt,
		faultinject.GraphCompactCommitted,
		faultinject.GraphMarkerImage,
	} {
		t.Run(point, func(t *testing.T) {
			l := newCrashLab(t)
			for _, f := range l.files {
				l.must("store", f, "--enrich")
			}
			before := l.list()
			l.crash(spec(point, "exit:3"), "graph", "compact")

			if got := l.list(); !reflect.DeepEqual(got.lines, before.lines) {
				t.Fatalf("list after the crash:\n%s\nwant\n%s", strings.Join(got.lines, "\n"), strings.Join(before.lines, "\n"))
			}
			l.must("graph", "compact")
			got, out := l.graphImage()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("image holds %v nodes and edges, an uninterrupted compaction %v\n%s", got, want, out)
			}
			if strings.Contains(out, "note") || !strings.Contains(out, "the chain holds") {
				t.Fatalf("graph status after the second compaction:\n%s", out)
			}
			l.checkStored(l.list())
		})
	}
}
