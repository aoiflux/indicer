# Store Pipeline - Bottleneck Status (May 2026)

This document tracks the original 8 store-path bottlenecks and their current
implementation status.

## Current Snapshot

- Status: 7 resolved, 1 partially resolved.
- Verification: `go test ./...` and `go test -race ./lib/store/...` passed.
- End-to-end compare: current branch median `1.8951s` vs main `2.5734s` (`1.36x`
  faster) using `compare_store_speed.ps1`.

---

## 1. sync.Map for cross-worker deduplication

- Original location: `lib/store/batch_owner.go`
- Status: Resolved
- What changed:
  - Replaced `sync.Map` with a custom sharded set (`shardedChunkSet`) using 256
    shards and per-shard `sync.RWMutex`.
  - Added allocation-free read probe via `unsafe.String` in lookup path.
- Code:
  - `lib/store/chunk_set.go`
  - `lib/store/batch_owner.go`

## 2. Two DB round-trips per new chunk in worker

- Original location: `lib/store/batch_owner.go` (`batchOwnerHashWorker`)
- Status: Partially resolved
- What changed:
  - File-backed chunk materialization now skips pre-probe and calls
    `MaterializeChonkNode` directly.
  - File writes were made idempotent (`fio.WriteChonk` checks file existence).
  - Container manager has in-process chunk dedup cache to avoid duplicate
    appends during a run.
- Remaining:
  - Container mode still performs `PingNode` pre-check for cross-run safety,
    because in-process dedup cannot detect previously ingested chunks from prior
    process lifetimes.

## 3. revRelAppendBuffer unnecessary mutex

- Original location: `lib/store/revrel_append_buffer.go`
- Status: Resolved
- What changed:
  - Removed `sync.Mutex` and lock/unlock overhead.
  - Buffer is now explicitly single-writer owned.

## 4. chash allocation on every revRelAppendBuffer.add

- Original location: `lib/store/revrel_append_buffer.go`
- Status: Resolved
- What changed:
  - Removed per-call chash copy.
  - `add` now stores provided slice directly under writer ownership contract.

## 5. simhash async ownership boundary

- Original location: `lib/store/simhash.go` (`enqueue`)
- Status: Resolved
- What changed:
  - Reintroduced copy only at async handoff boundary inside `enqueue`.
  - Worker hot path remains allocation-free when simhash is disabled.

## 6. Recovery scan double-read of orphan records

- Original location: `lib/store/recovery.go`
- Status: Resolved
- What changed:
  - Recovery scan now carries decoded `EvidenceFile` forward in memory.
  - Second pass no longer calls `GetEvidenceFile` for each orphan.

## 7. Flushed validation per-chunk transactions

- Original location: `lib/store/ingest_validation.go`
- Status: Resolved
- What changed:
  - Reworked validation into two `db.View` passes:
    1. collect relation chunk hashes, 2) verify chunk-node existence.
  - Eliminated per-chunk transaction open/close pattern.

## 8. string(chash) allocation in worker hot path

- Original location: `lib/store/batch_owner.go`
- Status: Resolved
- What changed:
  - Replaced with allocation-free lookup in `shardedChunkSet` using
    `unsafe.String` for probe path.
  - String allocation now occurs only on insert path.

---

## Remaining Follow-Up

1. Decide whether to fully remove container-mode `PingNode` by introducing a
   persistent chunk-existence index at the storage layer.
2. If that is adopted, simplify worker logic so all modes can use the same
   single-path idempotent materialization flow.

## Files Touched In This Implementation Wave

- `lib/store/revrel_append_buffer.go`
- `lib/store/simhash.go`
- `lib/store/recovery.go`
- `lib/store/chunk_set.go`
- `lib/store/batch_owner.go`
- `lib/store/ingest_validation.go`
- `lib/fio/fio.go`
- `lib/fio/container.go`
- `lib/store/ingest_benchmark_test.go`
