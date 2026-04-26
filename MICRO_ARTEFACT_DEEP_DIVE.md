# Micro-Artefact Deep Dive (Spec-Driven)

This document operationalizes [instructions.json](instructions.json) for the
current implementation in this repository.

## 1) Current State (What Exists)

The repo already has a working micro-artefact pipeline:

- CLI entry and traversal over stored indexed files:
  [cli/cmdmicro.go](cli/cmdmicro.go)
- Extraction service and detector routing by modality:
  [lib/microartefact/service.go](lib/microartefact/service.go)
- Supported parser kinds (text, pdf, pe, evtx, elf):
  [lib/microartefact/fileparser/fileparser.go](lib/microartefact/fileparser/fileparser.go)
- Detectors (url/ioc/key-value/pe/registry/evtx/log/event/task/service/browser):
  [lib/microartefact/detectors/detectors.go](lib/microartefact/detectors/detectors.go)
- Graph storage in hierarchy disk_image -> partition -> indexed_file ->
  micro_artefact:
  [lib/microartefact/repository/graphene/repository.go](lib/microartefact/repository/graphene/repository.go)
- Hierarchy query/read model:
  [lib/microartefact/repository/graphene/query.go](lib/microartefact/repository/graphene/query.go)

## 2) Gap Analysis vs instructions.json

### 2.1 Canonical node schema

Spec wants canonical node fields:

- id, type, canonical_key, timestamp, origin, payload_ref, features, confidence,
  parser, raw_evidence_snippet

Current artefact model:

- Kind, Detector, Value, Summary, Confidence, Span, Attributes in
  [lib/microartefact/model/types.go](lib/microartefact/model/types.go)

Gap:

- No explicit canonical_key
- No parser version field
- No timestamp/origin/payload_ref as first-class node fields
- No raw evidence snippet field
- Features map exists only partially through Attributes

### 2.2 Canonical edge schema

Spec wants relation edges with:

- relation_type, evidence_fields, deterministic_flag, created_by, created_at,
  confidence

Current graph edges:

- Only contains edges from indexed_file to micro_artefact in
  [lib/microartefact/repository/graphene/repository.go](lib/microartefact/repository/graphene/repository.go)

Gap:

- No deterministic cross-node relation edges (same_file, attached_to,
  references_transaction, etc.)
- No evidence_fields metadata on relation edges
- No deterministic/probabilistic edge tagging

### 2.3 Deterministic-first policy

Spec requires:

1. Persist deterministic links first.
2. Then run probabilistic clustering/linking.

Current pipeline:

- Extract and persist artefacts only; no deterministic linking pass over
  anchors.

Gap:

- Missing canonical anchor index and deterministic linker stage.

### 2.4 Scoring and triage

Spec defines weighted triage formula and threshold-based persistence.

Current pipeline:

- Detector confidence exists per artefact but no global triage score stage.
- All extracted artefacts are persisted when present.

Gap:

- Missing scoring function and configurable NODE_THRESHOLD.

### 2.5 Modality coverage from spec

Spec target modalities include: pdf, txt, log, email, evtx, registry, pe, elf,
pcap.

Current parser support:

- text, pdf, pe, evtx, elf in
  [lib/microartefact/fileparser/fileparser.go](lib/microartefact/fileparser/fileparser.go)

Current detectors partially cover:

- log/event/registry/pe/ioc patterns in
  [lib/microartefact/detectors/detectors.go](lib/microartefact/detectors/detectors.go)

Gap:

- No first-class email parser/extractor
- No pcap parser/extractor
- No deterministic relation builders for cross-modality anchors

## 3) Proposed Target Architecture (Incremental)

### Phase 1: Canonical model and deterministic anchors

Introduce canonical types (new file suggestion):

- lib/microartefact/model/canonical.go

Core structs:

- CanonicalNode
- CanonicalEdge
- EdgeEvidenceField

Implementation details:

