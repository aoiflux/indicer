# Benchmarking Guide

This document describes the initial benchmark harness for storage-path
performance tracking.

## What is included

- Service benchmark: `BenchmarkGetChonkMap` in `lib/service`
- Store benchmarks:
  - `BenchmarkIngestDataPath` in `lib/store`
  - `BenchmarkIngestMetadataPath` in `lib/store`
  - `BenchmarkIngestMetadataPathHotspots` in `lib/store`
  - `BenchmarkIngestBatchOwnership` in `lib/store`
  - `BenchmarkIngestBatchOwnershipSteadyState` in `lib/store`
  - `BenchmarkProcessRevRel` in `lib/store`
  - `BenchmarkInspectEvidenceRepairs` in `lib/store`
  - `BenchmarkRestoreDataPath` in `lib/store`
- Repeatable harness scripts:
  - `benchmark.ps1` (Windows/PowerShell)
  - `benchmark.sh` (Linux/macOS shell)

Outputs are written to `bench/outputs` by default.

Harness artifacts:

- per-benchmark raw outputs (`*.txt`),
- consolidated run transcript (`full_run.txt`),
- derived summary (`summary.txt`) including buffered-vs-unbuffered
  reverse-relation dual-write delta from the metadata hotspot benchmark.

## Quick Start

### Windows (PowerShell)

Run benchmark suite:

```powershell
./benchmark.ps1
```

Run with profiles enabled:

```powershell
./benchmark.ps1 -Profiles
```

Adjust run length/sample count:

```powershell
./benchmark.ps1 -Count 7 -Benchtime 3s
```

### Linux/macOS

Run benchmark suite:

```bash
sh ./benchmark.sh
```

Run with profiles enabled:

```bash
PROFILES=1 sh ./benchmark.sh
```

Adjust run length/sample count:

```bash
COUNT=7 BENCHTIME=3s sh ./benchmark.sh
```

## Manual benchmark commands

Service benchmark only:

```bash
go test ./lib/service -run '^$' -bench '^BenchmarkGetChonkMap$' -benchmem -count 5 -benchtime 2s
```

Store repair scan benchmark only:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkInspectEvidenceRepairs$' -benchmem -count 5 -benchtime 2s
```

Store reverse-relation write benchmark only:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkProcessRevRel$' -benchmem -count 5 -benchtime 2s
```

The reverse-relation benchmark currently includes:

- current append-primary noop path,
- current append-primary growing write path,
- direct append/shard write path,
- compatibility dual-write path,
- merged append+legacy member collection,
- append/shard prototype member collection.

Experimental runtime path:

- default runtime behavior persists reverse relations into append-primary shard
  keys and merges append + legacy map nodes on reads.
- `--revrel-append-prototype` enables compatibility dual-write so legacy map
  nodes are still emitted alongside append-primary keys for migration or
  rollback benchmarking.

Store ingest metadata-path benchmark only:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkIngestMetadataPath$' -benchmem -count 5 -benchtime 2s
```

This benchmark simulates multiple files ingesting the same chunk sequence and
compares:

- append-primary relation metadata writes,
- compatibility dual-write of append-primary + legacy map nodes.

Store batch-ownership benchmark only:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkIngestBatchOwnership$' -benchmem -count 5 -benchtime 2s
```

This benchmark compares shared-batch and batch-owner metadata write loops, but
includes per-iteration DB open/close overhead.

Store batch-ownership steady-state benchmark only:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkIngestBatchOwnershipSteadyState$' -benchmem -count 5 -benchtime 2s
```

This steady-state variant keeps one DB open per sub-benchmark and uses
iteration-offset file IDs to reduce DB-open noise and preserve non-trivial
relation/reverse-relation write pressure.

Store ingest metadata hotspot benchmark only:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkIngestMetadataPathHotspots$' -benchmem -count 5 -benchtime 2s
```

This benchmark breaks down the `files_32/chunks_16` metadata workload into:

- key construction only,
- relation-only writes,
- reverse-relation append-primary writes,
- reverse-relation compatibility dual-write path,
- reverse-relation compatibility dual-write unbuffered path,
- reverse-relation compatibility dual-write buffered path,
- append-member-only writes.

The harness summary parses the buffered/unbuffered pair and records:

- `unbuffered_ns_op`,
- `buffered_ns_op`,
- `buffered_improvement_percent`.

Store restore-path benchmark only:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkRestoreDataPath$' -benchmem -count 5 -benchtime 2s
```

## Profiling workflow

Example profile run:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkInspectEvidenceRepairs$' -benchmem -count 1 -benchtime 5s -cpuprofile bench/outputs/store_repair_scan.cpu.pprof -memprofile bench/outputs/store_repair_scan.mem.pprof
```

Restore-path profile run:

```bash
go test ./lib/store -run '^$' -bench '^BenchmarkRestoreDataPath$' -benchmem -count 1 -benchtime 5s -cpuprofile bench/outputs/store_restore_path.cpu.pprof -memprofile bench/outputs/store_restore_path.mem.pprof
```

Inspect CPU profile:

```bash
go tool pprof -top bench/outputs/store_repair_scan.cpu.pprof
```

Inspect memory profile:

```bash
go tool pprof -top bench/outputs/store_repair_scan.mem.pprof
```

## Baseline policy

- Keep benchmark commands stable while tuning code.
- Record machine type, OS, Go version, and command line with each run.
- Compare medians across multiple runs, not one-off results.
- Treat benchmark regressions as blockers for storage-path changes.

## Next benchmark additions

- Ingest-path benchmark for batch ownership variants.
- Reverse-relation benchmark extensions for larger fanout and mixed read/write
  workloads.
- Hierarchical block lookup benchmark for scan vs indexed lookup.
