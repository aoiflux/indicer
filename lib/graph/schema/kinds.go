// Package schema names the node and edge types DUES stores in the graph.
//
// # Why a kind is an offset
//
// GrapheneDB reserves a band of the type space for callers and exposes it as
// CustomNodeType(offset). DUES used to write the *result* of that call and keep
// only the expression, so when v0.4.0 widened the type from uint8 to uint16 and
// moved the band from 128 to 32768, every stored label silently stopped
// matching: NodesByType found nothing, hierarchies came back empty, and no
// error was raised anywhere because the files still parsed.
//
// A NodeKind here is the offset alone. The raw value is resolved through a
// Registry that knows which base a given store was written with, so the base
// can move again without taking the data with it. The store records its own
// base in dues.schema.json, and a mismatch against the linked library is
// detected at open rather than discovered in a report.
package schema

import (
	graphstore "github.com/aoiflux/graphene/store"
)

// NodeKind is an offset within graphene's custom node band. It is never a
// graphstore.NodeType: converting one to the other requires a Registry, which
// is the point.
type NodeKind uint16

// EdgeKind is an offset within graphene's custom edge band.
type EdgeKind uint16

// Node kinds.
//
// Band A (0-15) — containers. The first four are frozen: they match the
// ordering the legacy 128-131 labels were written with, which is what lets the
// shim map old values onto them without a translation table.
const (
	KindEvidenceFile NodeKind = 0
	KindPartition    NodeKind = 1
	KindIndexedFile  NodeKind = 2
	KindFile         NodeKind = 3
	// 4-15 reserved for Volume, Acquisition, Container.

	// Band B (16-31) — observations. Span-anchored raw findings that are never
	// deduplicated. Micro-artefacts keep graphene's built-in
	// NodeTypeMicroArtefact, which is unaffected by the band move.
	KindObservation NodeKind = 16

	// Band C (32-127) — canonical entities.
	KindProcess         NodeKind = 32
	KindExecutableFile  NodeKind = 33
	KindRegistryKey     NodeKind = 34
	KindAccount         NodeKind = 35
	KindHost            NodeKind = 36
	KindDomain          NodeKind = 37
	KindURL             NodeKind = 38
	KindCookie          NodeKind = 39
	KindNetworkEndpoint NodeKind = 40
	KindScheduledTask   NodeKind = 41
	KindService         NodeKind = 42
	KindEmailMessage    NodeKind = 43

	KindExecutionEvent NodeKind = 64
	KindLogonEvent     NodeKind = 65
	KindVisitEvent     NodeKind = 66
	KindDownloadEvent  NodeKind = 67

	// Band D (128-255) — analytic products.
	KindAttackPath NodeKind = 128
	KindCluster    NodeKind = 129
	KindHypothesis NodeKind = 130

	// 256-1023 reserved for future core kinds; never assigned dynamically.
	// 1024 and up (band E) are for kinds allocated at runtime for
	// rule-defined taxonomy and persisted in the schema marker.
)

// Edge kinds.
//
// EdgeKindRelation is the artefact-to-artefact relation DUES already writes. On
// disk in a legacy store it is a bare EdgeType(100) rather than an offset —
// that value was a raw literal, never derived from the custom base — so it is
// the one kind whose legacy encoding is not base+offset. Registry special-cases
// it.
const (
	EdgeKindRelation EdgeKind = 0
	// 1-15 reserved for hierarchy edges. Containment uses graphene's built-in
	// EdgeTypeContains, which the band move did not touch.

	EdgeKindEvidences       EdgeKind = 16
	EdgeKindSameEntity      EdgeKind = 17
	EdgeKindExecuted        EdgeKind = 18
	EdgeKindPersistsVia     EdgeKind = 19
	EdgeKindConnectedTo     EdgeKind = 20
	EdgeKindDownloaded      EdgeKind = 21
	EdgeKindRedirectedTo    EdgeKind = 22
	EdgeKindSetCookie       EdgeKind = 23
	EdgeKindCookieFor       EdgeKind = 24
	EdgeKindAuthenticatedAs EdgeKind = 25
	EdgeKindMovedTo         EdgeKind = 26
	EdgeKindTemporalNext    EdgeKind = 27
	EdgeKindAttackStep      EdgeKind = 28
	// 1024 and up are for kinds allocated at runtime, as for nodes.
)

// Legacy encodings. These are facts about bytes already written, not values to
// be re-derived from the linked library — deriving them is exactly the mistake
// that made the shim necessary.
const (
	// LegacyNodeBase is the custom band base in graphene v0.1.3, where
	// NodeType was a uint8 and the band ran [128, 255].
	LegacyNodeBase = graphstore.NodeType(128)

	// LegacyRelationEdge is the raw value DUES wrote for relation edges. It sat
	// below the v0.1.3 custom base and is unchanged by the band move, so it
	// still means what it meant — but it is not offset-derived and cannot be
	// resolved like one.
	LegacyRelationEdge = graphstore.EdgeType(100)

	// legacyMaxNodeKind is the highest kind that can appear in a v1 store. Only
	// these four were ever written, so the shim widens lookups for them alone
	// rather than for every kind in the registry.
	legacyMaxNodeKind = KindFile
)

var nodeKindNames = map[NodeKind]string{
	KindEvidenceFile:    "evidence_file",
	KindPartition:       "partition",
	KindIndexedFile:     "indexed_file",
	KindFile:            "file",
	KindObservation:     "observation",
	KindProcess:         "process",
	KindExecutableFile:  "executable_file",
	KindRegistryKey:     "registry_key",
	KindAccount:         "account",
	KindHost:            "host",
	KindDomain:          "domain",
	KindURL:             "url",
	KindCookie:          "cookie",
	KindNetworkEndpoint: "network_endpoint",
	KindScheduledTask:   "scheduled_task",
	KindService:         "service",
	KindEmailMessage:    "email_message",
	KindExecutionEvent:  "execution_event",
	KindLogonEvent:      "logon_event",
	KindVisitEvent:      "visit_event",
	KindDownloadEvent:   "download_event",
	KindAttackPath:      "attack_path",
	KindCluster:         "cluster",
	KindHypothesis:      "hypothesis",
}

var edgeKindNames = map[EdgeKind]string{
	EdgeKindRelation:        "relation",
	EdgeKindEvidences:       "evidences",
	EdgeKindSameEntity:      "same_entity",
	EdgeKindExecuted:        "executed",
	EdgeKindPersistsVia:     "persists_via",
	EdgeKindConnectedTo:     "connected_to",
	EdgeKindDownloaded:      "downloaded",
	EdgeKindRedirectedTo:    "redirected_to",
	EdgeKindSetCookie:       "set_cookie",
	EdgeKindCookieFor:       "cookie_for",
	EdgeKindAuthenticatedAs: "authenticated_as",
	EdgeKindMovedTo:         "moved_to",
	EdgeKindTemporalNext:    "temporal_next",
	EdgeKindAttackStep:      "attack_step",
}

// containerKinds are the kinds a v1 store could hold, in marker order.
var containerKinds = []NodeKind{KindEvidenceFile, KindPartition, KindIndexedFile, KindFile}
