# Testing DUES

This page explains how DUES is tested: the verify gate that every change must
pass, the fixtures and corpus the tests run against, the failure suites, and
how to add a test for a new bug.

## The verify gate

Run the whole gate from the repository root:

```powershell
scripts\verify.ps1            # everything, including the WSL part
scripts\verify.ps1 -Long      # also the long tests, on Windows and in WSL (about 20 minutes more)
scripts\verify.ps1 -Fast      # format, vet, builds and tests only
```

`-NoWSL`, `-NoVuln` (govulncheck needs network access) and `-NoFuzz` skip
those parts. `-FuzzTime 10s` shortens each fuzz target's run.

On Linux, or inside WSL, run `scripts/verify.sh` instead. It accepts `--fast`,
`--no-vuln`, `--no-race`, `--no-fuzz` and `--fuzztime DURATION`.

The scripts and tests read no environment variables. Test settings are build
tags and `go test` flags:

| Setting | How | Effect |
|---|---|---|
| Long tests | `-tags long` | Run the scale and soak tests (see Long tests) |
| Pseudo-terminal tests must run | `-tags requirepty` | Their skip becomes a failure (the WSL suite) |
| A Windows console is attached | `-tags winconsole` | Run the Ctrl-Break test in `internal/faultinject` |
| Strict panics | `-tags safestrict` | A panic `internal/safe` would contain crashes the test (the fuzz smoke) |
| Special mounts | `-args -dues.enospc-dir=DIR` and the rest | Run the filesystem-failure tests (see below) |
| The real corpus | `-args -dues.corpus-vhd=DIR` and the rest | Run the corpus tests (see below) |

A test binary refuses a flag it does not define, so a run that passes
`-args` names its packages rather than `./...`.

The gate runs these steps and prints one PASS or FAIL line per step, then a
summary:

| Step | What it checks |
|---|---|
| format | `go run ./internal/tools/fmtcheck`. Plain `gofmt -l` flags every file on a CRLF checkout, so fmtcheck normalises line endings before comparing. |
| vet | `go vet ./...` without tags, with `-tags faultinject`, and with every test tag (`faultinject,long,requirepty,safestrict,winconsole`) |
| cross build | Release builds for windows, linux and darwin on amd64 and arm64, for `.` and `./cmd/compression`. A binary built with the `faultinject` tag fails the step. |
| staticcheck | Without and with `-tags faultinject`, under the toolchain pinned in `scripts/tools.env` (older staticcheck cannot read newer export data). The target is 0 findings. The faultinject run is skipped when the plain run fails, so after a fix run both by hand. |
| govulncheck | Source mode, plus binary mode on the Linux build. The target is 0 reachable vulnerabilities. |
| test | `go test ./...`, once without and once with `-tags faultinject`; with `-Long`, the first run uses `-tags long` |
| WSL suite | `scripts/wsl/sync.sh` copies the working tree to ext4 with LF line endings. `scripts/wsl/suite.sh` then runs the race tests, the fault-injection tests, the filesystem-failure tests, the fuzz smoke and, with `--long`, the long tests. |

Tool versions live only in `scripts/tools.env`. Fuzz targets are listed in
`scripts/fuzz-targets.txt`, and each runs for `--fuzztime` (default 60s).
Each new-coverage input or failing input is minimised for at most 5s
(`-fuzzminimizetime 5s`). Go's own default is 60s, which in a 60s smoke
kept every worker minimising, so the run reported 0 execs/sec and explored
almost nothing.

Practical notes:
- Do not edit the tree while the Windows steps run. Editing is safe once the
  log shows `== WSL sync`.
- Do not run the real-image corpus tools while the WSL fuzz smoke runs. Together
  they have exhausted the Windows paging file (error 1455).
- WSL drops its mounts when the VM goes idle, and loses them when it restarts.
  `suite.sh` mounts what it needs in the same session as the tests.

## Known defects in tests

A test that describes correct behaviour DUES cannot deliver skips with a
message naming the bug inventory ID:

```
--- SKIP: .../deleted:/old/tobedeleted.bin  known defect PAR-6 (...)
```

A test that passed before and now fails is a regression, never a skip. Two
kinds of skip remain, both documented as residual in
`docs/CORRECTNESS_REPORT.md`:
- `lib/parser/scan`: ext4 zeroes a deleted inode's size and extent tree, so a
  deleted ext4 file can be recovered only from the journal (PAR-6).
