# Benchmarking

This page lists the Go benchmarks for the storage path, the harness scripts
that run them, and how to compare two runs. For the test gate, see
[TESTING.md](TESTING.md).

## Benchmarks

| Benchmark | Package | Harness output | What it measures |
|---|---|---|---|
| `BenchmarkGetChonkMap` | `lib/server` | `service_getchonkmap` | The server's chunk map (`chunkMap`) for an evidence of 16, 64 or 256 chunks. `start_zero` reads the evidence record. `start_mid_chunk` reads a partition record that starts half-way into the first chunk. |
| `BenchmarkIngestDataPath` | `lib/store` | `store_ingest_path` | Read-only probes for each chunk: a chunk key lookup (once per distinct chunk), a relation key lookup and a reverse-relation read. It writes nothing. Workloads: `highly_unique`, `half_duplicate` and `mostly_duplicate`, each with 16 and 64 chunks. |
| `BenchmarkIngestMetadataPath` | `lib/store` | `store_ingest_metadata_path` | `processRel` and `processRevRel` for many files that share one chunk sequence, one write batch per file. Opens and closes a database in every iteration. Workloads: `append_primary/files_8/chunks_16`, `files_32/chunks_16` and `files_8/chunks_64`. |
| `BenchmarkIngestMetadataPathHotspots` | `lib/store` | `store_ingest_metadata_hotspots` | The `files_32/chunks_16` workload split into `key_construction_only`, `process_rel_only`, `process_revrel_append_primary` and `append_member_only`. |
| `BenchmarkProcessRevRel` | `lib/store` | `store_revrel_path` | Reverse-relation writes and reads for one chunk: `existing_same_file_noop`, `existing_new_file_write`, `append_collect_32_members`. The `synthetic_append_shard_*` cases use a test-only key layout (`revapp:`) for comparison. |
| `BenchmarkInspectEvidenceRepairs` | `lib/store` | `store_repair_scan` | `InspectEvidenceRepairs` over 500, 2,000 and 5,000 evidence records with some pending and some failed (`small_mostly_completed`, `medium_mixed`, `large_mixed`). |
| `BenchmarkRestoreDataPath` | `lib/store` | `store_restore_path` | A logical restore of 16 or 64 chunks to `io.Discard`. `start_zero` restores the evidence. `start_mid` restores a partition that starts half-way into the first chunk. |
| `BenchmarkIngestBatchOwnership` | `lib/store` | not run | The batch-owner writer's relation and reverse-relation writes (`batch_owner/...`), one batch per file, with no chunk bytes, no chunk-metadata barrier and no hocho root. Opens and closes a database in every iteration. |
| `BenchmarkIngestBatchOwnershipSteadyState` | `lib/store` | not run | The same workload with one database per sub-benchmark and new file IDs in each iteration. `batch_owner_via_metadata_helper/...` uses the metadata helper. `batch_owner/...` uses the direct write loop. |
| `BenchmarkIngestBatchOwnershipSimhashToggle` | `lib/store` | not run | The full batch-owner chunk pass (`ingestChunksBatchOwner`) with SimHash off (`simhash_off/...`) and on (`simhash_on/...`). |

