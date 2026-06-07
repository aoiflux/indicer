# Store Staged Pipeline Plan

## Goal

Implement a high-throughput chunk ingest pipeline in Go with:

- immediate UUIDv7 assignment at ingestion
- asynchronous BLAKE3 hashing worker pool
- asynchronous dedup + batched disk writing

## Pipeline Shape

1. Ingestion stage

- Reads chunk slices from mapped input
- Assigns UUIDv7-derived chunk IDs immediately
- Pushes `Chunk` into bounded `hashCh`
- Does not hash or write to disk

2. Hashing stage

- Worker pool consumes `hashCh`
- Uses `github.com/zeebo/blake3`
- Reuses worker-local digest with `Reset()`
- Emits `HashedChunk` to `dedupCh`

3. Dedup stage

- Consumes `dedupCh`
- Uses sharded in-memory hash set for existence checks
- Enqueues unseen chunks to `writeCh`
- Does not write to disk

4. Disk writer stage

- Dedicated goroutine consumes `writeCh`
- Processes chunks in preallocated batches
- Materializes chunk metadata and relation/revrel updates
- Flushes one `badger.WriteBatch` at the end

## Data Structures

```go
type Chunk struct {
    ID   uint64
    Data []byte
}

type HashedChunk struct {
    ID   uint64
    Sum  [32]byte
    Data []byte
}
```

## Current vs New Architecture

Current (`batch-owner`) is already optimized and remains default:

- parallel hash workers
- sharded dedup gate
- single-owner WriteBatch
- container writer path already asynchronous internally

New (`staged`) is better for:

- stricter stage separation
- explicit channel backpressure
- easier per-stage tuning and instrumentation
- cleaner ownership boundaries for race safety

## Throughput Expectation

Will throughput increase?

- Likely yes in write-pressure scenarios where workers currently stall on
  materialization and I/O.
- Gains may be modest if dominant time remains in Badger/zstd flush/compression
  costs.
- Keep evidence-driven rollout: benchmark and compare both modes before default
  switch.

## Rollout and Validation

1. Keep `batch-owner` as default and gate new path with
   `--store-pipeline=staged`.
2. Run existing benchmark suites before/after with identical settings.
3. Compare medians and p95 latency for:

- metadata ingest path
- hotspot microbenchmarks
- end-to-end store flow

4. Keep staged mode opt-in until parity and throughput gates pass.

## Usage

- Existing mode (default):
  - `dues store --store-pipeline=batch-owner <FILE>`
- New staged mode:
  - `dues store --store-pipeline=staged <FILE>`
- Async file-mode writes (opt-in):
  - `dues store --store-async-file-write --store-pipeline=batch-owner <FILE>`
  - `dues store --store-async-file-write --store-pipeline=staged <FILE>`

Notes:

- Async file writes apply to non-container file-mode chunk materialization.
- The async file writer queue is drained before the store ingest path returns.
- Async file writes are experimental and remain default-off.