- `internal/testutil/legacyfixture`: data the release before the correctness
  pass wrote wrongly cannot be read back correctly: the second session of each
  container fixture (STO-1), and the E01 fixture's index (ING-7).

To list them:

```
go test ./... -v 2>&1 | grep -o "known defect [A-Z]*-[0-9]*" | sort | uniq -c
```

## Fixtures

### Filesystem images

`lib/parser/testdata/fixtures/<group>/` holds small disk images (zstd
compressed, about 3 MB in total), each with a `*.manifest.json` ground truth.
For every file on the volume, the manifest records:
- path, kind, size and content SHA-256;
- the properties the parser should report: fragmented, sparse, unwritten, inline, compressed, alternate streams, hard links;
- for deleted files, whether they are recoverable.

The manifests deliberately contain no offsets. Finding the data is the parser's
job; the test checks only that what it finds hashes to the true content.

The groups:
- FAT12, FAT16 and FAT32 (made with mkfs.vfat and the kernel driver, and also by fsagen);
- exFAT;
- ext4, with inline data, preallocated and unwritten extents, holes, symlinks and hard links;
- XFS, with four allocation groups, tail slack, holes, unwritten extents and many extents;
- NTFS from fsagen, with resident data, fragmentation, alternate data streams, hard links and deletions;
- NTFS from ntfs-3g, with compressed, incompressible, mixed, resident and sparse files (`ntfs_compressed`);
- classic HFS, and an empty HFS+ volume;
- GPT and MBR disks with several partitions;
- a three-segment E01;
- fixed VHD, dynamic VHD and VHDX.

Every image is written by third-party tools, never by DUES. A fixture built
with DUES's own reading of a format could not catch a misreading. The ext4 and
XFS images are filled with 0xA5 before formatting, and formatted without
discard. Allocated-but-unwritten blocks and tail slack therefore hold visible
garbage, and a parser that reads them as data fails instead of passing on
zeros.

Regenerate the images in WSL, as root, because the generators use loop mounts:

```
wsl -u root --exec bash lib/parser/testdata/gen/gen_fixtures.sh
wsl -u root --exec bash lib/parser/testdata/gen/gen_ntfs_extra.sh
```

The scripts share `gen/lib.sh`. They need dosfstools, mtools, exfatprogs,
xfsprogs, e2fsprogs, ntfs-3g, hfsutils, hfsprogs, gdisk, ewf-tools,
qemu-utils, zstd and python3, plus fsagen through WSL interop (`--fsagen
PATH`). They also leave uncompressed copies under `--dataset-dir DIR`
(default `/mnt/e/dataset/dues_synth`).

Tests load fixtures through `internal/testutil/fsfixture`:

```go
for _, m := range fsfixture.All(t) {
	path := fsfixture.Open(t, m) // decompressed once per test binary
	...
}
```

The tests that use them:
- `lib/parser/scan/fixture_invariants_test.go` checks the image layer and the
  parser against the manifests, including that extents read the same bytes as
  the library's own reader.
- `lib/index` checks that every file has a record whose content and hash are
  the true ones, and that the coverage counters reconcile.
- `lib/search` checks that every indexed file is found by a search for its own
  content.
- `internal/e2e` stores each image with the built binary, then checks that
  every file is indexed and restores to its true content.

### Other fixtures

- `lib/microartefact/detectors/testdata/hive.dat` is a registry hive written
  with Windows' offline registry library (`offreg.dll`). It holds a Run key with
  string, DWORD and binary values, and a chain of keys 508 deep.
- `testdata/fuzz` directories next to each fuzz target hold its seeds, including
  every input a fuzz run has found.

### Legacy databases

`internal/testutil/legacyfixture/testdata` holds DUES databases written by the
release before the correctness pass, in every storage mode:
- default and password-encrypted;
- quick, with and without a password;
- container, container with a password, and container with a hierarchical index;
- the sha3 hash algorithm and 64 KiB chunks;
- revrel and hocho;
- an E01;
- a duplicate store.

`golden.json` records, for each mode:
- the flags it was written with and its inputs;
- the `dues list` output;
- what restoring each object returned.

