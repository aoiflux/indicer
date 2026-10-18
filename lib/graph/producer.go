package graph

import (
	"strings"
)

// ProducerVersionProperty is the property key carrying the version of the code
// that derived a node or edge.
const ProducerVersionProperty = "producer_version"

// grapheneVersion is the storage engine this build links against, read from
// the build information (GRA-11). It belongs in the producer string because a
// change to how the engine encodes or indexes data is as capable of changing a
// derived result as a change to DUES.
var grapheneVersion = "graphene@" + linkedGrapheneVersion

// Producer identifies the code that derived a node or edge.
//
// # Why this is stamped from the start
//
// Raw evidence is immutable; everything above it — enriched properties,
// observations, entities, edges — is derived, and derived data is only
// trustworthy if you can say which version of which code produced it. That is
// what makes a later re-derivation a reviewable diff ("+N edges, K confidence
// changes, R retractions") rather than a silent overwrite, and what lets a
// report answer "what did we know, and when?".
//
// ATTACK_PATH_GRAPH_LLD.md §16 is explicit that this is cheap to add now and
// expensive to retrofit, because data written without it can never be
// attributed afterwards. So it is applied by the batch itself rather than left
// to each call site, where it would eventually be forgotten.
type Producer struct {
	// Pipeline names the producing subsystem, e.g. "enrichment".
	Pipeline string
	// Version is that subsystem's own version — a rule-sheet version, a
	// detector-set version — not the DUES release.
	Version string
}

// DefaultProducer is used when a writer names none, so that no derived node can
// be written unattributed.
var DefaultProducer = Producer{Pipeline: "dues", Version: "0"}

// String renders the stamp, e.g. "enrichment@2/graphene@v0.4.0".
func (p Producer) String() string {
	pipeline := p.Pipeline
	if pipeline == "" {
		pipeline = DefaultProducer.Pipeline
	}
	version := p.Version
	if version == "" {
		version = DefaultProducer.Version
	}
	var b strings.Builder
	b.WriteString(pipeline)
	b.WriteByte('@')
	b.WriteString(version)
	b.WriteByte('/')
	b.WriteString(grapheneVersion)
	return b.String()
}

// Zero reports whether the producer names nothing.
func (p Producer) Zero() bool { return p.Pipeline == "" && p.Version == "" }