- Keep existing Artefact model for compatibility.
- Add mapper Artefact -> CanonicalNode.
- Derive canonical_key for deterministic anchors when available:
  - hash/message-id/txid/ip:port/pid/record id/serial

### Phase 2: Triage scoring gate

Add scoring module (new file suggestion):

- lib/microartefact/triage.go

Default formula:

- score = 0.35*time + 0.35*attribution + 0.20*uniqueness + 0.10*extractability

Initial defaults:

- NODE_THRESHOLD = 0.60
- Only persist canonical nodes score >= threshold
- Persist low-score nodes optionally behind a debug flag

### Phase 3: Deterministic linker pass

Add deterministic linker stage after node persistence:

- lib/microartefact/linker_deterministic.go

Mechanics:

- Build/maintain anchor index keyed by canonical_key + anchor_type
- For each new node, emit deterministic edges where exact anchors match
- Persist edges regardless of UI surfacing

First deterministic relations to implement now (high ROI with existing data):

1. hash_match -> same_file
2. transaction_id_propagation -> references_transaction
3. messageid_attachment -> attached_to (after email parser lands)
4. mft_lnk_shortcut / shellbags_mru (after parsers exist)

### Phase 4: Probabilistic linker and clustering

Add derived edges and cluster metadata:

- lib/microartefact/linker_probabilistic.go

Initial rules:

- time_window_clustering:
  - high-frequency window = 5 minutes
  - human-scale window = 24 hours
- nlp_entity_match:
  - start threshold 0.85 and require second corroborating signal

### Phase 5: Explainability and UI control surface

For each surfaced edge, include:

- field names
- raw snippets
- parser name/version

CLI/TUI outputs to extend:

- show deterministic/probabilistic tags
- add confidence filters
- add export of cluster evidence summary

## 4) Concrete Code Touch Points

Primary edits expected:

- [lib/microartefact/model/types.go](lib/microartefact/model/types.go)
- [lib/microartefact/service.go](lib/microartefact/service.go)
- [lib/microartefact/repository/graphene/repository.go](lib/microartefact/repository/graphene/repository.go)
- [lib/microartefact/repository/graphene/query.go](lib/microartefact/repository/graphene/query.go)
- [cli/cmdmicro.go](cli/cmdmicro.go)

Likely new files:

- triage + scoring
- canonical schema mappings
- deterministic linker
- probabilistic linker
- relation definitions config

## 5) Filetype-specific adjustment to current parser/tooling

Based on currently available parsers/tools in this repo:

- Fully supported now:
  - txt/log-like text
  - pdf (text extraction path)
  - pe
  - elf
  - evtx

- Partially represented:
  - registry artefacts (detector-level on registry hive content)

- Not yet present as first-class modality parsers:
  - email
  - pcap

Recommended immediate policy:

- Keep spec filetype list as target state.
- Mark email/pcap as pending parsers.
- Implement deterministic linking first using existing anchors from current
  detectors.

## 6) Pilot defaults to use now

Use these defaults for first end-to-end pilot:

- NODE_THRESHOLD = 0.60
- Time windows:
  - 5 minutes for event streams/log bursts
  - 24 hours for human workflows (email/docs/transactions)
- Deterministic edge confidence = 1.0 when anchor exact-match is explicit
- Probabilistic edge confidence:
  - start in 0.55 to 0.85 range based on matcher
  - never auto-promote to high confidence without cross-modality corroboration

## 7) Suggested implementation order (2-week practical slice)

1. Canonical node/edge structs + repository persistence extensions.
2. Scoring gate and threshold config.
3. Deterministic anchor index and exact-link pass.
4. Minimal query/export updates to expose edge evidence.
5. Add email parser skeleton (headers + attachments hash extraction).

## 8) Validation checklist

- Deterministic edges are created only from explicit exact anchors.
- Every node/edge has parser metadata and evidence snippet.
- Running extraction twice does not duplicate nodes/edges.
- UI/API can distinguish deterministic vs derived edges.
- Cluster confidence requires at least two modalities for high-confidence
  tagging.