The back-compatibility tests:
- `TestLegacyRestore` restores every object with the current binary and
  compares the result with the true content.
- `TestLegacyReadsChangeNothing` checks that list, stats, repair's inspection,
  search and restore change no file and write no manifest.
- `TestStoreIntoLegacyDatabase` stores a new file into each fixture, which
  must keep the database's legacy encoding, and then restores old and new
  evidence.
- `TestLegacyE01AfterUpgrade` runs `repair --upgrade` on the E01 fixture.
- `TestLegacySearchAndNear` runs search and near against every fixture through
  the binary.

The fixtures age. badger rotates an encrypted database's data key once it is
older than seven days, and a read-only open cannot write the new key. Any
test that opens an encrypted fixture read-only must therefore open it through
`lib/dbopen`, which turns rotation off for read-only opens.

To regenerate them from a given commit (the default is the pre-pass commit):

```
bash scripts/fixtures/gen_legacy_dbs.sh [COMMIT]
```

### Real-image corpus

The large evidence corpus is not in the repository. It consists of:
- `E:\dataset`: raw images with oracle TSVs, and the jean E01 set;
- `E:\hyper\vhd`: Hyper-V VHDX disks with snapshot chains.

Treat both as read-only evidence. Tests that use the corpus skip unless a
`go test` flag names it:

| Flag | Test | What it checks |
|---|---|---|
| `-dues.corpus-vhd` (a directory) | `lib/parser/image` | Differencing disks resolve their parents from their own directory only; an incomplete chain refuses to open; reads that start inside a sector of a partially present block, and reads of a disk's partial last chunk, return the bytes libyal `vhdimount` returns |
| `-dues.corpus-ntfs` (images, separated as in `PATH`) | `lib/parser/fs/ntfs` `TestDecoderMatchesLibntfs` | DUES's LZNT1 decoder and libntfs agree on every live compressed file (8,463 on `jean.e01`) |
| `-dues.corpus-list` (a file of image paths) and `-dues.corpus-scratch` (a directory) | `internal/e2e` `TestCorpusRoundTrip` | Each image is stored with `--no-index`, then restored with full verification (and the EWF check for E01). Raw images must match their own SHA-256. Images too large for the scratch volume are skipped. |

```powershell
go test ./lib/parser/image/ -args '-dues.corpus-vhd=E:\hyper\vhd'
```

`cmd/dues-parsemanifest` writes a JSONL manifest of every partition and file
the parser finds in an image, with content digests computed two independent
ways (`-hash none` records structure only). `dues-parsemanifest diff` compares
two runs, and `-allow FILE` accepts listed differences. Run it across the
corpus before and after a parser or image-layer change. Every difference must
be explained.

Two corpus cautions:
- `E:` is a USB drive that can drop out mid-run. Check stderr for "externally
  altered" or "path not found", and rerun the affected images.
- The ubuntu VM in `E:\hyper\vhd` is sometimes running, so its newest snapshot
  changes between runs.

## Long tests

Tests that take minutes call `testutil.LongTest(t)` and run only in a build
with `-tags long` (`verify.ps1 -Long`, `suite.sh --long`). They cover scale
and boundaries:
- chunk sizes up to 64 MiB in `lib/ingest` `TestChunkBoundaryMatrix` (the
  smaller sizes run always);
- a partition of a million members, read streaming under a heap bound
  (`lib/dbio`);
- validation of four million chunks in bounded memory (`lib/store`);
- a folder of 10,000 files, with empty files, copies and an E01 set, stored
  through the binary (`internal/e2e` `TestFolderOfTenThousandFiles`).

## Helpers for writing tests

