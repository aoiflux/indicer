package graph_test

import (
	"strings"
	"testing"

	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/props"

	graphstore "github.com/aoiflux/graphene/store"
)

// TestProducer_EveryDerivedNodeIsStamped is the guarantee that makes a later
// re-derivation reviewable: data written without a producer version can never
// be attributed afterwards, so the stamp is applied by the batch rather than
// left to call sites that would eventually forget it.
func TestProducer_EveryDerivedNodeIsStamped(t *testing.T) {
	handle := enrichedStore(t)

	for _, tc := range []struct{ key, value string }{
		{"evidence_file_id", enrEvidenceID},
		{"partition_id", enrPartitionID},
		{"indexed_file_id", enrIndexedID},
	} {
		fields := nodeProps(t, handle, tc.key, tc.value)
		stamp := fields.Str(duesgraph.ProducerVersionProperty)
		if stamp == "" {
			t.Errorf("%s: no producer_version; this node can never be attributed to "+
				"the code that derived it", tc.key)
			continue
		}
		if !strings.HasPrefix(stamp, "enrichment@") {
			t.Errorf("%s: producer_version = %q, want it to name the enrichment pipeline",
				tc.key, stamp)
		}
		if !strings.Contains(stamp, "graphene@") {
			t.Errorf("%s: producer_version = %q, want the storage engine version too",
				tc.key, stamp)
		}
	}
}

// Edges are derived too, and a rule-built edge is exactly the kind of thing a
// re-derivation adds, retracts or re-scores.
func TestProducer_EdgesAreStampedToo(t *testing.T) {
	handle := enrichedStore(t)

	ids, err := handle.Graph().EdgesByType(graphstore.EdgeTypeContains)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("no containment edges to check")
	}
	edges, _, err := handle.Graph().GetEdges(ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range edges {
		fields, err := props.Unpack(edge.Properties)
		if err != nil {
			t.Fatal(err)
		}
		if fields.Str(duesgraph.ProducerVersionProperty) == "" {
			t.Fatalf("edge %d carries no producer_version", edge.ID)
		}
	}
}

// A writer that names no producer must still stamp something, so an omission
// degrades to a vague attribution rather than to none at all.
func TestProducer_DefaultsRatherThanOmitting(t *testing.T) {
	handle, err := duesgraph.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	writer, err := handle.Writer(duesgraph.ActorRef{ActorID: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	batch := writer.Batch()
	if _, err := batch.Upsert(
		duesgraph.UpsertKey{Key: "probe", Value: "v"},
		[]graphstore.NodeType{graphstore.NodeTypeTag},
		props.Map{"n": "x"}, nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	fields := nodeProps(t, handle, "probe", "v")
	if got := fields.Str(duesgraph.ProducerVersionProperty); got == "" {
		t.Error("a writer that named no producer wrote an unstamped node")
	}
}

// A caller carrying a value forward from an earlier derivation keeps that
// value's original stamp, which is what makes an as-of view possible.
func TestProducer_ExplicitStampIsNotOverwritten(t *testing.T) {
	handle, err := duesgraph.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	writer, err := handle.Writer(duesgraph.ActorRef{ActorID: 1})
	if err != nil {
		t.Fatal(err)
	}
	writer.As(duesgraph.Producer{Pipeline: "enrichment", Version: "9"})
	defer writer.Close()

	batch := writer.Batch()
	if _, err := batch.Upsert(
		duesgraph.UpsertKey{Key: "probe", Value: "carried"},
		[]graphstore.NodeType{graphstore.NodeTypeTag},
		props.Map{duesgraph.ProducerVersionProperty: "enrichment@1/graphene@v0.4.0"},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	fields := nodeProps(t, handle, "probe", "carried")
	if got := fields.Str(duesgraph.ProducerVersionProperty); got != "enrichment@1/graphene@v0.4.0" {
		t.Errorf("producer_version = %q, want the carried-forward stamp to survive", got)
	}
}

func TestProducer_StringNamesBothVersions(t *testing.T) {
	got := duesgraph.Producer{Pipeline: "enrichment", Version: "2"}.String()
	if !strings.HasPrefix(got, "enrichment@2/graphene@v") {
		t.Errorf("String() = %q, want the pipeline and the linked graphene version", got)
	}
	if !(duesgraph.Producer{}).Zero() {
		t.Error("the zero Producer does not report itself as zero")
	}
	if (duesgraph.Producer{}).String() == "" {
		t.Error("the zero Producer renders empty; it must still name something")
	}
}
