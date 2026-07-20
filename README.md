# DUES - Deduplicated Unified Evidence Store

<p align="center">
  <img src="logo.svg" alt="DUES Logo" width="600"/>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.25-blue.svg)](https://golang.org)
[![Version](https://img.shields.io/badge/version-0.39-green.svg)](https://github.com/aoiflux/indicer)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

DUES is a digital forensics data platform for ingesting, deduplicating,
indexing, searching, restoring, and comparing large evidence sets.

Version: 0.39 Codename: <pineapple> spacebar

## Important upgrade notice (breaking)

v0.38 introduces structural changes in on-disk DB and index layouts.

- Older DB directories may not be fully compatible with this release.
- Reusing older DB paths without migration or rebuild can cause read/startup
  failures.
- Back up existing DB paths before upgrading.
- After upgrade, run repair and reindex/restore workflows to rebuild state in
  the new format.

## Release summary

This release is a large cross-cutting update focused on reliability, retrieval
quality, and investigation speed.

- Storage pipeline improvements with better reliability and throughput tuning.
- Restore pipeline upgrades, including multithreaded restore controls.
- Search relevance improvements with BM25 and hybrid full-text fallback.
- New enrichment pipeline for evidence, partition, and indexed-file metadata.
- New micro-artefact extraction and graph workflows.
- Near-similarity and SimHash enhancements for in-db and out-of-db matching.
- Expanded server capabilities for browser upload sessions.
- Wider test and benchmark coverage across core subsystems.

Patent information: this software has led to two derivative inventions protected
by patents 567877 and 556272 registered at the Indian Patent Office.

## Feature highlights

### 1) Ingest, dedup, and storage

- Chunk-level deduplication with configurable hash algorithm.
- Default hashing is BLAKE3; SHA3 is also supported.
- Optional password-based encrypted DB operation.
- Per-chunk zstd compression with tunable compression level.
- Container mode for large-scale blob packing.
- Hierarchical block index mode for scalable lookup paths.
- Ingest validation and startup recovery for incomplete ingests.
- Repair tooling to inspect and optionally mark partial ingests as failed.

### 2) Image parsing with libtsk (via libtusk)

- DUES now uses libtusk (a C wrapper over The Sleuth Kit) for partition table
  and filesystem parsing on supported targets.
- This enables richer partition/filesystem-aware indexing behavior aligned with
  forensic image analysis use cases.
- Current CGO/libtusk-enabled targets: windows/amd64 and linux/amd64.
- On unsupported targets, DUES falls back to internal parsing (MBR/exFAT path).
- Fragmented-file handling remains limited in current parser flow; fragmented
  entries are reported and skipped by the libtusk indexing path.

See also:

- [TOOLCHAIN_NOTES.md](docs/TOOLCHAIN_NOTES.md)
- [clib/LIBTUSK_ABI.md](clib/LIBTUSK_ABI.md)

### 3) Search and ranking

- Query parsing with AND/OR/phrase search behaviors.
- BM25-driven ranking support.
- Occurrence-aware rank adjustment via rank-alpha.
- Hybrid mode: full-text sidecar first, automatic fallback to scan search.
- Search output generation for report workflows.

### 4) Near similarity

- Near in: compare objects already stored in DUES.
- Near out: compare external files against DUES content.
- SimHash-enabled workflows and top-K verification options.
- Optional deep and explain-exact behaviors for investigation workflows.

### 5) Enrichment and micro-artefacts

- Graph enrichment command to backfill hierarchy metadata.
- Micro-artefact extraction pipeline across indexed files.
- Detector and parser expansion for common forensic artefact patterns.
- Graph export support, including interactive HTML output.

### 6) API, server, and browser upload path

- Connect + gRPC + gRPC-Web server mode on port 50051.
- Localhost-aware CORS behavior for browser clients.
- Browser upload session endpoints:
  - POST /web/upload/start
  - POST /web/upload/chunk
  - POST /web/upload/finalize
- Health endpoint at GET /

### 7) TUI and operator workflow

- Bubble Tea v2 based TUI with integrated store/list/search/restore/near/reset
  flows.
- Improvements to command-line operational ergonomics and status visibility.

## Installation

### Prerequisites

- Go 1.25+
- Windows, Linux, or macOS

### Build

```bash
git clone https://github.com/aoiflux/indicer.git
cd indicer
go build -o dues .
```

Windows users may prefer:

```powershell
go build -o dues.exe .
```

Compression product specific builds:

Note: FFI build mode defaults to `auto`:

- Windows targets use `c-shared` (DLL)
- Non-Windows targets use `c-archive` (static `.a`)

Compression product build scripts compile with `-tags notusk`, which disables
libtusk/clib usage for compression artifacts on Windows and Linux.

```powershell
# Windows compression CLI artifact
./build-compression.ps1 -Mode cli -Targets windows/amd64

# Windows compression CLI + FFI c-archive artifacts
./build-compression.ps1 -Mode all -FfiBuildMode c-archive -Targets windows/amd64

# Windows compression FFI shared library (Flutter-friendly)
./build-compression.ps1 -Mode ffi -FfiBuildMode c-shared -Targets windows/amd64
```

```bash
# Linux compression CLI artifact
MODE=cli TARGETS=linux/amd64 ./build-compression.sh

# Linux compression CLI + FFI c-archive artifacts
MODE=all FFI_BUILD_MODE=c-archive TARGETS=linux/amd64 ./build-compression.sh

# Linux compression FFI shared library (Flutter-friendly)
MODE=ffi FFI_BUILD_MODE=c-shared TARGETS=linux/amd64 ./build-compression.sh
```

Product wrapper builds (routes to product-specific scripts):

```powershell
# Build compression CLI using root wrapper
./build-product.ps1 -Product compression -Mode cli -Targets windows/amd64

# Build compression FFI shared library using root wrapper
./build-product.ps1 -Product compression -Mode ffi -FfiBuildMode c-shared -Targets windows/amd64
```

```bash
# Build compression CLI using root wrapper
PRODUCT=compression MODE=cli TARGETS=linux/amd64 ./build-product.sh

# Build compression FFI shared library using root wrapper
PRODUCT=compression MODE=ffi FFI_BUILD_MODE=c-shared TARGETS=linux/amd64 ./build-product.sh
```

FFI ABI for Flutter/native callers:

- Build output includes a library and C header in `dist/compression`.
- Example Windows output:
  - `dues_engine-windows-amd64.dll`
  - `dues_engine-windows-amd64.h`
- Exported C functions from the generated header:
  - `char* DuesDispatchJSON(char* requestJSON)`
  - `void DuesFreeString(char* ptr)`

The caller must free response buffers returned by `DuesDispatchJSON` by calling
`DuesFreeString`.

## Quick start

```powershell
# Store evidence (file or folder)
dues store E01-image.dd --enrich --fts

# Store with convenience presets
dues store E01-image.dd --preset quick
dues store E01-image.dd --quick-mode
dues store E01-image.dd --performance-mode
dues store E01-image.dd --low-resource-mode

# List entries
dues list

# Search (scan ranking)
dues search "invoice payment"

# Hybrid full-text first, then fallback
dues search "invoice|receipt" --fts

# Restore by hash
dues restore <hash> --filepath restored.bin

# Near similarity from DB object
dues near in <hash> --advanced-deep --top-k 50

# Near similarity for external file
dues near out suspect.bin --explain-exact

# Backfill hierarchy enrichment in graphdb
dues enrich

# Extract micro-artefacts and export graph.html
dues micro extract --top-k 25

# Inspect/repair pending ingests
dues repair
dues repair --fix

# Launch TUI
dues tui
```

## Workflow recipes (copy/paste)

```powershell
# 1) Fast triage ingest (throughput-first)
dues store E01-image.dd --preset quick --fts

# 2) Balanced case ingest (good default)
dues store E01-image.dd --preset performance --enrich --fts

# 3) Low-resource ingest (laptop / constrained VM)
dues store E01-image.dd --preset low-resource --compress-level default

# 4) Search -> near -> restore investigation loop
dues search "invoice" --fts
dues near in <hash> --deep --advanced-deep --top-k 20
dues restore <hash> --filepath restored.bin

# 5) Tune heavy restore explicitly
dues restore <hash> -y 8 -U 32 -m 100 -b 128

# 6) Micro-artefact extraction workflow
dues micro extract -V -K 25
dues micro list
```

## Command summary

Core commands:

- dues store FILE
- dues list
- dues search QUERY
- dues restore HASH
- dues near in HASH
- dues near out FILE
- dues stats
- dues repair [--fix]
- dues enrich
- dues micro extract
- dues micro list
- dues server
- dues tui
- dues version
- dues reset

## Important flags

### Short-flag cheat sheet

Most-used shortcuts for day-to-day operations:

| Area    | Long form             | Short |
| ------- | --------------------- | ----- |
| Global  | --dbpath              | -d    |
| Global  | --password            | -p    |
| Global  | --chonksize           | -c    |
| Global  | --preset              | -P    |
| Global  | --quick-mode          | -Q    |
| Store   | --fts                 | -F    |
| Store   | --enrich              | -E    |
| Store   | --no-index            | -N    |
| Store   | --store-workers       | -w    |
| Store   | --store-queue         | -W    |
| Restore | --filepath            | -f    |
| Restore | --restore-workers     | -y    |
| Restore | --restore-queue       | -U    |
| Restore | --restore-progress-ms | -m    |
| Restore | --restore-buffer-mb   | -b    |
| Search  | --rank-alpha          | -r    |
| Near    | --deep                | -e    |
| Near    | --advanced-deep       | -a    |
| Near    | --top-k               | -k    |

Tip: run `dues help <command>` for command-specific short flags and examples.

### Global

- --dbpath, -d: custom database path
- --password, -p: password for encrypted DB access
- --chonksize, -c: chunk size in KB (default 256)
- --low, -l: low-resource mode
- --quick, -q: throughput-first mode
- --container, -x: container blob mode
- --hierarchical, -i: hierarchical index mode
- --compress-level, -z: fast, default, or best
- --preset, -P: apply convenience preset (quick, performance, low-resource)
- --quick-mode, -Q: convenience alias for --preset quick
- --performance-mode, -o: convenience alias for --preset performance
- --low-resource-mode, -L: convenience alias for --preset low-resource

### Store-specific highlights

- --sync, -s: synchronous indexing
- --no-index, -N: skip indexing
- --fts, -F: build sidecar full-text index
- --enrich, -E: upsert hierarchy metadata to graphdb
- --hash-algo, -g: sha3 or blake3
- --hash-strategy, -S: sync, async, or hocho
- --pipeline, -Y: batch-owner or staged
- --simhash, -H: compute chunk SimHash signatures during ingest
- --revrel, -R: persist reverse relations during ingest
- --store-workers, -w and --store-queue, -W: override ingest pipeline tuning

### Restore-specific highlights

- --filepath, -f: output path
- --restore-workers, -y and --restore-queue, -U: pipeline tuning
- --restore-progress-ms, -m: progress update interval
- --restore-buffer-mb, -b: write buffer sizing

### Search-specific highlights

- --rank-alpha, -r: occurrence boost weighting
- --fts, -F: hybrid full-text + fallback scan path

### Near-specific highlights

- --deep, -e: partial chunk matching
- --advanced-deep, -a: phase-2 full-file SimHash rerank
- --top-k, -k: top-K candidates for phase-2 rerank
- --explain-exact, -t: force chunk drilldown for exact out-file match

## CLI migration notes

- `store --no-index` short alias is now `-N`.
- `--enable-fts` and `--enable-enrichment` are now `--fts` and `--enrich`.
- `--topk` is now `--top-k`.
- Presets are available via `--preset` and convenience aliases (`--quick-mode`,
  `--performance-mode`, `--low-resource-mode`).

## Migration checklist (pre-0.38 to 0.38)

1. Back up current DB directory.
2. Upgrade binary to v0.38.
3. Run dues repair and review pending/failed evidence.
4. Re-run store/index paths where needed for format alignment.
5. Re-run enrichment and micro extract if graph metadata is required.
6. Validate search and restore outputs on representative samples.

## Architecture and design docs

- [STORE_LLD.md](docs/STORE_LLD.md)
- [STORE_BOTTLENECKS.md](docs/STORE_BOTTLENECKS.md)
- [SEARCH_LLD.md](docs/SEARCH_LLD.md)
- [ENRICHMENT_LLD.md](docs/ENRICHMENT_LLD.md)
- [MICRO_ARTEFACT_DEEP_DIVE.md](docs/MICRO_ARTEFACT_DEEP_DIVE.md)
- [NEAR_SIMILARITY_LLD.md](docs/NEAR_SIMILARITY_LLD.md)
- [HIERARCHICAL_INDEX.md](docs/HIERARCHICAL_INDEX.md)
- [CONTAINER_MODE_LLD.md](docs/CONTAINER_MODE_LLD.md)
- [CONTAINER_MANAGER_CURRENT.md](docs/CONTAINER_MANAGER_CURRENT.md)
- [RESTORE_LLD.md](docs/RESTORE_LLD.md)
- [BENCHMARKING.md](docs/BENCHMARKING.md)
- [TUI_QUICKSTART.md](docs/TUI_QUICKSTART.md)
- [TUI_IMPLEMENTATION.md](docs/TUI_IMPLEMENTATION.md)

## Output artefacts

- report.json: search report output
- graph.html: similarity and micro-artefact graph visualisations
- BLOBS/*.blob: deduplicated chunk/container data

## License

MIT. See [LICENSE](LICENSE).

## Contributing

Issues and pull requests are welcome.