| Package | Use it for |
|---|---|
| `internal/testutil` | I/O fault injectors (`FailingReaderAt`, `ShortReaderAt`, `NewENOSPCWriter`, `ShortWriter`, `ErrCloser`, slow and blocking readers); deterministic data (`RandomBytes`, `WriteRandomFile`); `TreeHash` and `AssertNoFilesMatching`; gates (`NewDirFlag` and the mount flags `ENOSPCDir`, `LowSpaceDir`, `ReadOnlySrc` and `ReadOnlyDB`; `RequireLinuxNonRoot`; `LongTest`); leak and memory checks (`NoLeakedGoroutines`, `AllocBelow`, `PeakHeap`); `FakeClock`; subprocess scenarios (`RunChildScenarios`, `StartChild`, `NotifyPoint`, `ChildNotifyFile`, `WaitForPoint`, `Kill`, `Interrupt`), which get their settings as arguments; the built binary (`BuildDues`, `BuildBinary`, `RunDues`, `StartDues`, `FaultArgs`, `NoConfig`; the build functions take build tags); and `HashFile`, `HashRange` |
| `internal/testutil/vtscreen` | A small terminal for asserting what a person would see (below) |
| `internal/testutil/testdb` | Opening throwaway databases. `ForEachMode` runs a test once per storage mode (default, encrypted, quick, container). Every helper snapshots and restores the process-wide layout globals, so tests using it must not call `t.Parallel`. |
| `internal/faultinject` | Making the code fail at named points (see below) |

`RunDues` also holds every run to the output contract
([CLI_UX.md](CLI_UX.md)): it fails a test whose piped stdout or stderr holds an
escape code, unless the run asked for colour (`--color=always`). Parse `list` and `micro list` stdout as one JSON document;
the run header and notes are on stderr.

No test reads the account's own config file (`~/.config/dues/dues.json` or
`compression.json`), which could otherwise change any result: DUES finds the
home directory from the account, so no test setting can move it. `RunDues`
and `StartDues` put `--no-config` before the arguments unless they name a
config file, through `testutil.NoConfig`; a test that runs a binary any other
way passes its arguments through `NoConfig` too (the environment, PTY and
legacy-database tests do). In-process tests of `package main`, in the root and
in `cmd/compression`, point the default file at a missing one in `TestMain`.
A test of the config file writes its own and passes `--config`.

Terminal output itself is tested against a small terminal emulator,
`internal/testutil/vtscreen`:
- It applies `\r`, `\n`, cursor-up, line and display erase, cursor show and
  hide, and SGR, and lets a test assert the final screen, the cursor and the
  raw bytes.
- It fails the test on any other escape sequence, on one left unfinished
  (`Finish`), and on text that would wrap at the right margin.
- `AllowTaskbar` accepts OSC 9;4 taskbar progress ended by ST (never BEL)
  and records the states; `AllowWrap` wraps text as a terminal does, for a
  whole run whose report lines may be long.

The `internal/ui` tests draw every frame on it. Two tests in `internal/e2e`
run the binary on a real pseudo-terminal through util-linux
`script -qec "stty cols N rows 30; exec dues ..." /dev/null`:
- `TestLiveDisplayInPTY` stores a file at 100 and 50 columns and checks the
  screen a person is left with: the report once, each step's row once, the
  footer last, nothing left of the live display, the cursor shown. It also
  checks `dues version` (the banner) and `dues --version` (one line).
- `TestTaskbarInPTY` (`-tags faultinject`) holds a store for two seconds
  before its commit, so the display redraws, and checks that with
  `--taskbar` the taskbar progress ends cleared, and that without it there is
  none.

They skip without util-linux `script` (Windows, macOS, `-short`). The WSL
suite builds them with `-tags requirepty`, which turns that skip into a
failure.

### Fault injection

Code marks a point with `faultinject.Hit(faultinject.SomePoint)`. Normal
builds compile the call to nothing, and the gate fails a release binary that
carries the tag. A build with `-tags faultinject` takes a spec from the
hidden `--faults` flag of dues and compression (compression takes it before
the command), and tests in the process call `faultinject.Set`:

```
dues --faults=ingest.before_commit=exit:9@2,restore.mid=enospc store ...
```

The actions are:
- `error`;
- `enospc`, `eio` and `eacces`, which return the platform's own errors;
- `panic`;
- `exit:CODE`, which exits without deferred cleanup, as a crash would;
- `sleep:DURATION`;
- `block`;
- `signal:INT` (Ctrl-Break on Windows) and `signal:TERM` (Unix only).

`@N` fires on the Nth hit only, and `@N+` from the Nth hit on. When
`--faults-notify` names a file, the first hit of each point is written to
it, so a parent test can wait for the child to get there. A release build has
neither flag, so passing one is a usage error.

