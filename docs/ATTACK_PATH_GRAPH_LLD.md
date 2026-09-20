# Attack-Path Graph — Low-Level Design & Build Plan

> **Status:** Draft v0.1 — living document, meant to be iterated on.
> **Supersedes/extends:** [MICRO_ARTEFACT_DEEP_DIVE.md](MICRO_ARTEFACT_DEEP_DIVE.md)
> (which proposed the canonical node/edge model and triage scoring that this
> document builds into a full attack-path system).
> **Scope:** the end-to-end pipeline that turns raw evidence (disk / RAM / pcap /
> flat file) into a queryable **attack-path graph** (historically called the
> "micro-artefact graph"): parse → enrich container graph → extract & canonicalise
> micro-artefacts → link with a rule engine → assemble and score attack paths.
>
> This is written to be extremely detailed because it is the technical core of the
> forensics product and we will refine it over many passes. Every "CURRENT" claim
> is grounded in the code as it exists today; every "TARGET" claim is a proposal to
> debate.

---

## Table of contents

1. [Goals & non-goals](#1-goals--non-goals)
2. [The layered model (the core idea)](#2-the-layered-model-the-core-idea)
3. [Current state & the constraints we must break](#3-current-state--the-constraints-we-must-break)
4. [Identity strategy — content-anchored canonical keys](#4-identity-strategy--content-anchored-canonical-keys)
5. [Evidence-class taxonomy & multi-modal ingestion (Stage 0)](#5-evidence-class-taxonomy--multi-modal-ingestion-stage-0)
6. [The pipeline stages (the prongs)](#6-the-pipeline-stages-the-prongs)
7. [What counts as a micro-artefact? (the hard problem)](#7-what-counts-as-a-micro-artefact-the-hard-problem)
8. [Graph schema (nodes, edges, properties, IDs)](#8-graph-schema-nodes-edges-properties-ids)
9. [The rule engine](#9-the-rule-engine)
10. [Built-in rule catalogue](#10-built-in-rule-catalogue)
11. [Worked examples](#11-worked-examples)
12. [Cross-evidence correlation (reusing SimHash)](#12-cross-evidence-correlation-reusing-simhash)
13. [Cross-case analysis](#13-cross-case-analysis)
14. [Retrospective analysis](#14-retrospective-analysis)
15. [Hard problems & decisions to make](#15-hard-problems--decisions-to-make)
16. [Phased build roadmap](#16-phased-build-roadmap)
17. [Appendix A — current code map](#17-appendix-a--current-code-map)
18. [Appendix B — glossary](#18-appendix-b--glossary)

---

## 1. Goals & non-goals

### Goals

- Reconstruct, from **dead evidence**, the likely attacker activity as a graph:
  execution, persistence, privilege escalation, lateral movement, exfiltration —
  each hop anchored to concrete artefacts and mapped to MITRE ATT&CK.
- Support **micro-artefacts at any container level** — evidence file, partition,
  or indexed file — not only indexed files (the current hard limit).
- Support **multi-modal roots**: a root evidence file may be a disk image, a RAM
  image, a network capture, or a flat file.
- Provide an **extensible rule engine** with an extensive built-in rule set that
  builds chains such as cookie→website, cookie chaining, redirect chaining, and
  command-history→executable-run.
- Correlate **across evidence items** in one case (same implant / account / host
  across many machines) using the existing storage-level similarity engine.

### Non-goals (for this document)

- Live/EDR telemetry collection. We work post-hoc on stored evidence.
- Court-admissibility certification (tracked separately in the product plan).
- The GUI. This LLD defines the engine + data model + API; visualisation consumes
  the graph and is specified only at the interface level.

---

## 2. The layered model (the core idea)

The single most important design decision: **separate three kinds of graph node**.
Today the code conflates "a thing found in a file" with "a real-world entity,"
which is why micro-artefacts can only hang off one indexed file and can't be
shared or chained across files. We split them into three layers.

```
 LAYER A — CONTAINERS (where evidence physically lives)
   EvidenceFile ──contains──> Partition ──contains──> IndexedFile
   (disk/ram/pcap/flat)                                 (a file / record source)
        │                        │                          │
        └─────────── can each own OBSERVATIONS ─────────────┘
                                  │
                                  ▼
 LAYER B — OBSERVATIONS (raw micro-artefacts: a fact at a byte span)
   "cookie row @ off 4096 in Cookies.sqlite"
   "Run value @ path ...\Run\Updater in NTUSER.DAT"
   "URL string @ off 91233 in memory region"
        │  observed_as / evidences
        ▼
 LAYER C — ENTITIES (canonical, deduplicated real-world things)
   Process · ExecutableFile · RegistryKey · Account · Host · Domain ·
   URL · Cookie · NetworkEndpoint · ScheduledTask · Service · EmailMessage …
        │  causal / temporal / attack-path edges (built by rules)
        ▼
 ATTACK PATH = a scored, MITRE-tagged subgraph over LAYER C entities
```

- **Layer A — Containers.** The existing hierarchy (`EvidenceFile → Partition →
  IndexedFile`). We generalise it so observations may attach at *any* level (a
  disk image with no filesystem, or a partition-level artefact, still works).
- **Layer B — Observations (micro-artefacts).** A raw, cheap, span-anchored fact
  extracted from a container's bytes. **This is what "micro-artefact" means at the
  storage layer.** Every observation has exactly one container parent (preserving
  the current one-file-parent invariant) and a `raw_evidence_snippet`. Observations
  are never deduplicated — two files that both contain `evil.com` produce two
  observations.
- **Layer C — Entities.** The deduplicated real-world thing an observation refers
  to, keyed by a **content-derived canonical key** (§4). A `URL(evil.com/x)` entity
  is *one* node no matter how many observations across how many evidence items
  reference it. Entities carry the timeline (timestamps) and are what the rule
  engine chains.

This cleanly resolves four current constraints at once: artefacts at any level
(Layer B attaches anywhere), cross-file sharing (Layer C dedups), the
one-file-parent invariant (kept for Layer B), and idempotent cross-evidence
identity (Layer C keys on content, not UUIDs).

**"Observed_as / evidences" edge** links B→C: an observation *is evidence of* an
entity. One entity has many observation-evidences; one observation resolves to
(usually) one entity.

---

## 3. Current state & the constraints we must break

Grounded in the code (see [Appendix A](#17-appendix-a--current-code-map) for refs).

### What exists and works

- **Extraction pipeline** `lib/microartefact/service.go`:
  `content → scanWindow(cap 4 MiB) → parser.Parse → detectionEngine.Detect →
  dedupe → sort → relationEngine.Relate → repository.Store`.
- **Parsers** (`fileparser`/`parsing`): text, pdf, pe, elf, evtx.
- **Detectors** (`detectors/`): url, ioc (ip/email/domain/hash), key-value,
  log-line, windows-event-record, scheduled-task, service, browser, registry-hive
  (via `regparser`), evtx-native. Mostly **regex over the first 4 MiB of text**.
- **Relations** (`relation/`): two builders — `same-identifier`,
  `transaction-propagation` — both **intra-file**, deterministic.
- **Graph** (`repository/graphene`): node types evidence(0)/partition(1)/
  indexed(2)/`MicroArtefact`; edges `Contains` + generic `Relation`=EdgeType(100)
  differentiated by a `relation_type` string property. Property-indexed nodes,
  directional filtered traversal, `EdgeExists`, interactive HTML export.
- **Canonical model** `model/artefact_relation.go`: `CanonicalNode{ID,
  CanonicalKey, Timestamp, Origin, PayloadRef, Features, Confidence, Parser,
  RawEvidenceSnippet}` and `Relation{...}` — **defined but not wired into the
  writer** (the writer still persists the older `Artefact` struct).

### The seven constraints we must break

| # | Constraint (CURRENT) | Why it blocks attack-path | Where |
| --- | --- | --- | --- |
| C1 | Micro-artefacts attach **only** to indexed files; `EvidenceFileID`+`PartitionID` required, exactly-one-file-parent enforced | Need artefacts at evidence/partition level and shared entities | `repository.go` `validateStoreHierarchy`, `ensureMicroArtefactHasExactlyOneFileParent` |
| C2 | Detection is **regex over first 4 MiB of text** | Real artefacts (SQLite, $MFT, prefetch, pcap, memory) are structured & large | `service.go` `scanWindow`; `detectors/*` |
| C3 | Relations are **intra-file only**; a new graph node per artefact **occurrence** | Attack-path is cross-file / cross-evidence / cross-modality | `relation/engine.go`; `storeArtefactNodes` |
| C4 | **Canonical entities not wired**; no global "one entity per canonical_key" upsert | No entity to chain; no dedup across evidence | `model/artefact_relation.go` unused by writer |
| C5 | Object IDs are **UUIDv7, non-idempotent** across re-ingests | Cross-evidence identity must be content-derived, not ID-derived | `structs/inputfile.go` `NewInputFile` |
| C6 | `EvidenceType` = **file-format sniff only**; pipeline always assumes disk image | Multi-modal roots (RAM/pcap/flat) need branched ingestion | `util/filetype.go`; `store/store.go` |
| C7 | **Fragmented files are skipped** before the graph | Missing artefacts from fragmented evidence | `parser/common.go` `buildIdxMap` |

---

## 4. Identity strategy — content-anchored canonical keys

**Problem:** object IDs (evidence/partition/indexed) are UUIDv7, freshly minted
each ingest (C5). Graph node logical IDs are salted by those UUIDs, so they also
change across runs. Therefore **nothing in the current identity scheme is stable
across two ingests of the same evidence** — fatal for cross-evidence correlation.

**Solution:** entities (Layer C) are keyed by a deterministic **canonical key**
derived from content/semantics, never from object IDs. Observations (Layer B)
remain tied to their container by the existing (UUID-based) IDs — that's fine
because observations are per-evidence anyway.

### Canonical-key formula

```
canonical_key = entity_type ":" normalized_identity
node_id       = sha256(canonical_key)   // hex, stable across runs & evidence
```

Per-entity `normalized_identity` (proposals — iterate):

| Entity | normalized_identity | Notes |
| --- | --- | --- |
| ExecutableFile | `content_hash` (BLAKE3, base64) — fall back to normalized path if no hash | reuse `IndexedFile.FileHash` + `IdxFileHashLookup` |
| File (generic) | `content_hash`; alias paths become separate `PathName` sub-nodes | |
| RegistryKey | lowercase full key path (+ value name) | hive-relative, normalized |
| Process | `image_content_hash` + pid + create_time (when available) | from EVTX 4688 / memory |
| Account | `domain\username` lowercased, or SID | SID preferred when present |
| Host | hostname lowercased / machine SID | |
| Domain | registrable domain (public-suffix aware) | |
| URL | scheme+host+path+query, normalized (drop fragment) | |
| Cookie | `host ":" name` | value/expiry are properties, not identity |
| NetworkEndpoint | `ip ":" port ":" proto` | |
| ScheduledTask | task name + action command normalized | |
| Service | service name + ImagePath normalized | |
| EmailMessage | RFC Message-ID | |

**Rule:** prefer a cryptographic content hash whenever the entity *is* a byte
object (files, binaries, email bodies); prefer a normalized semantic string
otherwise (URLs, registry keys, accounts). Store the raw, un-normalized form as a
property for display and evidence.

**Entity upsert** = `graph.NodesByProperty("canonical_key", key)` → reuse-or-create
(mirror enrichment's `upsertNode`). This is the missing global dedup (C4).

---

## 5. Evidence-class taxonomy & multi-modal ingestion (Stage 0)

Today everything is parsed as a disk image (C6). Introduce a **semantic evidence
class** distinct from the format sniff.

```go
type EvidenceClass string
const (
    ClassDiskImage     EvidenceClass = "disk_image"   // dd/E01/raw/vmdk
    ClassMemoryImage   EvidenceClass = "memory_image" // raw/crashdump/lime
    ClassNetworkCapture EvidenceClass = "network_capture" // pcap/pcapng
    ClassFlatFile      EvidenceClass = "flat_file"    // a single file of interest
    ClassArchive       EvidenceClass = "archive"      // zip/tar container (future)
    ClassUnknown       EvidenceClass = "unknown"
)
```

- Add `EvidenceClass` to `EvidenceFile` (keep `EvidenceType` as the format sniff).
- **Classification**: extend `DetectEvidenceType` with a real classifier — E01/dd
  geometry & partition signatures → disk; LiME/crashdump headers or `--class`
  override → memory; pcap magic (`0xa1b2c3d4`/pcapng) → network; else if a known
  file magic → flat file. Allow explicit `dues store --evidence-class` override.

### Ingestion strategy per class

| Class | Ingestion (Stage 1) | Entities produced |
| --- | --- | --- |
| DiskImage | filesystem-parse provider (libtusk today → **in-house pure-Go FS libraries** later) → partition+file discovery; MBR/exFAT fallback | Files, and everything filesystem artefacts yield |
| MemoryImage | **new**: **in-house pure-Go memory-forensics library** (Volatility 3 feature parity, native — no subprocess, no Python) → processes, DLLs, handles, netscan, injected regions, command lines | Process, ExecutableFile (mapped), NetworkEndpoint, Account |
| NetworkCapture | **new**: pure-Go pcap reader → flows, DNS, HTTP, TLS SNI | NetworkEndpoint, Domain, URL, Host |
| FlatFile | treat the file as a single IndexedFile under a synthetic partition; run modality extraction directly | modality-dependent |
| Archive | expand entries as IndexedFiles (future) | file-dependent |

**Key point:** Stage 0 selects *which* Stage-1 extractors run, but Stages 2–6
(graph enrichment, entities, rules, paths) are **class-agnostic** — they operate
on the container/observation/entity model regardless of source. A RAM `Process`
and a disk `prefetch`-derived `Process` unify into the same entity type.

### 5.6 Provider abstraction — pure-Go parsers (strategic)

The filesystem and memory parsers are **swappable providers behind a stable
interface**, not hard dependencies of the pipeline. Two in-house replacements are
in progress and must drop in without touching Stages 2–6:

- **Filesystem:** libtusk (CGo Sleuth Kit wrapper) is the *current* provider; it
  will be replaced by **in-house pure-Go filesystem libraries**. Keep the existing
  `ParseImage`/`IndexFilesystem` seam (disk → partitions → indexed files) as the
  contract so the provider swap is invisible upstream.
- **Memory:** ingested via an **in-house pure-Go memory-forensics library** aiming
  at Volatility 3 feature parity — called **natively in-process**, emitting entities
  straight into the graph (no subprocess, no JSON marshalling boundary, no Python
  runtime).

**Why this matters (not just a dependency swap):**
- **Removes the CGo/libtusk toolchain constraints.** Today forensic parsing only
  builds with CGo on windows/amd64 + linux/amd64 (the `notusk` tag disables it
  elsewhere; see `TOOLCHAIN_NOTES.md`). Pure-Go means **one binary, all platforms,
  trivial cross-compilation** — which is itself a competitive differentiator (every
  incumbent is Windows-only; see `../prodocs/COMPETITIVE_ANALYSIS.md §4.3`).
- **No Python/Volatility runtime dependency** for memory analysis → the
  single-binary deployment story holds for RAM images too.
- **Tighter integration & control:** in-process extraction means entities and their
  provenance are emitted directly, extraction is cancellable/streamable, and the
  behaviour is ours to tune (important for the deterministic/auditable pipeline that
  admissibility later needs, LLD §15).

**Design rule:** define a `ParseProvider` (filesystem) and `MemoryProvider`
interface now; libtusk is one implementation. Stages 2–6 consume the provider
output (containers + a work-list of extraction tasks), never the provider directly.

---

## 6. The pipeline stages (the prongs)

The multi-pronged plan, as concrete stages. Each stage is independently runnable
and idempotent (re-runs converge, not duplicate).

```
Stage 0  Classify evidence → pick ingestion strategy                (§5)
Stage 1  Parse root→partition→indexed (all classes, incl fragmented) (C7 fix)
Stage 2  Enrich container graph with file/artefact properties        (extends enrichment)
Stage 3  Extract micro-artefacts (observations) at every level +
         canonicalise into entities                                  (Layers B+C)
Stage 4  Deterministic + probabilistic linking (cross-file/evidence) (rule engine pass 1-2)
Stage 5  Attack-path assembly over entity graph                      (rule engine pass 3)
Stage 6  Score, MITRE-map, export                                    (triage + viz)
```

### Stage 1 — Full parse from root, partition, and indexed

- Disk: filesystem-parse provider (libtusk today, pure-Go later — §5.6) + MBR/exFAT
  fallback. **Fix C7**: implement fragmented-file
  reassembly (lift fragment ranges to the partition level — the TODO already in
  `common.go`/`tusk.go`) so fragmented files become indexed files with multi-range
  `Fragments` rather than being dropped.
- Every container level can emit observations. A disk image with no parsable
  filesystem still yields **evidence-level** observations (carving, strings,
  embedded IOCs). A partition can yield **partition-level** observations
  (bootloader strings, unallocated-space carves).
- Output: the container graph (A) fully populated + a work-list of
  `(container, byte-range, modality)` extraction tasks.

### Stage 2 — Graph enrichment with artefact/file properties

- Extend the existing enrichment (`lib/enrichment`) so every container node carries
  the properties rules will need: `content_hash`, `size`, `mime/file_type`,
  `entropy`, `is_deleted`, `is_fragmented`, and **timestamps** (MACB from $MFT for
  disk files — new). Timestamps are first-class because the attack-path timeline
  depends on them.
- This stage is where "enrich graphdb with artefact info, properties" happens for
  **files-as-artefacts** (Layer A metadata), before micro-artefact extraction.

### Stage 3 — Micro-artefact extraction + canonicalisation

- Run the modality extractor for each work item (§7 defines the extraction
  contracts). Emit **observations** (Layer B) attached to their container.
- For each observation, run a **canonicaliser** that (a) computes the entity's
  `canonical_key`, (b) upserts the entity node (Layer C), (c) links observation
  →entity with `evidences`.
- Apply the **triage gate** (§7.4): observations below threshold stay as
  observations but are not promoted to entities (or are stored behind a debug
  flag), keeping the entity graph signal-rich.

### Stage 4 — Deterministic then probabilistic linking

- **Deterministic pass**: rules whose join is an exact anchor (canonical_key,
  content hash, path, pid, host, message-id). Confidence 1.0. Cross-file and
  cross-evidence (this is new — C3). Emits typed causal edges between entities.
- **Probabilistic pass**: temporal-window, similarity (SimHash), and fuzzy-entity
  rules. Confidence < 1, require corroboration before high confidence.

### Stage 5 — Attack-path assembly

- Over the entity+causal+temporal graph, run path assembly: seed from
  high-salience anchors (execution of an unsigned binary, a persistence entity, an
  external-download URL), expand along causal/temporal edges within time bounds,
  and emit **AttackPath** subgraphs. Each hop tagged with MITRE tactic/technique.

### Stage 6 — Score, MITRE-map, export

- Score paths (path confidence = f(edge confidences, corroboration, temporal
  tightness, salience)). Map to ATT&CK Navigator layer JSON. Export interactive
  HTML (extend existing `graphviz.ExportInteractiveHTML`) + machine-readable JSON +
  STIX (future).

---

## 7. What counts as a micro-artefact? (the hard problem)

This is the crux you flagged. The answer is a **per-modality extraction contract**
plus a **triage gate**, layered on the observation/entity split.

### 7.1 The principle

- An **observation** (Layer B) is the atomic unit a modality's contract defines.
  It is cheap and always recorded (subject to caps).
- An **entity** (Layer C) is created only when an observation has a stable
  canonical identity *and* passes triage.
- So "what counts as a micro-artefact" splits into two easier questions:
  1. **What is the atomic observation for this modality?** (deterministic per
     modality — the contract).
  2. **Which observations deserve to become entities in the attack-path graph?**
     (the triage gate).

### 7.2 Extraction contracts (the atomic unit per modality)

| Modality | Atomic observation | Entity(ies) promoted | Cardinality control |
| --- | --- | --- | --- |
| Shell/PowerShell history | **one per command line** | Process (image), maybe File/URL args | all lines (your example) |
| Browser cookies (SQLite) | **one per cookie row** | Cookie, Domain | all rows |
| Browser history (SQLite) | one per visit row + one per URL row | URL, Domain, Visit(event) | all rows |
| Browser downloads (SQLite) | one per download row | URL, File | all rows |
| Registry hive | one per value **under watchlisted keys** (Run/Services/Tasks/…) | RegistryKey, Service, Task, ExecutableFile | **watchlist-scoped**, not every value |
| EVTX / event log | one per event record | Process/Account/Service/Host (per event id) | all records (already) |
| Prefetch | one per (executable, run-count, run-times) | ExecutableFile, ExecutionEvent | all |
| Amcache/Shimcache | one per program entry | ExecutableFile, ExecutionEvent | all |
| $MFT | one per file record (MACB) | File (+ timestamps) | **lazy/scoped**: only for files referenced by other anchors, or on demand — millions of records otherwise |
| LNK / jumplists | one per shortcut/target | File, ExecutionEvent | all |
| PE/ELF binary | one per binary (metadata) + embedded IOC observations | ExecutableFile | one entity per binary |
| pcap | one per flow, one per DNS query, one per HTTP request | NetworkEndpoint, Domain, URL | all (bounded by capture) |
| Email (PST/MBOX/EML) | one per message + one per attachment | EmailMessage, File, Account | all |
| Memory (in-house pure-Go lib) | one per process / handle / connection | Process, ExecutableFile, NetworkEndpoint | all |
| Flat text / log | one per detector span (current regex behaviour) | IOC entities (URL/IP/Domain/Hash) | triage-gated |

**The disambiguation rule:** structured modalities (SQLite, EVTX, prefetch, MFT,
pcap, PE) have a **deterministic record boundary** → the atomic observation is one
record; no ambiguity. Unstructured modalities (arbitrary text/logs, memory
strings) fall back to **detector-span** observations and lean on triage. This is
exactly why the cmd-history case is "simple" (structured: one line = one record)
and a random blob is "confusing" (unstructured: only detector hits + triage).

### 7.3 High-cardinality control

$MFT and full registry hives can produce millions of observations. Policy:

- **Watchlist scoping** for registry (only persistence/execution keys by default).
- **Lazy/anchor-driven MFT**: don't materialise every MFT record as an entity;
  materialise a `File` entity when another anchor references its path/hash, and
  keep the full MFT as a queryable side-index for timeline lookups.
- **Global caps + `log()` of what was dropped** (never silently truncate).

### 7.4 The triage gate (promotion to entity / attack-path)

Adopt and extend the deep-dive's score (§ MICRO_ARTEFACT_DEEP_DIVE):

```
score = 0.35*temporal_relevance    // has a usable timestamp near other activity
      + 0.35*attribution           // ties to a process/account/host
      + 0.20*uniqueness            // rare value beats boilerplate
      + 0.10*extractability        // cleanly parsed, low ambiguity
NODE_THRESHOLD = 0.60   // promote to entity/attack-path if >= threshold
```

- Deterministic, well-identified observations (a Run key, a 4688 record) score
  high and always promote.
- Boilerplate/ambiguous detector hits score low and stay as observations only.
- Threshold and weights are config (`spec.Sheet`), tunable per rule-sheet.

---

## 8. Graph schema (nodes, edges, properties, IDs)

### 8.1 Node types

```
Containers (Layer A)   — reuse CustomNodeType(0/1/2) + extend
  evidence_file, partition, indexed_file
Observation (Layer B)  — NodeTypeMicroArtefact (existing), generalised parent
Entity (Layer C)       — NEW: one CustomNodeType per entity class, OR a single
                         "entity" type discriminated by `entity_type` property
Event (sub-kind of entity, time-stamped): ExecutionEvent, LogonEvent, VisitEvent…
AttackPath             — NEW: a path node grouping an ordered entity subgraph
```

Recommendation: a single `entity` node label + `entity_type` property (simpler
indexing, open taxonomy) rather than a fixed enum of `CustomNodeType`s. Validate
against the current graphene API during the re-baseline (§13).

### 8.2 Edge types

| Edge | From → To | Meaning | Deterministic? |
| --- | --- | --- | --- |
| `contains` | container → container/observation | hierarchy & ownership | n/a (existing) |
| `evidences` | observation → entity | this observation is evidence of this entity | yes |
| `same_entity` | entity ↔ entity | merge/alias (e.g. path-alias, hash match) | yes |
| `executed` | Process/Account → ExecutableFile | ran this binary | rule-dependent |
| `persists_via` | ExecutableFile → RegistryKey/Task/Service | persistence mechanism | yes (anchor match) |
| `connected_to` | Process/Host → NetworkEndpoint/URL/Domain | network activity | rule-dependent |
| `downloaded` | URL → File | download provenance | yes (download row) |
| `redirected_to` | URL → URL | referrer/redirect chain | yes/prob |
| `set_cookie` / `cookie_for` | Domain ↔ Cookie | cookie ownership | yes |
| `authenticated_as` | LogonEvent → Account | logon | yes |
| `moved_to` | Host → Host | lateral movement | probabilistic |
| `temporal_next` | event → event | time ordering within a window | probabilistic |
| `attack_step` | entity → entity (in an AttackPath) | a hop in a reconstructed path, MITRE-tagged | scored |

Edge properties (all typed relation edges): `relation_type`, `method` (rule id),
`deterministic_flag`, `confidence`, `evidence_fields[]`, `mitre_technique?`,
`observed_time?`. Extend edge-dedup to key on `(src,dst,relation_type,method)` not
just `(src,dst,type)` (current `EdgeExists` limitation).

### 8.3 Identity summary

- Container node ids: existing (UUID-salted) — fine, per-evidence.
- Observation node ids: per-occurrence (existing).
- **Entity node ids: `sha256(canonical_key)`** — stable across runs & evidence (§4).
- Edge ids: graphene-internal; logical dedup via the 4-tuple above.

---

## 9. The rule engine

### 9.1 Rule model

Extend today's `spec.RelationRule` / `relation.Builder` seam into a richer rule.

```go
type Rule struct {
    ID            string          // "web.redirect_chain"
    Name          string
    Tactic        string          // MITRE tactic
    Technique     string          // MITRE technique id, e.g. T1547.001
    Class         RuleClass       // Deterministic | Probabilistic | PathAssembly
    Enabled       bool
    Priority      int

    // Match: what entity/observation patterns and how they join
    Match         MatchSpec       // entity types, property predicates, anchor join key
    Temporal      *TemporalSpec   // optional window (e.g. within 5m / 24h)
    Corroboration int             // min independent signals for high confidence

    // Emit: the edge(s) produced
    Emit          EmitSpec        // edge type, direction, base confidence, evidence fields
}
```

- `MatchSpec` supports: entity-type filters, property predicates, and an **anchor
  join** (the field two entities must share — a canonical_key, hash, host, pid,
  referrer URL, …).
- `TemporalSpec`: window sizes reuse the deep-dive defaults (5 min for event
  bursts, 24 h for human-scale) plus per-rule overrides.
- Rules are data (a `Sheet`), hot-loadable, addable/removable (the existing
  `spec.AddRelationRule`/`RemoveRelationRule` mutation surface generalises).

### 9.2 Evaluation strategy

```
Pass 1 (deterministic):  for each Deterministic rule, index entities by anchor
                         key, join, emit edges (confidence 1.0). Order-independent.
Pass 2 (probabilistic):  temporal/similarity/fuzzy rules; require corroboration
                         before promoting confidence; tag derived edges.
Pass 3 (path assembly):  seed from salient anchors; BFS/DFS over causal+temporal
                         edges within time bounds; emit scored AttackPath subgraphs;
                         MITRE-tag each hop.
```

- Passes are re-runnable and idempotent (edge dedup on the 4-tuple).
- Anchor indexing = `graph.NodesByProperty` on the join key — the same primitive
  entity upsert uses, so it's already supported.

### 9.3 Explainability (mandatory)

Every emitted edge carries: the rule id, the anchor value that matched, the
`evidence_fields[]` (field name + parser + raw snippet) from **both** endpoints,
and deterministic/probabilistic tag + confidence. No edge without provenance.

### 9.4 Migration — replace the current hard-coded artefact-graphing with the rule engine

> This is a code-migration/refactor, not a greenfield build. The `microartefact`
> package currently constructs the artefact graph with **bespoke Go code**; the
> rule engine (§9.1–9.3) replaces that with **data-driven rules**. Capturing the
> migration here so the refactor is deliberate and parity-checked, not a rewrite.

#### 9.4.1 What is "coded in" today (CURRENT)

Graph/relation construction is imperative Go, in three coupled places:

- **`relation/builders.go`** — relation logic is hard-coded as `Builder`
  implementations (`SameIdentifierBuilder`, `TransactionPropagationBuilder`). **Each
  new relation type = a new Go struct + `Build()` method.** Logic (grouping,
  pairwise emit, txid regex) lives in Go, not in a rule.
- **`relation/engine.go`** — `Engine.Relate(file, artefacts)` runs builders keyed by
  `Method.BuilderName`, over **one file's `[]Artefact`** (intra-file, per-extraction
  batch).
- **`spec/`** — a `Sheet` of `RelationRule`s exists, but a `RelationRule` only
  *names a `BuilderName`* and toggles/decorates it (enabled, confidence,
  relation_type). The actual join logic is still in the Go builder. So it is
  **half-data-driven**: rules configure builders; they cannot express a *new* join
  without new Go code.
- **`repository/graphene/repository.go: storeRelationEdges`** — edge creation is
  hard-coded and matches endpoints by the **in-batch** map
  `relationNodeLookupKey(kind, value)`, so relations can only connect artefacts
  created in the *same* `Store` call.

Net effect: adding "cookie→website" or "cmd→exe" means writing a new Go builder;
logic is scattered and imperative; it is structurally intra-file, so it *cannot*
express the cross-file / cross-evidence joins attack-path needs.

#### 9.4.2 Target (rule engine)

- Rules are **data** (`Rule` with declarative `MatchSpec` anchor-join + `TemporalSpec`
  + `EmitSpec`, §9.1). **New relation = a new data row / rule file, not new Go code.**
- A **single generic evaluator** interprets any rule (anchor field, entity-type
  filters, predicates) — replacing the per-relation `Builder` structs.
- Evaluation runs over the **entity graph** (Layer C) via anchor-index joins
  (`graph.NodesByProperty`), not per-file `[]Artefact` — the cross-file/cross-evidence
  unlock.
- Three passes (deterministic / probabilistic / path), edges tagged with
  `producer_version` for retrospective re-derivation (§14.2).

#### 9.4.3 Migration steps (staged, parity-checked, non-breaking)

1. **Introduce the generic rule model & evaluator alongside the existing builders.**
   Extend `spec.RelationRule` → the declarative `Rule` (§9.1). Add a generic
   `Evaluate` path. Do **not** delete builders yet.
2. **Port the two existing builders to data rules.** Express `same-identifier`
   (anchor = normalized identifier value) and `transaction-propagation` (anchor =
   txid) as generic rules. Run **both old and new** over the same input and **diff
   the emitted edges** — require exact parity before proceeding.
3. **Decouple graphing from extraction (the structural change).** Today
   `service.Process` does extract→relate→store per file in one shot. Split into:
   (a) **extract + canonicalise** per file (Stage 3, emits observations + upserts
   entities), and (b) **rule evaluation** as a **separate corpus/case-level pass**
   (Stage 4) that runs *after* entities exist. Graphing stops being a side-effect of
   single-file extraction.
4. **Move edge emission to the generic emitter.** Replace `storeRelationEdges`'s
   in-batch `relationNodeLookupKey` matching with an `EmitSpec`-driven emitter that
   resolves endpoints by entity anchor index (works across files/evidence). Keep the
   graphene typed-edge *mechanics* (`edgeTypeRelation`, property packing, edge
   dedup) — only the matching changes.
5. **Add the three passes** and rule versioning; the current two relations become
   deterministic-pass rules.
6. **Delete the bespoke builders** once parity holds — but **keep the `Builder`
   interface as a narrow escape hatch** for a small number of "native" rules too
   complex to express declaratively (don't lose expressiveness).

#### 9.4.4 Keep / replace / delete

| Disposition | Item |
| --- | --- |
| **Keep** | `spec.Sheet` container + `Add/RemoveRelationRule` mutation surface; the rule-as-data idea; graphene typed-edge write mechanics; the **detection** engine (detection ≠ graphing — untouched by this migration) |
| **Replace** | `relation/builders.go` bespoke builders → generic evaluator + data rules; `Engine.Relate(file, artefacts)` (per-file) → `Engine.Evaluate(scope)` (graph-level); `storeRelationEdges` in-batch matching → anchor-index emit |
| **Add** | `MatchSpec`/`TemporalSpec`/`EmitSpec`; anchor index; 3-pass orchestration; `producer_version` tagging; (later) a rule file format / DSL for user-authored rules |
| **Delete (after parity)** | the two hard-coded `Builder` structs — retaining the interface only as an escape hatch |

#### 9.4.5 Risks

- **Parity**: generic rules must reproduce the current two relations *exactly* before
  builders are removed (step 2 diff gate).
- **Expressiveness**: keep the native-rule escape hatch so nothing becomes
  inexpressible.
- **Graph schema shift**: relations move from artefact↔artefact to entity↔entity;
  existing graphs need re-derivation (fine — that is exactly what §14.2 provides) or
  a one-time backfill.
- **Terminology**: fix vocabulary across the package — **detection** (find
  observations) vs **graphing** (rule-driven edge construction) vs **path assembly**
  (Pass 3). "Artefact graphing being coded in" = today's step-4 hard-coding; it
  becomes rule-driven.

### 9.4.6 Rule representation — JSON base + Mutant power tier

**Decision:** two authoring backends behind **one `Rule` abstraction** — not two
parallel systems.

- **JSON (built-in, canonical, dependency-free)** — the declarative base for the
  ~80% of rules that are *pattern + anchor-join + emit* (most of §10). Easy to
  author, version, diff, share, and sandbox; the natural rule-sheet format for
  retrospective re-derivation (§14.2). **Never grow it into a computed-expression
  language** — the moment a rule needs computation, it becomes a Mutant rule.
- **Mutant** ([github.com/aoiflux/mutant](https://github.com/aoiflux/mutant) —
  pure-Go, owner-controlled) — **the power tier.** Mutant is not a predicate DSL; it
  is a Turing-complete language that *already models our exact domain as builtins*:

  ```
  let db,_ = db_open();
  let p,_  = db_add_artifact(db, "process", {"pid":4021, "name":"powershell.exe"});
  let f,_  = db_add_artifact(db, "file", {"path":"C:/Users/Public/updater.ps1"});
  let _,_  = db_add_relation(db, p["node_id"], f["node_id"], "writes");
  let tl,_ = db_timeline(db);
  let b,_  = detect_network_beacon([...]);   // built-in multi-signal scoring
  ```

  Its `db_add_artifact` / `db_add_relation` / `db_timeline` / `db_bfs` /
  `db_shortest_path` / `detect_*` builtins are **near-isomorphic to our
  entity/relation/timeline/detection model**, so Mutant can own whole hard rule
  classes, not just predicates:
  - **Pass 3 attack-path assembly** (`db_bfs` / `db_shortest_path` / `db_timeline`).
  - **Probabilistic / fuzzy correlation** (`text_levenshtein`, arithmetic, `detect_*`).
  - Optionally, **custom detectors/parsers** (bytes-cursor + regex + PE/registry/
    email modules already shipped as Mutant examples) — a future authoring surface
    for §7 extraction; out of scope for the rule engine itself.

**Boundary (restated):**

| JSON rule (native evaluator) | Mutant rule (VM) |
| --- | --- |
| declarative match + anchor-join + emit | computation, graph traversal, fuzzy/scoring |
| closed predicate set (eq/in/gt/prefix/regex/hamming-within) | arbitrary logic, multi-hop, custom functions |
| bulk deterministic joins — fast native eval on the hot path | the expressive minority — VM cost fine; run **graph-level** (one `db_bfs`), not per-pair |

**Integration (two tiers; Mutant is pure-Go, compiles into the single binary):**

- **T1 — boundary integration (works today).** Mutant exposes
  `runner.Run(srcpath, password, secureMode, enforceSignerAuth)`. DUES writes the
  scoped entity subgraph to a fixture (Mutant has `json_parse` + filesystem
  builtins), runs a **signed** Mutant rule script that builds its artifact graph and
  does timeline/detection/traversal, then reads the emitted relations/paths back and
  materialises them into graphene. Coarse but real, and inherits Mutant's security
  (signing, encrypted bytecode).
- **T2 — in-process VM embedding (small owner-controlled addition to Mutant).** Add
  an `Eval(script, inputs) (object.Object, error)` + host-function registration over
  the existing `mutant/vm` + `mutant/object` + `mutant/builtin` packages, so DUES
  compiles a rule once, injects the entity subgraph as objects, and reads results
  in-process (no file boundary). Preferred once volume/perf demands it.

**Graph interop:** because Mutant's artifact/relation model ≈ our entity/edge model,
the mapping is near 1:1 — **mirror** the scoped entity subgraph into Mutant artifacts,
run the script's detection/timeline/traversal, read relations/paths back, upsert into
graphene (dedup on the 4-tuple).

**Product bonuses (Mutant-only):** signed official rule packs; **sandboxed
user-authored rules** (safe sharing); **encrypted-bytecode premium packs**
(protectable detection IP). Licensing is owner-resolved (dual-license / relicense /
private commercial fork) — not a blocker.

**Sequencing:** JSON lands in **AP-3**; the Mutant tier integrates at **AP-4/AP-5**
(probabilistic + path assembly), starting with T1, moving to T2 if needed.

### 9.5 Go interface sketch (target shape for the §9.4 migration)

> What `relation/` migrates toward: one `Rule` abstraction, one
> `Engine.Evaluate(scope)` over the **entity graph**, two pluggable evaluators
> (`json`, `mutant`). This replaces `Engine.Relate(file, artefacts)`.

```go
type Backend string
const ( BackendJSON Backend = "json"; BackendMutant Backend = "mutant" )

type RuleClass string
const ( Deterministic RuleClass = "det"; Probabilistic = "prob"; PathAssembly = "path" )

// Rule — the unit the engine evaluates, regardless of authoring backend.
type Rule struct {
    ID, Name        string
    Class           RuleClass
    Tactic, Technique string        // MITRE
    Enabled         bool
    Priority        int
    Version         string          // producer_version — for retrospective diffing (§14.2)
    Backend         Backend

    // JSON backend (declarative; nil for Mutant rules):
    Match    *MatchSpec
    Temporal *TemporalSpec
    Emit     *EmitSpec

    // Mutant backend:
    Script   string                 // path to signed .mu / inline source; compiled + cached
}

type MatchSpec struct {
    Left, Right EntitySelector       // entity type + predicates
    Anchor      AnchorJoin           // Left.Field <op> Right.Field
    Where       []Predicate          // extra flat predicates
}
type EntitySelector struct { Type string; Predicates []Predicate }
type AnchorJoin struct { LeftField, RightField string; Op PredicateOp; Threshold float64 }
type Predicate  struct { Field string; Op PredicateOp; Value any } // Eq/In/Gt/Prefix/Regex/HammingWithin…
type TemporalSpec struct { WithinSeconds int64; Order Order }       // 0 = no window
type EmitSpec  struct {
    EdgeType   string; Direction Direction; Confidence float32
    Mitre      string; Evidence  []string                          // field refs captured as evidence
}

// EvalScope — what a pass runs over (whole case, or an incremental anchor delta).
type EvalScope struct {
    Graph       GraphAccess          // entity graph read/write (graphene-backed)
    CaseID      string
    OnlyAnchors []string             // incremental re-eval; empty = full
}

type EmittedEdge struct {
    FromKey, ToKey  string           // entity canonical keys (§4)
    EdgeType        string
    Confidence      float32
    Deterministic   bool
    Mitre           string
    Evidence        []EvidenceField
    ProducerVersion string
}

// Evaluator — the backend interface. jsonEvaluator + mutantEvaluator implement it.
type Evaluator interface {
    Backend() Backend
    Evaluate(ctx context.Context, rule Rule, scope EvalScope) ([]EmittedEdge, error)
}

// Engine — replaces relation.Engine.Relate(file, artefacts).
type Engine struct {
    evaluators map[Backend]Evaluator // {json, mutant}
    rules      []Rule                // from spec.Sheet
}
func (e *Engine) Evaluate(ctx context.Context, scope EvalScope) error {
    for _, pass := range []RuleClass{Deterministic, Probabilistic, PathAssembly} {
        for _, r := range e.rulesFor(pass) {           // priority-ordered
            edges, err := e.evaluators[r.Backend].Evaluate(ctx, r, scope)
            if err != nil { return err }
            scope.Graph.UpsertEdges(edges)             // dedup on (src,dst,type,method)
        }
    }
    return nil
}
```

- `jsonEvaluator.Evaluate` interprets `MatchSpec` via anchor-index joins
  (`graph.NodesByProperty(anchorField, …)`) — the same primitive entity upsert uses.
- `mutantEvaluator.Evaluate` mirrors `scope` into Mutant artifacts, runs the rule
  script (T1 `runner.Run` now / T2 in-process later), and parses emitted
  relations/paths back into `[]EmittedEdge`.
- Migration parity gate (§9.4.3 step 2): port the two current builders to JSON rules,
  run old `Relate` and new `Evaluate` on the same input, diff `EmittedEdge`s, then
  delete the builders.

### 9.6 jsonEvaluator — how a declarative rule actually executes

The `jsonEvaluator` turns a `MatchSpec` into edges by an **anchor join** over the
entity graph. The engine builds anchor indexes **once per pass** (not per rule) so N
rules sharing an anchor field pay for one scan; incremental runs index only
`scope.OnlyAnchors`.

**Anchor kinds** (a `MatchSpec.Anchor.Kind`, so the declarative model covers most of
§10 without Mutant):

- **`field_eq`** — hash/equality join: `Left.Field == Right.Field` (cookie→website,
  run-key→exe by path, download→exe by hash).
- **`shared_neighbor`** — graph join: Left and Right both connect (via `ViaEdge`) to a
  common entity of `ViaType` (cookie-chain: two VisitEvents sharing one Cookie;
  redirect where both visits touch the same URL). No value comparison — a structural
  join.
- **`hamming_within`** — similarity join on a SimHash field (implant clustering).
  Not exact-indexable → LSH-bucketed; belongs to the **probabilistic** pass.

**Execution (field_eq shown; the common case):**

```go
func (je *jsonEvaluator) Evaluate(ctx, rule, scope) ([]EmittedEdge, error) {
    ms := rule.Match
    left  := scope.Graph.Entities(ms.Left.Type,  ms.Left.Predicates,  scope.OnlyAnchors)
    right := scope.Graph.Entities(ms.Right.Type, ms.Right.Predicates, scope.OnlyAnchors)

    var pairs [][2]Entity
    switch ms.Anchor.Kind {
    case FieldEq:
        idx := indexBy(right, ms.Anchor.RightField)          // map[normValue][]Entity  (transient, per pass)
        for _, l := range left {
            v := normalize(l.Get(ms.Anchor.LeftField))        // consistent normalization (lowercase host, etc.)
            for _, r := range idx[v] {
                if selfPair(l, r) { continue }                // skip l==r on self-joins
                pairs = append(pairs, [2]Entity{l, r})
            }
        }
    case SharedNeighbor:
        via := groupByNeighbour(left, right, ms.Anchor.ViaType, ms.Anchor.ViaEdge) // both linked to same via-entity
        pairs = via.crossPairs()
    case HammingWithin:
        pairs = lshSimilarityJoin(left, right, ms.Anchor.Field, ms.Anchor.Threshold)
    }

    var edges []EmittedEdge
    for _, p := range pairs {
        l, r := p[0], p[1]
        if !allPredicates(ms.Where, l, r)              { continue }   // extra flat predicates
        if rule.Temporal != nil && !within(l, r, rule.Temporal) { continue } // window + order
        from, to := orient(l, r, rule.Emit.Direction)
        edges = append(edges, EmittedEdge{
            FromKey: from.CanonicalKey, ToKey: to.CanonicalKey,
            EdgeType: rule.Emit.EdgeType, Confidence: rule.Emit.Confidence,
            Deterministic: rule.Class == Deterministic, Mitre: rule.Emit.Mitre,
            Evidence: collectEvidence(rule.Emit.Evidence, l, r, ms.Anchor), // field refs + raw values + anchor
            ProducerVersion: rule.Version,
        })
    }
    return edges, nil
}
```

**Indexing dependency (important):** for `field_eq` the anchor fields must be readable
per entity. Two mechanisms, in priority order: (1) if the canonicaliser persisted the
field as a graph index, use `graph.NodesByProperty(field, v)` directly; (2) otherwise
the engine builds a **transient per-pass anchor index** by scanning candidate
entities once. Either way, **rules declare which fields they anchor on**, and the
entity writer (Stage 3) should index the hot ones. `normalize()` must be identical on
both sides of a join and identical to how the canonical key was built (§4) — a
mismatch silently drops edges.

**Worked (cookie→website, `field_eq`):** `left=Cookie`, `right=Domain`,
`anchor = Cookie.host == Domain.name`; `idx` groups Domains by `name`; each Cookie
probes `idx[normalize(host)]`; emit `cookie_for` with evidence
`[Cookie.host, Domain.name]`, confidence 1.0, deterministic.

### 9.7 Mutant T1 fixture contract (boundary integration)

For T1, DUES and a Mutant rule script exchange **two JSON files in a sandboxed
per-run working dir**; DUES never passes args through `runner.Run`, so filenames are
conventional.

**Invocation:**
1. Engine selects the rule's scoped subgraph (bounded by the rule's entity types +
   temporal window) and writes `dues_rule_input.json`.
2. Engine runs the **signed** rule script: `runner.Run(scriptPath, "", /*secure*/ true,
   /*signerAuth*/ true)`. The script reads the input (Mutant `json_parse` + fs), mirrors
   entities into its `db_*` artifact graph, runs detection/timeline/traversal, and
   writes `dues_rule_output.json`.
3. Engine reads the output and upserts edges (dedup on the 4-tuple) / materialises
   paths as AttackPath subgraphs.

**Input — `dues_rule_input.json`:**

```json
{
  "schema_version": "1",
  "rule_id": "web.redirect_chain",
  "case_id": "case-123",
  "scope": {
    "entities": [
      { "key": "sha256:…", "type": "URL", "props": {"value":"http://a/","host":"a"},
        "first_seen": 1690000000, "last_seen": 1690000100 }
    ],
    "edges": [
      { "from":"sha256:obs…", "to":"sha256:url…", "type":"evidences", "props":{} }
    ],
    "observations": [
      { "id":"obs…", "container":"idx…", "kind":"visit_row", "span":[4096,4200],
        "snippet":"…", "props":{"url":"http://b/","referrer":"http://a/"} }
    ]
  }
}
```

Scope is **bounded**: only entities/edges/observations the rule's types touch, keeping
the fixture small.

**Output — `dues_rule_output.json`:**

```json
{
  "schema_version": "1",
  "rule_id": "web.redirect_chain",
  "producer_version": "rulesheet@0.4",
  "edges": [
    { "from_key":"sha256:urlA…", "to_key":"sha256:urlB…", "edge_type":"redirected_to",
      "confidence":0.9, "deterministic":true, "mitre":null,
      "evidence":[ {"field":"Visit.referrer","source":"chrome_history","raw":"http://a/"} ] }
  ],
  "paths": [
    { "id":"path-1", "score":0.82, "tactic_sequence":["initial-access","execution"],
      "steps":[ {"from_key":"…","to_key":"…","edge_type":"downloaded","mitre":"T1105"} ] }
  ],
  "stats": { "candidates":128, "emitted":12, "dropped":3 }
}
```

`edges` feed Passes 1–2; `paths` feed Pass 3 (materialised as AttackPath subgraphs).

**DUES-provided Mutant preamble** (ship a small signed `dues_rule_lib.mut`) so rule
authors write only logic, not I/O plumbing:

```
let scope = dues_read_scope();          // parse dues_rule_input.json → {entities, edges, observations}
// … build db artifacts, run detection/timeline/db_bfs, decide edges …
dues_emit_edge(from_key, to_key, "redirected_to", 0.9, true, evidence);
dues_emit_path(steps, score, tactics);
dues_flush();                           // write dues_rule_output.json
```

**Security & limits:** scripts run signed under `secureMode`; the working dir is a
controlled temp sandbox DUES owns; fixtures are the only I/O channel. The file
boundary means serialization cost + no streaming — fine for bounded scopes, and the
reason T2 (in-process `Eval`, §9.4.6) exists for large/hot rules.

---

## 10. Built-in rule catalogue

Grouped by capability. Each entry: `id` — join anchor → emitted edge (class).
This is the "extensive built-in rule set." It is meant to grow; treat as seed.

### 10.1 Web activity (your named cases)

- `web.cookie_for_site` — Cookie.host == Domain.name → `cookie_for` (det). *Cookie→website.*
- `web.cookie_chain` — same Cookie(host,name) observed across sessions/times →
  `temporal_next` between VisitEvents sharing the cookie (prob). *Cookie chaining.*
- `web.redirect_chain` — Visit.referrer_url == Visit.url of prior visit →
  `redirected_to` URL→URL (det when referrer recorded; prob on timing). *Redirect chaining.*
- `web.download_provenance` — Download.url → Download.target_file →
  `downloaded` URL→File (det).
- `web.download_to_execution` — downloaded File.hash == ExecutableFile.hash that
  has an ExecutionEvent → `executed` (det via hash). *Download→run.*
- `web.dns_to_connection` — DNSQuery.name resolves to NetworkEndpoint.ip →
  `connected_to` (det/prob).

### 10.2 Execution evidence

- `exec.cmdhistory_to_exe` — command line image name == ExecutableFile name/path →
  `executed` Account→ExecutableFile (det on path, prob on name-only). *cmd history→exe run.*
- `exec.prefetch` — Prefetch.exe == ExecutableFile → `ExecutionEvent` + run-times (det).
- `exec.amcache_shimcache` — cache entry → ExecutableFile → ExecutionEvent (det).
- `exec.evtx_4688` — 4688 NewProcessName → ExecutableFile; ParentProcessId →
  Process → `spawned` Process→Process (det).
- `exec.powershell_4104` — script-block log → command entity + IOC observations (det).

### 10.3 Persistence

- `persist.run_key` — Run/RunOnce value data path → ExecutableFile →
  `persists_via` (det). *(T1547.001)*
- `persist.service` — Service.ImagePath → ExecutableFile → `persists_via` (det). *(T1543.003)*
- `persist.scheduled_task` — Task action command → ExecutableFile →
  `persists_via` (det). *(T1053.005)*
- `persist.startup_folder` — startup LNK target → ExecutableFile (det). *(T1547.001)*
- `persist.wmi_subscription` — WMI consumer command → ExecutableFile (det). *(T1546.003)*

### 10.4 Credential / privilege / logon

- `auth.logon_4624` — 4624 → LogonEvent → `authenticated_as` Account (det).
- `auth.special_priv_4672` — 4672 → Account privilege escalation marker (det). *(T1078)*
- `cred.lsass_access` — handle to lsass (memory) → Process → cred-theft marker (prob). *(T1003.001)*

### 10.5 Lateral movement

- `lateral.remote_service_7045` — 7045 service install correlated across hosts →
  `moved_to` Host→Host (prob). *(T1021.002 / T1543.003)*
- `lateral.logon_type3` — network logon (type 3) Account across hosts → `moved_to` (prob). *(T1021)*
- `lateral.rdp_artifacts` — RDP cache/bmc + 4624 type 10 → `moved_to` (prob). *(T1021.001)*

### 10.6 Cross-evidence correlation (uses SimHash — §12)

- `corr.same_implant` — ExecutableFile.file_simhash within Hamming threshold of a
  known-bad or of another host's binary → `same_entity`/cluster edge (prob). *(variant detection)*
- `corr.same_account` — Account canonical_key across evidence items → `same_entity` (det).
- `corr.same_c2` — NetworkEndpoint/Domain across hosts → shared-infrastructure edge (det).

### 10.7 Path assembly (Pass 3)

- `path.killchain` — seed from external `downloaded`→`executed` or a persistence
  entity; expand along `executed`/`spawned`/`connected_to`/`persists_via`/
  `moved_to` within a temporal envelope; emit `attack_step` chain, MITRE-tag hops,
  score.

---

## 11. Worked examples

Tracing your named scenarios end-to-end through the stages/schema.

### 11.1 Cookie → website

1. Stage 0: disk image. Stage 1: `Cookies` SQLite discovered as an IndexedFile.
2. Stage 3 (contract: one observation per cookie row): observation
   `cookie_row{host=evil.com,name=sid,expiry,...} @ off N in Cookies.sqlite`.
   Canonicalise → `Cookie(evil.com:sid)` entity + `Domain(evil.com)` entity;
   `evidences` edges from the observation.
3. Stage 4 deterministic `web.cookie_for_site`: `Cookie.host == Domain.name` →
   `cookie_for` edge Cookie→Domain. Done: cookie is now linked to its website.

### 11.2 Cookie chaining

1. Two `VisitEvent`s (from history SQLite) both carry/refresh `Cookie(evil.com:sid)`
   at times t1 < t2.
2. Stage 4 probabilistic `web.cookie_chain`: same Cookie entity `evidences`ed by
   both visits within a session window → `temporal_next` VisitEvent(t1)→VisitEvent(t2),
   confidence scaled by temporal tightness. The cookie identity is the join anchor.

### 11.3 Redirect chaining

1. History rows yield `Visit{url=A}` and `Visit{url=B, referrer=A}`.
2. Canonicalise → `URL(A)`, `URL(B)` entities.
3. Stage 4 deterministic `web.redirect_chain`: `Visit.referrer == URL(A)` and
   `Visit.url == URL(B)` → `redirected_to` A→B (deterministic because the browser
   recorded the referrer). Chain of such edges = the redirect chain; Pass 3 can
   walk it as a path.

### 11.4 Command history → executable run

1. Stage 1: `ConsoleHost_history.txt` (PowerShell) / bash_history discovered.
2. Stage 3 (contract: one observation per line): observation
   `cmdline{"C:\Users\x\evil.exe -enc ..."} @ line K`. Canonicalise the image →
   `ExecutableFile(path=...\evil.exe)` (and, if the binary is also present on disk,
   dedup to the same entity by content hash).
3. Stage 2 gave us `ExecutableFile(evil.exe)` from the filesystem with a content
   hash; Stage 3's cmdline entity resolves to the **same** entity (path match, then
   hash match upgrade).
4. Stage 4 deterministic `exec.cmdhistory_to_exe`: `Account`(owner of history file)
   → `executed` → `ExecutableFile(evil.exe)`. If a prefetch/4688 corroborates, an
   `ExecutionEvent` with a real timestamp attaches, feeding the timeline and Pass 3.

Each example produces explainable edges (anchor value + raw snippets from both
endpoints) and is idempotent on re-run.

---

## 12. Cross-evidence correlation (reusing SimHash)

The storage-level similarity engine (`lib/near`, `lib/util`) is the unique asset
that makes cross-evidence attack-path possible. Reuse, don't rebuild:

- `util.ChunkSimHash64` / `fileSimHashAccumulator` (whole-file, alignment-independent,
  cached at `F|||:<fid>`) + `util.HammingSimilarity64` → cluster `ExecutableFile`
  entities across hosts even when the implant is repacked/renamed (`corr.same_implant`).
- `dbio.GuessFileTypes` (content-hash → object id) → resolve a hash anchor to its
  container across evidence.
- `countRList/countEviFile/countPartiFile/countIdxFile` already resolve a chunk
  match up to indexed→partition→evidence — the exact container mapping the
  `evidences`/attribution edges need.
- Because entities are keyed by content hash (§4), the **same binary/account/domain
  seen on machine A and machine B is one entity**, and cross-host edges
  (`moved_to`, `same_entity`, shared-C2) fall out of the join naturally.

This is the capability no incumbent forensic tool has: attack-path correlation
across an entire evidence corpus via a shared similarity index.

---

## 13. Cross-case analysis

Cross-evidence correlation (§12) works *within* one case. Cross-case analysis lifts
it a level: correlate entities, indicators, and attack paths **across separate
investigations**. For the target buyer (IR consultancies, corporate DFIR), the
highest-value question is often "have we seen this actor / tooling / infrastructure
before?" — and the content-anchored identity model (§4) makes it almost free,
because the same implant / domain / account produces the **same canonical key** in
every case.

### 13.1 Prerequisite: Case as a first-class scope

- Introduce a `Case` container above evidence: `Case ──contains──> EvidenceFile …`.
  (This is the case-management layer on the forensics backlog; cross-case depends
  on it.)
- Each case has an isolated case-scoped entity graph; evidence and entities are
  tagged with `case_id`.

### 13.2 The cross-case entity index

- A **global index** maps `canonical_key → { case_id → appearance }`, where an
  appearance records `{ first_seen, last_seen, roles[], entity_ref, evidence_count }`.
  Because entities key on content (§4), aggregation is a pure index write — **no raw
  case data is copied** into the index, only keys + metadata.
- Backed by a dedicated KV namespace (`XC|||:<canonical_key>` → appearances) or a
  small dedicated cross-case graph.
- Lookups: `appearances(canonical_key)` → cases; `shared_entities(caseA, caseB)` →
  the entities two cases have in common.

### 13.3 Capabilities

- **Seen-before**: for any entity, list other cases where its canonical key appears.
- **Campaign / actor clustering**: cluster cases by shared entities and shared TTP
  sequences (shared C2/domain, shared tooling via file-SimHash, shared account/SID,
  repeated attack-path shapes) → surface likely same-actor campaigns.
- **Playbook detection**: compare reconstructed attack paths across cases to find
  recurring technique sequences (an actor's repeated kill-chain).
- **Shared-infrastructure pivot**: from a NetworkEndpoint/Domain, jump to every case
  touching it.

### 13.4 Isolation, access control, privacy (non-negotiable)

Cases are frequently siloed for legal/contractual reasons. Cross-case must be
**permission-aware by construction**:

- Every cross-case result is filtered by the viewer's case ACLs. An analyst without
  access to Case B sees, at most, a **redacted count** ("also seen in 2 cases you
  cannot access") or nothing, per policy.
- **Sealed cases** can be excluded from the cross-case index entirely.
- For multi-tenant / MSSP / ISAC sharing, support **hashed-key matching** (match on
  `sha256(canonical_key)` so a shared index detects overlap without revealing the
  underlying value — a private-set-intersection-style option). Optional.
- All cross-case access is itself audit-logged (who pivoted into which case).

### 13.5 UX

- **"Seen before" badge** on any entity node/detail — a subtle chip showing the
  count of other *accessible* cases; click opens a panel listing those cases with
  first/last seen and the entity's role in each. Inaccessible cases appear only as a
  redacted count, never leaking content.
- **Cross-case pivot**: right-click an entity → "Find in other cases" → results
  grouped by case, each with a mini attack-path snippet for context.
- **Campaign view**: a case-similarity graph (cases = nodes, edges weighted by
  shared entities/TTPs); visible clusters = candidate campaigns. Filterable by
  shared-signal type (infrastructure vs tooling vs identity).
- **Path comparison**: pick two cases → side-by-side attack paths with shared hops
  highlighted, so repeated playbooks pop out visually.
- Principles: nothing surfaces that the viewer can't access; every cross-case link
  shows provenance and is one click from its supporting evidence.

---

## 14. Retrospective analysis

The layered model persists **raw evidence (immutable)** separately from the
**derived layers** (observations, entities, edges, paths). That separation makes
two retrospective modes possible — re-asking old evidence new questions.

### 14.1 Mode A — Retro-hunt (new knowledge vs old evidence)

When a new indicator arrives (a hash, domain, IP, YARA rule, a new sample to
SimHash, or a new detection rule), run it against the **entire historical corpus**
of already-ingested evidence across all accessible cases.

- **Fast because it hits indexes, not raw bytes**: exact indicators resolve via the
  existing property indexes (`artefact_value_hash`, entity `canonical_key`,
  `IdxFileHashLookup`); similarity indicators (SimHash of a new sample) scan the
  cached `F|||` file-simhashes with `HammingSimilarity64` — no re-ingest.
- **Output**: every case/evidence/artefact that matches, on a timeline that
  contrasts **when the underlying activity happened** vs **when the indicator became
  known** — the "compromised months before attribution" gap that makes
  retro-hunting so valuable.
- **Feeds & subscriptions**: an indicator / threat-intel feed can auto-trigger
  retro-hunts; a match on historical evidence raises an alert.

### 14.2 Mode B — Re-derivation (new engine vs old evidence)

When detectors, parsers, or rules improve, re-run Stages 3–6 over the persisted
lower layers **without re-ingesting raw evidence**, and present a **diff**.

- **Versioned derived layers**: every observation/entity/edge/path is tagged with
  `producer_version` (detector + rule-sheet + parser version) and a timestamp,
  making re-derivation reproducible, diffable, and reversible.
- **Diff, don't clobber**: re-derivation yields "+N new edges, +M attack paths, K
  confidence changes, R retractions" for review, not a silent overwrite. The analyst
  accepts/rejects; history is preserved.
- **As-of / time-travel**: reconstruct the graph "as known at rule-sheet version X /
  date Y" — essential for reporting and defensibility ("what did we know when?").

### 14.3 Retrospective across cases

Modes A and B compose with cross-case (§13): a new indicator can light up multiple
historical cases at once ("Domain X, now attributed to APT-Y, appears in Cases 4,
11, 23"), and a rule-engine upgrade can be re-derived corpus-wide.

### 14.4 UX

- **Retro-hunt workspace**: paste/upload an indicator (hash/domain/IP, drop a sample
  → auto-SimHash, or select a rule); pick scope (this case / my cases / all
  accessible / date range); run as a **background job** with progress. Results as a
  **timeline + case matrix**: which cases and artefacts matched and when, with the
  known-vs-occurred gap called out. One-click export and pivot to evidence.
- **Watchlists & retro-alerts inbox**: subscribe an indicator or feed; future
  ingests and new intel that match raise actionable notifications ("New match: hash
  … now seen in Case 12, host DB-01"). A triageable alert inbox.
- **Re-analyze & review-diff**: a "Re-analyze" action (per case or corpus-wide) runs
  re-derivation and opens a **diff view** — new/changed/retracted edges and paths,
  each labelled with the rule/version that produced it — that the analyst reviews
  before accepting. Non-destructive and versioned, so it's trustworthy.
- **Timeline scrubber / as-of view**: a control to view the graph as of a chosen
  analysis version or date.
- Principles: retrospective jobs never block interactive work (background + notify);
  every derived change carries provenance ("added by rule-sheet v0.4, 2026-08-01");
  changes are reviewable diffs, not silent mutations; results are exportable and
  audit-logged.

---

## 15. Hard problems & decisions to make

Open questions to resolve as we iterate (each is a real fork):

1. **Graphene re-baseline.** Graphene has changed since this code was written.
   Before building, audit the current `github.com/aoiflux/graphene` API: does it
   support the property-indexed upsert, typed edges, multi-parent nodes, and edge
   dedup we assume? Pin a version. *(Blocking for Stage 3+.)*
2. **Entity node modelling** — single `entity` label + `entity_type` property
   (flexible, recommended) vs one `CustomNodeType` per class (faster typed
   traversal). Decide against the real graphene API.
3. **Multi-parent entities vs one-file-parent invariant.** Observations keep one
   container parent; entities need many `evidences` in-edges. Confirm graphene
   allows this and drop `ensureMicroArtefactHasExactlyOneFileParent` for entities
   only (keep it for observations).
4. **The 4 MiB scan window (C2).** Fine for regex over text; wrong for SQLite,
   $MFT, memory, pcap. Structured extractors must stream/seek the full object via
   `dbio.StreamLogicalFileBytes`, not the capped `scanWindow`. Decide per-modality.
5. **MFT/registry cardinality.** Confirm the watchlist + lazy-MFT policy (§7.3);
   decide default watchlists and whether full-MFT timeline is a separate opt-in.
6. **Identity edge cases.** Path aliases, hard links, deleted/overwritten files,
   packed binaries (content hash differs but same logical malware → similarity
   edge, not `same_entity`). Nail the canonical-key table (§4) per entity.
7. **Timestamps & timezones.** MACB, EVTX, browser epochs, prefetch — normalise to
   UTC with source-tz metadata; timeline correctness depends on it.
8. **Fragmented files (C7).** Reassembly is a prerequisite for complete parsing;
   scope it in Stage 1.
9. **Determinism vs recall.** How aggressively to promote probabilistic edges;
   corroboration thresholds; false-path suppression in Pass 3.
10. **Performance & incrementality.** Rules must run incrementally as new evidence
    is added to a case (re-run passes only over affected anchors), and be
    benchmarked (regressions are release blockers per `BENCHMARKING.md`).
11. **Case isolation & ACL for cross-case (§13).** The cross-case index must never
    leak sealed-case content; decide the default posture (redacted counts vs
    hashed-key matching) and how ACLs are enforced at query time vs index time.
12. **Derived-layer versioning cost (§14).** Tagging every node/edge with
    `producer_version` and retaining diff history grows the graph; decide retention
    (keep last N analysis versions?) and whether re-derivation is full or
    anchor-incremental.
13. **Retro-hunt corpus scale.** Historical retro-hunts must stay index-driven
    (never a raw re-scan); confirm every indicator type has a backing index
    (exact → property index; similarity → cached `F|||` simhashes) before promising
    corpus-wide retro-hunt latency.

---

## 16. Phased build roadmap

Maps to the forensics pillar phases in
[../prodocs/PRODUCT_DEVELOPMENT_PLAN.md](../prodocs/PRODUCT_DEVELOPMENT_PLAN.md)
(F1 = attack-path v1, F2 = v2). Sequential, solo-friendly.

| Phase | Deliverable | Stages | Breaks |
| --- | --- | --- | --- |
| **AP-0** | Graphene re-baseline + entity-upsert primitive + canonical-key lib | schema, §4 | C4, C5 |
| **AP-1** | Layer split: observations at any container level + entity nodes wired; canonical model persisted; **decouple graphing from extraction** (§9.4.3 step 3) | 3 | C1, C4 |
| **AP-2** | Structured extractors v1: browser SQLite (cookies/history/downloads), registry watchlist, cmd/PS history, prefetch | 1, 3 | C2 |
| **AP-3** | **Rule-engine migration** — replace hard-coded `relation/builders.go` with the generic data-driven evaluator (§9.4, parity-gated) + web/execution/persistence rule packs | 4 (pass 1) | C3 |
| **AP-4** | Timestamps/MACB + super-timeline + temporal (probabilistic) rules | 2, 4 (pass 2) | — |
| **AP-5** | Path assembly + MITRE mapping + scoring + HTML/JSON export | 5, 6 | — |
| **AP-6** | Multi-modal roots: pcap then memory ingestion (memory via in-house pure-Go lib); introduce `ParseProvider`/`MemoryProvider` interfaces (§5.6) | 0, 1 | C6 |
| **AP-7** | Cross-evidence correlation pack (SimHash clustering, shared C2/account) | 4, 12 | — |
| **AP-8** | Fragmented-file reassembly; $MFT lazy timeline; email/pcap depth | 1 | C7 |
| **AP-9** | Case scope + cross-case entity index + seen-before/campaign clustering (ACL-aware) | 13 | — |
| **AP-10** | Retrospective: versioned derived layers, retro-hunt, re-derivation diff, retro-alerts inbox | 14 | — |

AP-0 and AP-1 are the foundation and unlock immediate value (structured artefacts
+ deterministic web/execution chains) before the full pathfinding of AP-5.

**Dependencies for the cross-case / retrospective phases:** AP-9 depends on the
case-management layer (a `Case` scope, backlog D in the product plan) and on
content-anchored identity (AP-0). AP-10 depends on `producer_version` tagging being
introduced in AP-1 (cheap to add early, expensive to retrofit) — so **stamp
`producer_version` on every derived node/edge from AP-1 onward even before
re-derivation is built**, so historical data is diffable when AP-10 lands.

---

## 17. Appendix A — current code map

Grounding references (file:symbol) for everything above.

- **Pipeline:** `lib/microartefact/service.go` — `Service.ExtractWithRelations`,
  `scanWindow` (4 MiB cap), `Process`.
- **Detection:** `lib/microartefact/detection/engine.go` — `DefaultMethods` (kind→detector map);
  `lib/microartefact/detectors/*` — `Default()` detector set; `detectors/common.go` regex bank.
- **Parsing:** `lib/microartefact/parsing/parser.go`, `.../fileparser/fileparser.go` — kinds text/pdf/pe/elf/evtx.
- **Relations (rule-engine migration target — §9.4):** `lib/microartefact/relation/engine.go`
  (`Engine.Relate` — per-file, to become graph-level `Evaluate`), `relation/builders.go`
  (`SameIdentifierBuilder`, `TransactionPropagationBuilder`, `DefaultMethods` — hard-coded
  builders to port to data rules).
- **Spec/rules:** `lib/microartefact/spec/{types,methods,mutations,defaults}.go` — `Sheet`,
  `DetectionRule`, `RelationRule`, `AddRelationRule`/`RemoveRelationRule`.
- **Graph write:** `lib/microartefact/repository/graphene/repository.go` — `Store`,
  `createArtefactNode`, `createContainsEdge`, `storeRelationEdges`,
  `ensureMicroArtefactHasExactlyOneFileParent`; node types 0/1/2 + `MicroArtefact`,
  `edgeTypeRelation=EdgeType(100)`.
- **Graph read/export:** `.../graphene/query.go` — `ReadHierarchy`; `repository.go` — `ExportInteractiveHTMLTopK`.
- **Canonical model (unwired):** `lib/microartefact/model/artefact_relation.go` —
  `CanonicalNode`, `Relation`, `EvidenceField`; `model/file_record.go` — `FileRecord`, `BuildIndexedFileID`.
- **Container enrichment:** `lib/enrichment/graphene_repository.go` — `UpsertEvidence/Partition/File`,
  `upsertNode` (the idempotent find-or-create to mirror), `ensureContains` (only `contains` today).
- **Data model:** `lib/structs/filestruct.go` — `EvidenceFile ⊃ PartitionFile ⊃ IndexedFile ⊃ baseFile`,
  `EvidenceType`, `InternalObjects`, `IngestState`; `structs/inputfile.go` — `NewInputFile` (UUIDv7).
- **Identity:** UUIDv7 object ids (`inputfile.go`), content hash `IndexedFile.FileHash` +
  `IdxFileHashLookupNamespace` reverse index (`parser/common.go`), `dbio.GuessFileTypes`.
- **Parser flow:** `lib/parser/parse.go` (`TuskAnalysis`, `ParseImage` → MBR/exFAT fallback),
  `parser/tusk.go` (`IndexFilesystem`, fragmented skip), `parser/common.go` (`buildIdxMap`,
  `finalizeIndexedFiles` → `EnrichPartition`).
- **Evidence type:** `lib/util/filetype.go` (`DetectEvidenceType`=format sniff), `lib/store/store.go`
  (`evidenceFilePreflight`, `maybeUpdateEvidenceType`).
- **Similarity:** `lib/near/{infile,outfile,near,simhash,verify}.go` — `NearInFile`, `NearOutFile`,
  `partialChonkMatch`, `verifyPhase2`; `lib/util/util.go` — `ChunkSimHash64`, `HammingSimilarity64`;
  `dbio` — `GetChonkSignature`, `Get/SetFileSimhash`, `StreamLogicalFileBytes`.
- **KV namespaces:** `lib/cnst/const.go` — `E||| P||| I||| R||| RA||| C||| S||| F||| EH||| PH|||`.

---

## 18. Appendix B — glossary

- **Container** — an evidence/partition/indexed node; where bytes live (Layer A).
- **Observation / micro-artefact** — a raw fact at a byte span in one container
  (Layer B). One container parent; never deduplicated.
- **Entity** — a canonical, deduplicated real-world thing keyed by content-derived
  `canonical_key` (Layer C). Shared across files and evidence.
- **Event** — a time-stamped entity (ExecutionEvent, LogonEvent, VisitEvent).
- **Anchor** — the field two entities/observations must share for a rule to join
  them (canonical_key, hash, host, pid, referrer…).
- **Canonical key** — `entity_type:normalized_identity`; its sha256 is the entity
  node id, stable across runs and evidence.
- **Rule** — a data-defined matcher that emits typed, explainable edges
  (deterministic / probabilistic / path-assembly).
- **Attack path** — a scored, MITRE-tagged subgraph over entities produced by Pass 3.
- **Triage gate** — the score threshold that decides which observations become
  entities in the attack-path graph.
