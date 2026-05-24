# UUID and Hash Design Decisions

Last updated: 2026-05-21 Status: Active working design (to be refined)

## Why This File Exists

This document is the single source of truth for ID and hash behavior while the
project transitions to UUID-first file identity.

We will keep updating this file as decisions evolve.

## Core Decisions

1. Evidence and partition records are UUIDv7 keyed.
2. Indexed records remain content-hash keyed for now.
3. File content hash and file identity are separate concepts.
4. Reverse hash indexes are used where primary keys are UUIDs.
5. Evidence keeps UUID identity in all hashing modes for structural commonality.

## Key Spaces and What They Mean

- E|||: Primary key space for evidence records. Key suffix is raw UUID bytes.
- P|||: Primary key space for partition records. Key suffix is raw UUID bytes.
- I|||: Primary key space for indexed records. Key suffix is raw content-hash
  bytes.
- EH|||: Reverse index mapping evidence content hash to evidence UUID.
- PH|||: Reverse index mapping partition content hash to partition UUID.
- IH|||: Reverse index mapping partition content hash to indexed UUID.

## Identity vs Content Hash

- File ID answers: "Which stored object is this?"
- File hash answers: "What is the file content fingerprint?"

Current model:

- Evidence: ID is UUIDv7, FileHash is base64(content hash)
- Partition: ID is UUIDv7, FileHash is base64(content hash)
- Indexed: ID is currently content hash key (I namespace), FileHash also stored
  in metadata

## Lookup Strategy

Given an external hash input, resolution prefers fast indexed lookups:

1. Direct UUID key probe when input is already UUID bytes.
2. Legacy namespace probes for backward compatibility.
3. Reverse hash index probes:
   - EH|||:<base64Hash> -> evidence UUID
   - PH|||:<base64Hash> -> partition UUID
4. Full-scan fallback only when no index-based hit exists.

## NeAR Exact-Match Behavior

Exact-match short-circuit now uses:

1. I|||:<rawHash> for indexed exact match.
2. PH|||:<base64Hash> -> UUID for partition exact match.
3. EH|||:<base64Hash> -> UUID for evidence exact match.

Rationale: partition/evidence are UUID keyed, so direct P|||:<hash> and
E|||:<hash> probes are not valid for new data.

## Ingest and Commit Semantics

1. Evidence UUID is assigned at initialization.
2. Evidence content hash is computed asynchronously.
3. Completion transition atomically writes:
   - updated evidence record with FileHash and completed ingest state
   - EH reverse index entry

This keeps restore-by-hash and recovery behavior consistent without making hash
generation a startup blocker.

## Parser and Enrichment Data Flow

- Parent evidence ID is no longer stored inside InputFile as hidden state.
- Parser/enrichment call chains receive evidence ID explicitly as an argument
  where needed.
- This makes linkage explicit and avoids confusing API semantics.

## Why Indexed Records Still Use Hash Keys

Keeping indexed files hash keyed currently provides:

1. Natural dedup identity across evidence and partitions.
2. Fast exact lookup without an extra reverse index.
3. Lower write amplification for common operations.

## Existing Chunk Hashing And Dedup

The codebase already hashes chunks and deduplicates them during ingest.

Relevant paths:

- `lib/util/util.go`: `GetLogicalFileHash(...)` and `GetChonkHash(...)`
- `lib/fio/container.go`: in-run chunk dedup via `seen` keyed by chunk hash
- `lib/fio/blockindex.go`: persisted chunk metadata is sorted and deduplicated

This means we already have chunk-level hashing and chunk-level dedup behavior.
What we do not yet have is a file-level Merkle root or hash-of-hashes scheme
that uses the chunk hashes themselves as the canonical file fingerprint.

That distinction matters:

- current chunk hashing supports dedup and lookup of chunks
- a Merkle-style root would be a new file identity strategy built from those
  chunk hashes
- hashing chunks alone does not automatically give us a file root hash

## Hashing Options (A-D)

This section is the short implementation view for the four options.

### A. `sync`

- What it is: full logical-file hash before commit.
- Implementation impact: low; mostly ingest flow ordering.
- Data model impact: none; uses current IDs and indexes.
- Operational tradeoff: predictable behavior, higher ingest latency.

### B. `async`

- What it is: hash in background while ingest/index runs, finalize at
  completion.
- Implementation impact: low-medium; requires state coordination and atomic
  completion write.
- Data model impact: none; keeps evidence UUID and EH reverse lookup.
- Operational tradeoff: lower wall-clock latency, more failure-path handling.