The points are listed in `internal/faultinject/points.go`, so a misspelt point
is an error, not a silent no-op. There are 20 of them:
- the ingest lifecycle: after the attempt is recorded, around the durability
  barrier, before FLUSHED, before and inside the commit, inside a duplicate's
  cleanup, at each indexed file;
- a container frame between header and payload; the manifest's temporary file;
- each restored chunk; each entry a reset removes;
- before and after an upload's ingest; before an archive is closed;
- inside search, near, enrich, `micro extract` and recovery, where tests
  interrupt them.

`TestEveryPointIsWired` fails when a point is never hit by the code, or named
by no test.

The suites that use them, in `internal/e2e` (tag `faultinject`):
- `crash_test.go` kills the dues and compression binaries at every point
  (`exit:3`, or `block` and a real kill). It then recovers with
  `repair --fix --reindex-failed`, stores the inputs again, and checks:
  - the listing equals an uninterrupted run's;
  - every completed evidence restores verified;
  - `repair --verify` passes;
  - no temporary or part file is left.
- `interrupt_test.go` signals each command at a point and expects exit 130 or
  143, a recoverable database and a released lock.
- `TestConcurrentProcesses` runs read commands in parallel processes, and
  checks that a second writer is refused while a store holds the database.

A kill-point test written by hand runs the binary in a child process and
waits for it to reach the point:

```go
c := testutil.StartDues(t, bin, work, "ingest.before_commit=block", "store", src, "--dbpath", db)
c.WaitForPoint(t, "ingest.before_commit#blocked", time.Minute)
c.Kill(t)
// reopen the database and check that recovery is correct
```

A scenario in a test binary is started the same way with
`testutil.StartChild(t, "store", spec)`; the child passes its first argument
and `testutil.ChildNotifyFile()` to `faultinject.Configure`.

### Filesystem-failure tests

`scripts/wsl/mounts.sh up` (run as root) mounts four filesystems:

| Mount | Flag | Use |
|---|---|---|
| `/mnt/dues-enospc`, a 16 MB tmpfs | `-dues.enospc-dir` | Real disk-full errors |
| `/mnt/dues-lowspace`, a 384 MB tmpfs | `-dues.lowspace-dir` | Room for a database and DUES's 128 MiB free-space reserve, so a store fills the volume part way |
| `/mnt/dues-ro-db`, a read-only bind of `/var/tmp/dues-ro-src` | `-dues.ro-src`, `-dues.ro-db` | A test builds a database in the source and reads it through the bind, as from write-protected media |

`mounts.sh args` prints the flags, one per line. `suite.sh` passes them after
`-args` to the packages that use them (`internal/e2e`, `internal/diskspace`
and `internal/products/compression_product`); a package that starts using a
mount must be added to its list. The flags are defined in `internal/testutil`
(`testutil.ENOSPCDir.Require(t)` and the rest), and a test skips when its flag
is not given. Permission tests call `RequireLinuxNonRoot`,
because root ignores file modes, so run `suite.sh` as a normal user.

`internal/e2e/fsfail_test.go` covers, for the commands that write:
- a full disk: the database stays whole and earlier evidence still restores;
- read-only media: every read command works, and every write command fails
  with an error saying the media is read-only;
- unreadable blobs, sources, data roots and log directories.

### Fuzzing

`scripts/fuzz-targets.txt` lists 32 targets:
- parsers and the image layer (FAT, exFAT, NTFS, ext, HFS, XFS, GPT, MBR,
  VHD, VHDX, LZNT1, extents, names, detection);
- storage formats (the blob codec, containers, block indexes, records,
  bounded msgpack decoding);
- micro-artefact parsers and detectors;
- the server's origin check and upload requests;
- the FFI dispatcher, archive entry names and the `.duesarc` manifest;
- configuration, the fault spec, terminal sanitising, log redaction and the
  search matcher.

The fuzz smoke builds each with `-tags safestrict`, so a panic that
`internal/safe` would contain is still a finding. The exception is
`FuzzExtract`: it feeds third-party parsers, whose contained panics are
expected, so only a crash past containment, a hang or unbounded output counts.

To run one target longer:

```
go test ./lib/parser/scan -run '^$' -fuzz '^FuzzScanNTFS$' -fuzztime 10m
```

A failing input is saved under the package's `testdata/fuzz/<Target>/`. Keep
it there after the fix: it then runs as a regression test with every
`go test`.
