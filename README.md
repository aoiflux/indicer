# DUES - Deduplicated Unified Evidence Store

<p align="center">
  <img src="logo.svg" alt="DUES Logo" width="600"/>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.25-blue.svg)](https://golang.org)
[![Version](https://img.shields.io/badge/version-0.38-green.svg)](https://github.com/aoiflux/indicer)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

DUES is a digital forensics data platform for ingesting, deduplicating,
indexing, searching, restoring, and comparing large evidence sets.

Patent information: this software has led to two derivative inventions protected
by patents 567877 and 556272 registered at the Indian Patent Office.

## Feature Highlights

- Chunk-level deduplication with SHA3-256 integrity checks.
- Optional password-based encryption for stored chunks and metadata.
- Zstandard-backed compression.
- Partition-aware indexing for supported images and filesystems.
- Full-text search with report generation (`report.json`).
- Occurrence-aware ranking with configurable weighting (`--rank-alpha`).
- NeAR (near-duplicate analysis) for files inside (`near in`) and outside
  (`near out`) the DB.
- Graph generation for similarity analysis (`graph.html`).
- Container mode for blob packing (`--container`).
- Hierarchical block index mode (`--hierarchical`, auto-enables container mode).
- Rich interactive TUI command (`dues tui`) built on Bubble Tea v2.
- Connect/gRPC/gRPC-Web server mode (`dues server`) with CORS support.

## Installation

### Prerequisites

- Go 1.25+
- Windows, Linux, or macOS

### Build

```bash
git clone https://github.com/aoiflux/indicer.git
cd indicer
go build -o dues.exe .
```

## Quick Start

```powershell
# Store evidence
dues store E01-image.dd

# List stored entries
dues list

# Search indexed content/metadata
dues search "invoice"

# Search with multiple terms (AND by default)
dues search "invoice payment"

# Search with OR semantics
dues search "invoice|receipt"

# Increase the influence of raw occurrence counts in ranking
dues search --rank-alpha 0.8 "invoice payment"

# Try sidecar full-text mode first, fallback to scan search
dues search --fulltext "invoice|receipt"

# Restore by hash
dues restore <hash> --filepath restored.bin

# NeAR analysis from DB entry
dues near in <hash>

# Launch interactive TUI
dues tui
```

## Commands

### Core

- `dues store FILE`: Store one file or recursively store a directory.
- `dues list`: List completed evidence entries in the database.
- `dues restore HASH`: Restore a stored file by hash.
- `dues search QUERY`: Run search and emit `report.json`.
  - Supports AND queries (`foo bar`), OR queries (`foo|bar`), and quoted phrases
    (`"foo bar"`).
  - `--rank-alpha` tunes how strongly raw occurrence counts influence ranking
    while BM25 remains the primary relevance signal.
  - `--fulltext` attempts sidecar full-text retrieval first and automatically
    falls back to scan search when sidecar results are unavailable or empty.
- `dues near in HASH`: Find similar files for a stored object.
- `dues near out FILE`: Compare an external file against DB objects.
- `dues reset`: Delete database after confirmation.

### Extended

- `dues tui`: Launch Bubble Tea v2 based interactive terminal UI.
- `dues server`: Start Connect + gRPC + gRPC-Web API server on port `50051`.
- `dues version`: Show version details and capability highlights.

## Global Flags

| Flag             | Short | Description                                         | Default   |
| ---------------- | ----- | --------------------------------------------------- | --------- |
| `--dbpath`       | `-d`  | Database path                                       | `~/.dues` |
| `--password`     | `-p`  | Password for DB encryption                          | none      |
| `--chonksize`    | `-c`  | Chunk size in KB                                    | `256`     |
| `--low`          | `-l`  | Low-resource mode                                   | `false`   |
| `--quick`        | `-q`  | Throughput-first mode (less protection/compression) | `false`   |
| `--container`    | `-x`  | Container blob storage mode                         | `false`   |
| `--hierarchical` | `-i`  | Hierarchical block index mode                       | `false`   |

### search

| Flag           | Short | Description                                            | Default |
| -------------- | ----- | ------------------------------------------------------ | ------- |
| `--rank-alpha` | none  | Weight for occurrence-aware ranking influence (`>= 0`) | `0.35`  |
| `--fulltext`   | none  | Try sidecar full-text first, then fallback to scan     | `false` |

If `--hierarchical` is set without `--container`, DUES enables container mode
automatically.

## Memory and Cache Tuning

High RAM usage is often cache-driven and expected in throughput-first operation.
With large datasets (for example, ~100 GB on disk), seeing multi-GB process
memory can be normal when caches are intentionally kept warm.

Quick guidance:

- Use default mode for speed on high-memory hosts.
- Use `--low` on constrained hosts to reduce worker and cache pressure.
- During restore, container read cache improves repeated-read performance and is
  bounded.

Current cache policy summary:

- Badger cache budget targets `25%` of available RAM.
- Budget is bounded (`64 MB` minimum, `8 GB` maximum) with fallback (`256 MB`)
  if memory probing fails.
- Badger budget is split by role (block cache `75%`, index cache `25%`) to avoid
  double budgeting.
- Restore container cache uses the same baseline and is capped at `4 GB`.

For detailed rationale and implementation notes, see:

- [Container Mode LLD](CONTAINER_MODE_LLD.md)
- [Container Manager (Current Implementation)](CONTAINER_MANAGER_CURRENT.md)
- [Search LLD and Developer Guide](SEARCH_LLD.md)

## Command Flags

### store

| Flag         | Short | Description               | Default |
| ------------ | ----- | ------------------------- | ------- |
| `--sync`     | `-s`  | Run indexer synchronously | `false` |
| `--no-index` | `-n`  | Skip indexing             | `false` |

### restore

| Flag         | Short | Description         | Default    |
| ------------ | ----- | ------------------- | ---------- |
| `--filepath` | `-f`  | Output restore path | `restored` |

### near in

| Flag     | Short | Description                   | Default |
| -------- | ----- | ----------------------------- | ------- |
| `--deep` | `-e`  | Enable partial chunk matching | `false` |

## TUI (Bubble Tea v2)

DUES TUI now uses:

- `charm.land/bubbletea/v2`
- `charm.land/bubbles/v2`
- `charm.land/lipgloss/v2`

From the menu you can run Store, List, Search, Restore, NeAR, and Reset flows in
one interactive session. See:

- [TUI Quickstart](TUI_QUICKSTART.md)
- [TUI Implementation Notes](TUI_IMPLEMENTATION.md)

## Server Mode

`dues server` starts a service that supports Connect, gRPC, and gRPC-Web:

- Port: `50051`
- Health endpoint: `GET /`
- Service path: `/dues.DuesService/*`
- Upload staging folder: `<dbpath>/uploads`

## Storage and Indexing Notes

High-level model:

```text
Evidence File
  -> Partitions
    -> Indexed Files
      -> Chunk Relations
```

Namespaces used internally:

- `E|||:` evidence files
- `P|||:` partition files
- `I|||:` indexed files
- `C|||:` chunks
- `R|||:` chunk -> file relations
- `Я|||:` file -> chunk reverse relations

## Architecture Docs

- [Container Manager (Current Implementation)](CONTAINER_MANAGER_CURRENT.md)
- [Container Mode LLD](CONTAINER_MODE_LLD.md)
- [Hierarchical Block Index](HIERARCHICAL_INDEX.md)

## Output Artifacts

- `report.json`: Search output with matches and summary.
- `graph.html`: Relationship graph output from NeAR workflows.
- `BLOBS/*.blob`: Deduplicated chunk/container data.

## License

MIT. See [LICENSE](LICENSE).

## Contributing

Issues and pull requests are welcome.
