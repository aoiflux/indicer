# Testing DUES

This page explains how DUES is tested: the verify gate that every change must
pass, the fixtures the tests run against, and how to add a test for a new bug.

## The verify gate

Run the whole gate from the repository root:

```powershell
scripts\verify.ps1            # everything, including the WSL part
scripts\verify.ps1 -Fast      # format, vet, builds and tests only
```

On Linux, or inside WSL, run `scripts/verify.sh` instead. It accepts `--fast`,
`--no-vuln`, `--no-race` and `--no-fuzz`.

The gate runs these steps and prints one PASS or FAIL line per step:

| Step | What it checks |
|---|---|
| format | `go run ./internal/tools/fmtcheck`. Plain `gofmt -l` flags every file on a CRLF checkout, so fmtcheck normalises line endings before comparing. |
| vet | `go vet ./...`, once without and once with `-tags faultinject` |
| cross build | Release builds for windows, linux and darwin on amd64 and arm64, for `.` and `./cmd/compression`. A binary built with the `faultinject` tag fails the step. |
| staticcheck | Runs under the toolchain pinned in `scripts/tools.env`, because older staticcheck cannot read newer export data |
| govulncheck | Source mode, plus binary mode on the Linux build |
| test | `go test ./...`, once without and once with `-tags faultinject` |
| WSL suite | `scripts/wsl/sync.sh` copies the working tree to ext4 with LF line endings. `scripts/wsl/suite.sh` then runs race tests, fault-injection tests, filesystem-failure tests and fuzz smoke. |

Tool versions live only in `scripts/tools.env`. Fuzz targets are listed in
`scripts/fuzz-targets.txt`, and each runs for `FUZZTIME` (default 60s).

## Known defects in tests

During the correctness pass, many tests describe correct behaviour that the
code does not deliver yet. Such a test skips with a message naming the bug
inventory ID:

```
--- SKIP: .../indexed:/a.bin  known defect ING-19: ...
```

The change that fixes a bug deletes the skips for its ID, so the test then has
to pass. A test that passed before and now fails is reported as a regression,
not skipped. To list what is still outstanding:

```
go test ./... -v 2>&1 | grep -o "known defect [A-Z]*-[0-9]*" | sort | uniq -c
```

## Fixtures

### Filesystem images

`lib/parser/testdata/fixtures/<group>/` holds small disk images (zstd
compressed, about 3 MB in total), each with a `*.manifest.json` ground truth.
For every file on the volume, the manifest records:
- path, kind, size and content SHA-256;
- the properties the parser should report: fragmented, sparse, unwritten, inline, alternate streams, hard links;
- for deleted files, whether they are recoverable.

The manifests deliberately contain no offsets. Finding the data is the parser's
job; the test checks only that what it finds hashes to the true content.

The groups:
- FAT12, FAT16 and FAT32 (made with mkfs.vfat and the kernel driver, and also by fsagen);
- exFAT;
- ext4, with inline data, preallocated and unwritten extents, holes, symlinks and hard links;
- XFS, with four allocation groups, tail slack, holes, unwritten extents and many extents;
- NTFS from fsagen, with resident data, fragmentation, alternate data streams, hard links and deletions;
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

Regenerate the images in WSL, as root, because the generator uses loop mounts:

```
wsl -u root --exec bash lib/parser/testdata/gen/gen_fixtures.sh
```

The script needs dosfstools, mtools, exfatprogs, xfsprogs, e2fsprogs,
hfsutils, hfsprogs, gdisk, ewf-tools, qemu-utils, zstd and python3, plus
fsagen through WSL interop (`$FSAGEN`). It also leaves uncompressed copies
under `$DUES_DATASET_DIR` (default `/mnt/e/dataset/dues_synth`).

Tests load fixtures through `internal/testutil/fsfixture`:

```go
for _, m := range fsfixture.All(t) {
	path := fsfixture.Open(t, m) // decompressed once per test binary
	...
}
```