### C. `hash-of-chunks`

- What it is: file fingerprint from ordered logical chunk hashes.
- Implementation impact: medium-high; needs strict canonical ordering/framing.
- Data model impact: usually add as metadata first; do not replace primary keys
  initially.
- Operational tradeoff: can reuse chunk work, but boundary rules must be exact
  (especially partition/indexed start/end chunks).
- Current implementation note: exposed as `hash-strategy=hocho` for evidence
  flow, with async overlap during ingest/dedup.
- Logical-file note (current): partition/indexed attempt chunk-hash reuse only
  for chunk-aligned ranges; when reuse is unavailable or range boundaries are
  not aligned, hashing falls back to existing exact byte-range logical hashing.
- Observability note: hocho logical reuse counters are emitted per evidence
  indexing pass (`attempts`, `reused`, `fallback_unaligned`,
  `fallback_missing_relation`, `errors`) to guide whether wider rollout is
  beneficial.

### D. `merkle-tree-root`

- What it is: Merkle root built from logical chunk-hash leaves.
- Implementation impact: high; tree construction/versioning/proof conventions.
- Data model impact: add root field (and optional reverse index if queried by
  root).
- Operational tradeoff: strongest verification model, highest implementation and
  test complexity.

## Quick Comparison

| Option                | Complexity  | Runtime effect                    | Migration risk | Current posture                          |
| --------------------- | ----------- | --------------------------------- | -------------- | ---------------------------------------- |
| A. `synchash`         | Low         | Higher ingest latency             | Low            | Keep as baseline fallback                |
| B. `asynchash`        | Low-Medium  | Best latency profile for evidence | Low-Medium     | Keep as primary for evidence             |
| C. `hash-of-chunks`   | Medium-High | Potential reuse wins              | Medium         | Prototype behind feature flag            |
| D. `merkle-tree-root` | High        | Similar to C plus tree overhead   | High           | Defer until proof/audit need is explicit |

## Key Mutation Reality

The temporary UUID idea only works cleanly if the UUID remains the canonical
key.

In Badger, keys are not mutated in place. If we want to turn a temporary UUID
into a final hash key later, that is not a rename; it is a rekey operation:

1. Write the value under the new key.
2. Delete the old key.
3. Update every reference that pointed at the old key.

That is why UUID-first with later hash availability is cheap only when the UUID
stays the stable identity and the hash is stored as metadata or a reverse index.

If the hash must become the real primary key later, we need an explicit
migration/rekey plan rather than a simple field update.

## Relationship Modeling Options

Historically we encoded relationships by embedding evidence/partition identity
into constructed names like `evi|||parti` and `evi|||parti|||idx`.

That works, but it mixes identity into display names and makes lookup rules
harder to reason about.

Possible directions:

1. Keep the current name format only as a human-readable label.
2. Store parent UUIDs explicitly as fields or relation edges.
3. Use reverse indexes for hash lookup, but keep parent-child links UUID-based.

The current codebase is already moving in that direction for evidence and
partition linkage.

## If We Move Indexed Records to UUID Later

If indexed records migrate to UUID primary keys, we should add:

1. IH reverse index (hash -> UUID).
2. Clear dedup policy (single canonical UUID per content hash or multi-record
   strategy).
3. Migration/backfill plan for existing I namespace keys.

## Invariants to Preserve

1. UUID is the canonical object identity for evidence and partition records.
2. Content hash remains queryable in O(1) for all object types.
3. No silent fallback to full-scan when an index path exists.
4. API names must reflect semantics (ID getters return IDs, hash getters return
   content hashes).
5. Any hash-of-chunks mode must include deterministic chunk ordering and
   boundary-trim rules.

## Implementation Decision (Current)

1. Keep B (`asynchash`) as default for evidence.
2. Keep current behavior for partition/indexed until C is prototyped.
3. Prototype C (`hash-of-chunks`) as `hash-strategy=hocho` for evidence; keep it
   additive (no primary-key migration).
4. Revisit D (`merkle-tree-root`) only when proof-grade verification is a
   concrete requirement.

## Open Questions

1. Should indexed records stay hash-keyed long-term or move to UUID + IH?
2. Do we want unique constraints or conflict handling around EH/PH reverse index
   writes?
3. Should we add background consistency checks for stale EH/PH entries?
4. Should we formalize this into ADR files once design stabilizes?

## Update Rule

When we change behavior, update this file in the same PR.