"Not run" means the harness scripts do not run that benchmark. Run it by hand
(see [Running one benchmark](#running-one-benchmark)).

### Reverse relations are off in the benchmarks

`processRevRel` writes nothing unless `cnst.ENABLEREVREL` is true.
`dues store --revrel` sets it. No benchmark sets it, so:

- `process_revrel_append_primary` and the two `existing_*` cases of
  `BenchmarkProcessRevRel` time an empty call and an empty batch flush.
- The metadata-path and batch-ownership benchmarks time relation writes only.
- `append_member_only` and `append_collect_32_members` call `lib/dbio`
  directly, so they do write and read reverse-relation keys.

## Harness scripts

`benchmark.ps1` (PowerShell) and `benchmark.sh` (POSIX `sh`) run the seven
benchmarks that have a harness output name above. Each runs
`go test <package> -run '^$' -bench '^<name>$' -benchmem` from the repository
root.

```powershell
./benchmark.ps1
./benchmark.ps1 -Profiles
./benchmark.ps1 -Count 7 -Benchtime 3s
```

```sh
sh ./benchmark.sh
sh ./benchmark.sh --profiles
sh ./benchmark.sh --count 7 --benchtime 3s
```

### Options

| `benchmark.ps1` | `benchmark.sh` | Default | Meaning |
|---|---|---|---|
| `-Count` | `--count` | `5` | `go test -count` |
| `-Benchtime` | `--benchtime` | `2s` | `go test -benchtime` |
| `-Profiles` | `--profiles` | off | Also write a CPU and a memory profile for each benchmark |
| `-OutputDir` | `--output-dir` | `bench\outputs` / `bench/outputs` | Where the outputs go |
| `-StoreDataset` | `--store-dataset` | empty | Run the io_uring queue-depth sweep on this evidence file |
| `-StoreExe` | `--store-exe` | empty | The `dues` binary for the sweep |
| `-StoreCount` | `--store-count` | `3` | Sweep runs for each depth |
| `-IOUringQueueDepths` | `--io-uring-depths` | `256,512,1024,2048` / `"256 512 1024 2048"` | Queue depths to sweep, separated by commas or spaces. Each must be a whole number above 0; the list is checked before the benchmarks run |
| `-StoreDbRoot` | `--store-db-root` | `bench\store_matrix` / `bench/store_matrix` | Where the sweep's databases go |

### Outputs

The `bench/` directory is ignored by git.

| File | Content |
|---|---|
| `<name>.txt` | The output of one benchmark, for example `store_restore_path.txt` |
| `full_run.txt` | Every benchmark's output, plus the summary |
| `summary.txt` | The summary sections below |
| `<name>.cpu.pprof`, `<name>.mem.pprof` | Profiles, with `-Profiles` or `--profiles` |

`summary.txt` starts with a "Reverse-Relation Write Summary
(files_32/chunks_16)": the ns/op of the `process_revrel_append_primary` and
`append_member_only` sub-benchmarks of `BenchmarkIngestMetadataPathHotspots`,
or "Reverse-relation hotspot lines not found" when the output has neither.

### io_uring queue-depth sweep

With a dataset set, the harness also times `dues store` at each queue depth:

```
dues store --io-engine io-uring --io-uring-queue-depth <depth> -d <db> <dataset>
```

- Each run uses a fresh database under the sweep's database root.
- The summary gets one `depth=... median_seconds=...` line for each depth and a
  `recommended_depth=...` line.
- Without `-StoreExe` or `--store-exe`, the harness uses `./dues.exe` or
  `./dues` when it exists, and otherwise `go run .`.
- Every run passes `--no-config` when the binary takes it, so the account's
  config file (`~/.config/dues/dues.json`) cannot change a timing. The harness
  tries `<binary> --no-config version` once first. Releases from before the
  config file reject the flag and read no config file, so they run without it;
  the harness prints a note, and the summary's `config:` line says which.
- The duration comes from the
  `Stored as evidence <id> (<bytes> bytes, <format>) in <duration>` line, or
  the `Stored in: <n>s` line of older releases. A run that prints neither stops
  the sweep with "Could not parse store duration".
- `benchmark.sh` needs a regular file. `benchmark.ps1` accepts any existing
  path.

`compare_store_speed.ps1` times `dues store` for two binaries, round by round,
and prints each one's median. It reads the duration and treats `--no-config`
the same way.

Limits:

- io_uring exists only on Linux. Elsewhere `dues store` prints a warning and
  falls back to the `stdlib` engine, so a sweep on Windows or macOS times
  `stdlib` at every depth.

## Running one benchmark

```sh
go test ./lib/store -run '^$' -bench '^BenchmarkRestoreDataPath$' -benchmem -count 5 -benchtime 2s
go test ./lib/server -run '^$' -bench '^BenchmarkGetChonkMap$' -benchmem -count 5 -benchtime 2s
```

To check that a benchmark still runs, without timing it:

```sh
go test ./lib/store -run XXX -bench '^BenchmarkIngestBatchOwnership$' -benchtime 1x
```

## Profiling

```sh
go test ./lib/store -run '^$' -bench '^BenchmarkInspectEvidenceRepairs$' -benchmem -count 1 -benchtime 5s -cpuprofile bench/outputs/store_repair_scan.cpu.pprof -memprofile bench/outputs/store_repair_scan.mem.pprof
go tool pprof -top bench/outputs/store_repair_scan.cpu.pprof
go tool pprof -top bench/outputs/store_repair_scan.mem.pprof
```

## Comparing runs

- Keep the benchmark commands the same while you tune code.
- Record the machine, the OS, `go version` and the command line with each run.
- Compare with `benchstat` (`golang.org/x/perf/cmd/benchstat`) over several
  runs, not one.
- Run `benchstat` comparisons interleaved, round by round (old, new, old, new),
  on a quiet machine, so drift affects both sides.
- Treat a benchmark regression as a blocker for a storage-path change.

## Writing a benchmark

- Library code draws no progress by itself: it reports into the context
  (`internal/progress`), and only the CLI's `ui.Run` displays it. A benchmark
  that prints anything else must silence it with `defer console.Quiet()()`
  (`internal/console`). Otherwise the output pushes the result columns off the
  `Benchmark` line, and `benchstat` and the harness drop the result. Swapping
  `os.Stdout` does not work, because `internal/console` keeps the writers it
  captured at start.
- A new benchmark that the harness should run needs a `Run-Bench` line in
  `benchmark.ps1` and a `run_bench` line in `benchmark.sh`.

## Folder stores, measured by hand

`dues store <folder>` with `--no-config`, a fresh database each run, median
of 3, on a 16-CPU Windows 11 machine (2026-11-03). Baseline is `afcabc4`.

| Folder | Baseline | Now, one at a time | Now, `--store-jobs 4` |
|---|---|---|---|
| 200 FAT12 images of 1 MiB, distinct, `--fts --enrich` | 14.6 s | 6.7 s | |
| 3,000 random files of 1 to 64 KiB, 10% copies | 44.1 s | 17.7 s | 10.4 s |

- The first row is the ingest session: the full-text index and the graph
  open once per store, not once per evidence (and the graph no longer
  replays its log each time).
- One at a time, the second row is the feed ring: each chunk pass allocated
  up to 256 MiB of buffers when it started, about 32 MiB for every small
  file here; it now allocates no more than the file's chunks.
- Both stores gave the same stdout (IDs and times aside), the same 300
  aliases and the same `dues list` content.

With `--store-jobs` above 1, the `StoreFolder COMPLETE` log record says how
the workers spent the store: `busy_ms` and `held_ms` per worker,
`utilization` and `items_per_s` (STORE_LLD, "Parallel folder stores").

The corpora were generated by script; the second is `random.seed(42)`, sizes
uniform in 1 to 64 KiB, every tenth file a copy of an earlier one.

### One at a time against `--store-jobs 0`

`--store-jobs 0` stores `min(4, CPUs/4)` evidence at once, 4 here, and fewer
when memory is short (it was not). Measured 2026-11-05 at `b9eddac` on the
same machine (16 CPUs, 31.4 GiB of memory), with `--no-config --container`
and a fresh database each run. Peak working set is the store process's,
sampled every 250 ms.

| Folder | One at a time | `--store-jobs 0` | Peak working set, one / auto |
|---|---|---|---|
| 1,000 random files of 1 to 64 KiB and two VHDX images (10 and 20 GiB virtual), 30.0 GiB read; median of 3 | 35.5 s | 34.0 s | 1.84 / 1.84 GiB |
| A case folder: 410 files, 406 evidence (memory dumps, EWF images, archives, documents), 100 GiB read; two runs, one in each order | 867 s, 967 s | 974 s, 1,053 s | 2.32 / 2.36 GiB |

- Each pair stored the same bytes. The first folder read 30.0 GiB, found
  1.42 GiB new and wrote 1.06 GiB, a 1,097 MiB database. The second read
  100 GiB, found 70.3 GiB new and wrote 46.5 GiB, a 46.9 GiB database.
- The first folder gains 4%. Its two images are heavy (256 MiB or more
  decoded), so they run alone and take most of the time; the pool's
  `utilization` was 0.31.
- The second folder is 9 to 12% slower. Its 377 small evidence gain: they
  took 47 to 48 s of the wall time, against 74 to 78 s one at a time. Its 29
  heavy evidence run alone in both settings, yet took 926 and 1,006 s,
  against 791 and 887 s. The cause is not known. The folder and the
  database shared one USB SSD (no other disk had room), and two runs of
  one setting differed by up to 12%.
- The plan asked for no regression beyond 5% on these folders, so
  `--store-jobs` stays 1 by default. Measure again with the evidence and
  the database on different disks before changing that.
- File mode (no `--container`) writes one file per chunk. The first folder
  took 52.3 s one at a time and 46.6 s at `--store-jobs 0` (median of 3).
  On the USB SSD, the second folder had stored 71 GiB after 2 h 17 min,
  where container mode stores all of it in 15 to 18 minutes.
- Keep the machine awake for long runs. Idle standby dropped the USB SSD
  under one run, and badger, which writes its memtable through a mapped
  file, ended the process with an in-page error (`0xc0000006`).

The small files of the first folder were generated as the second corpus
above.

## Gaps

No benchmark covers:

- the staged pipeline (`dues store --pipeline staged`, `ingestChunksStaged`);
- reverse-relation writes with `cnst.ENABLEREVREL` on;
- reverse relations with more than 32 members, or mixed reads and writes.