The tests that use them:
- `lib/parser/scan/fixture_invariants_test.go` checks the image layer and the parser against the manifests;
- `internal/e2e` stores each image with the built binary, then checks that every file is indexed and restores to its true content.

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

`TestLegacyRestore` restores every object with the current binary and compares
the result with the true content. This is the back-compatibility gate: new
code must keep reading these databases.

To regenerate them from a given commit (the default is the pre-pass commit):

```
bash scripts/fixtures/gen_legacy_dbs.sh [COMMIT]
```

### Real-image corpus

The large evidence corpus is not in the repository. It consists of:
- `E:\dataset`: raw images with oracle TSVs, and the jean E01 set;
- `E:\hyper\vhd`: Hyper-V VHDX disks with snapshot chains.

A few tests use the corpus directly and skip unless it is named:

```powershell
$env:DUES_CORPUS_VHD = 'E:\hyper\vhd'
go test ./lib/parser/image/
```

`lib/parser/image` uses the small `drive.vhdx` snapshot chain to check that a
differencing disk resolves its parents from its own directory only. It also
checks that an incomplete chain refuses to open.

`cmd/dues-parsemanifest` writes a JSONL manifest of every partition and file
the parser finds in an image, with content digests computed two independent
ways. Run it across the corpus before and after a parser change and diff the
results. Every difference must be explained.

## Helpers for writing tests

| Package | Use it for |
|---|---|
| `internal/testutil` | I/O fault injectors (`FailingReaderAt`, `ShortReaderAt`, `NewENOSPCWriter`, `ShortWriter`, `ErrCloser`, slow and blocking readers); deterministic data (`RandomBytes`, `WriteRandomFile`); `TreeHash`, `AssertNoFilesMatching` and `Golden`; environment gates (`RequireEnvDir`, `RequireLinuxNonRoot`, `LongTest`); leak and memory checks (`NoLeakedGoroutines`, `AllocBelow`, `PeakHeap`); `FakeClock`; subprocess scenarios (`RunChildScenarios`, `StartChild`, `WaitForPoint`, `Kill`, `Interrupt`); and the built binary (`BuildDues`, `RunDues`) |
| `internal/testutil/testdb` | Opening throwaway databases. `ForEachMode` runs a test once per storage mode (default, encrypted, quick, container, hierarchical). Every helper snapshots and restores the process-wide layout globals, so tests using it must not call `t.Parallel`. |
| `internal/faultinject` | Making the code fail at named points (see below) |

### Fault injection

Code marks a point with `faultinject.Hit(faultinject.SomePoint)`. Normal
builds compile the call to nothing. A build with `-tags faultinject` reads
`DUES_FAULTS`:

```
DUES_FAULTS="ingest.before_commit=exit:9@2,restore.mid=enospc"
```

The actions are:
- `error`;
- `enospc`, `eio` and `eacces`, which return the platform's own errors;
- `panic`;
- `exit:CODE`;
- `sleep:DURATION`;
- `block`;
- `signal:INT` (Ctrl-Break on Windows) and `signal:TERM`.

`@N` fires on the Nth hit only, and `@N+` from the Nth hit on. Points must be
registered in `internal/faultinject/points.go`, so a misspelt point is an error,
not a silent no-op.

A kill-point test runs the scenario in a child process and waits for it to reach the point:

```go
c := testutil.StartChild(t, "store", "DUES_FAULTS=ingest.before_commit=block")
c.WaitForPoint(t, "ingest.before_commit#blocked", time.Minute)
c.Kill(t)
// reopen the database and check that recovery is correct
```

### Filesystem-failure tests

`scripts/wsl/mounts.sh up` (run as root) mounts two filesystems:
- a 16 MB tmpfs at `/mnt/dues-enospc`, for real disk-full errors;
- a read-only tmpfs at `/mnt/dues-ro`.

Tests find them through `DUES_TEST_ENOSPC_DIR` and `DUES_TEST_RO_DIR`, and skip
when those variables are unset. Permission tests call `RequireLinuxNonRoot`,
because root ignores file modes.
