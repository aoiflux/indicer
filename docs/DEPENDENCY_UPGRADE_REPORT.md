# Dependency upgrade report

This report records every module the correctness pass upgraded, what changed
in DUES because of it, and the upstream defects the pass found. The baseline
is commit `41dbc74`, the last commit before the pass. Defect IDs refer to
`docs/CORRECTNESS_REPORT.md`.

Policy:
- Every `github.com/aoiflux/*` module is at its latest release.
- Third-party modules are at their latest release as published, workarounds
  included where an upstream defect is still present.
- No upstream issue has been filed. The defects are listed at the end.

## Toolchain

| Item | Before | After | Why |
|---|---|---|---|
| `go` directive (language floor) | `go 1.26.2` | `go 1.26.2` | Unchanged, so DUES still builds with Go 1.26.2 |
| `toolchain` line | none | `toolchain go1.27.3` | Standard-library vulnerabilities reachable from DUES: GO-2026-6613, 6608, 6607, 6605, 6600 and 6599 (`net/http`, `net/textproto`, `crypto/tls`, `html/template`), fixed in go1.27.2; GO-2026-6809 (`crypto/x509`, through `evidence.Fingerprint`), fixed in go1.27.3 |

With `GOTOOLCHAIN=auto`, the default, the `go` command fetches go1.27.3 when
the installed toolchain is older. A `go get` once raised the `go` directive to
1.27; check it after every `go get`.

The analysis tools are pinned in `scripts/tools.env`: staticcheck v0.8.1 (run
under go1.26.6, because staticcheck cannot read export data of toolchains newer
than it knows) and govulncheck v1.8.0.

## aoiflux libraries

| Module | Before | After |
|---|---|---|
| `github.com/aoiflux/graphene` | v0.4.0 | v0.9.0 |
| `github.com/aoiflux/libewf` | v0.2.1 | v0.2.1 (already the latest) |
| `github.com/aoiflux/libext` | v0.2.0 | v0.4.2 |
| `github.com/aoiflux/libfat` | v0.2.0 | v0.3.5 |
| `github.com/aoiflux/libhfs` | v0.2.0 | v0.3.2 |
| `github.com/aoiflux/libntfs` | v0.3.1 | v0.3.6 |
| `github.com/aoiflux/libtable` | v0.2.2 | v0.2.4 |
| `github.com/aoiflux/libvhdi` | v0.2.0 | v0.4.2 |
| `github.com/aoiflux/libxfat` | v1.2.0 | v1.4.1 |
| `github.com/aoiflux/libxfs` | v0.3.1 | v0.4.1 |

### graphene v0.4.0 to v0.9.0

API changes:
- Traversal takes a `GraphReader`.
- The `PropertyIndex` `FilterMask` changed, `DriverFilter` was removed, and
  `ReindexReject` is now the zero value.

Behaviour changes:
- Nodes and edges without labels are refused. DUES now refuses them itself when
  they are written, naming the node, instead of failing the commit.
- `Open` takes an exclusive `graphene.lock`. A second writer gets
  `disk.ErrStoreLocked`, which DUES reports as `ErrGraphLocked` naming the
  holder.
- The declared-constraint catalogue is persisted.

Features DUES adopted:
- `OpenReadOnly` (shared lock) for `list`, `micro list` and the TUI micro view,
  so read-only DUES processes no longer exclude each other (GRA-5, GRA-21).
- `OpenLive` (no process lock) on read-only media.
- `Tx.IndexNodeProperties`, `IndexEdgeProperties`, `UpdateNodeIndexed` and
  `UpdateNodePartialIndex`, so index entries commit with the nodes
  (GRA-2/3/4).
- `CountNodesByType` and `CountEdgesByType` for store detection (GRA-9).
- `disk.PreflightOpen` for the log-size warning (GRA-6).

Compatibility: DUES never calls `Compact`, so the store stays log-only (D6) and
a release that reads only the log can still open it. The open cost grows with
the write history; DUES warns at 1 GiB or 5M records.

### libvhdi v0.2.0 to v0.4.2

- v0.3.0: a nil `ParentResolver` resolves parent locators read from the
  evidence, which could open host, UNC or `..` paths (PAR-9). DUES passes
  `siblingResolver`:
  - it matches base names only, in the child's own directory;
  - it opens through `os.OpenInRoot` and accepts regular files only;
  - it sets `RequireParentChain` and `MaxChainDepth` 16.

  A disk read from a bare reader uses `NoParentResolution` and is refused when
  it needs a parent. `Version` became a function.
- v0.4.1: a false "parent chain contains a cycle" on every Hyper-V `.avhdx` is
  fixed (PAR-25). DUES treats an incomplete chain as an open error, so the
  corpus image with a missing ancestor fails closed.
- v0.4.2: unaligned reads in partially present VHDX blocks (PAR-31) and the
  last chunk's sector bitmap (PAR-33) are fixed. DUES's `sectorAlignedReader`
  and `vhdx_tail.go` workarounds are deleted. All five corpus snapshot leaves
  decode to the same SHA-256 before and after.

### libntfs v0.3.1 to v0.3.6

- `Open` promotes an `io.WriterAt`, so DUES opens with
  `OpenWithOptions{ReadOnly: true}`. `TestOpenIsReadOnly` checks that the
  volume is not writable and a walk writes nothing.
- DUES keeps the library's `BaseOffset` at 0 and adds the partition offset
  once itself.
- v0.3.6 fixes:
  - compression units are decoded at their real size (PAR-27);
  - the MFT record count is bounded (PAR-34);
  - `ReadSupport().WOFCompressed` reports WOF files (NTFS-U1);
  - `MFTDataRunsError` reports a damaged `$MFT` run list.

  DUES keeps its own LZNT1 decoder, because deleted records have no
  attribute-level reader in libntfs. `TestDecoderMatchesLibntfs` checks that
  both agree on every live compressed file (8,463 on `jean.e01`). DUES also
  keeps its own bound on the MFT count (partition size divided by record
  size), which is stricter than the library's.
- WOF detection applies to the unnamed stream only, so a `WofCompressedData`
  stream is indexed like any other stream. A WOF file's unnamed stream is
  still recorded as unavailable: libntfs does not decode WOF.

### libext v0.2.0 to v0.4.2

- Adopted in the adapter:
  - `DataRuns` with `Unwritten` and `FileOffset` (PAR-5);
  - `InlineData`;
  - `ScanDeletedContext` with each entry's own block map (PAR-6);
  - `Warnings()`;
  - opening `Permissive`.

  Directories are listed one at a time. `WalkDir` ended at the first bad
  directory before v0.4.2 (EXT-U2).
- v0.4.0: a symlink target of exactly 60 bytes is read correctly; 413 more
  symlinks are indexed on the ubuntu corpus image.
- v0.4.2: inline sizes are bounded (PAR-35), and `ReadAt` reads inline data
  instead of zeros (EXT-U1). DUES's `checkInline` and its inline branch of
  `Open` are deleted. A forged inline size is now refused by libext with
  `ErrUnsupportedLayout`.

### libfat v0.2.0 to v0.3.5 and libxfat v1.2.0 to v1.4.1

- libxfat v1.4.0 renamed or removed about 35 symbols and returns `*ExFAT` by
  pointer:

  | Before | After |
  |---|---|
  | `GetFullPathIndexableEntries` | `ContiguousFilePaths`, then `WalkWithOptions` |
  | `GetAllEntries` | `AllEntries` |
  | `Get*` accessors | bare names |
  | `HasFatChain` | `!IsContiguous` |
  | `ClusterOffset` (could wrap at cluster 0) | `ClusterOffset` returning an error |

- Adopted in both adapters:
  - `WalkWithOptions` with deleted entries, deleted directories and orphans,
    cycle-safe and depth-capped (PAR-7);
  - `FragmentOffsetsWithOptions`, assuming deleted files are contiguous
    (PAR-29);
  - `RecoverDeletedLongNames`;
  - the `Virtual` flag for `$MBR` and `$FAT` entries (PAR-19).
- libfat v0.3.5 and libxfat v1.4.1 report the directories a walk skips
  (`OnSkip`) and a truncated orphan or recovery sweep (`OnOrphanScan`,
  `OnRecoveredScan`) (FAT-U1, XFAT-U1). DUES's `PreorderTracker` and its
  probes are deleted.
- These releases also fix a live directory listed after a deleted entry that
  names its cluster: its subtree is no longer lost (found by
  `TestWalkKeepsLiveDirectoryAfterDeletedOne`).

### libhfs v0.2.0 to v0.3.2, libxfs v0.3.1 to v0.4.1

- libhfs:
  - `DataForkRanges`, which includes classic HFS's allocation-block start and
    an HFS wrapper's embedded offset;
  - decmpfs decoding;
  - `WalkDeletedContext` and `ExtentRanges`;
  - `MaxAlloc`.

  Classic HFS volumes, dropped before, are now read (PAR-26).
- libxfs:
  - `DataRuns(ctx, ino)` with `FileOffset`, `IsSparse` and `IsUnwritten`, which
    cut block slack and keep holes (PAR-4);
  - `ScanDirectoryRecordsWithOptions`;
  - `OpenInodeReader`.

### libtable v0.2.2 to v0.2.4, libewf v0.2.1

- libtable: DUES passes on its warnings, including overlaps. v0.2.4 adds
  `Table.ByteRange`, which computes partition byte ranges without overflow
  (TABLE-U1) and replaces DUES's own `byteRange`.
- libewf v0.2.1 was already the latest. DUES now uses its own segment discovery
  (`SegmentPaths`, so `case.Exe` is not a segment: PAR-23), opens with
  `OpenPathWithOptions(ChecksumWarn)`, and reads `Metadata()` for the stored
  MD5/SHA-1 and the chunk-table checksum failures (PAR-18).

## Third-party modules

| Module | Before | After | Why |
|---|---|---|---|
| `github.com/dgraph-io/badger/v4` | v4.9.1 | v4.9.6 | #2268: read-only opens no longer delete empty value logs or open them writable. #2310: a deleted key no longer reappears after value-log GC, which DUES runs after store and `repair --fix`. |
| `google.golang.org/grpc` | v1.81.1 | v1.85.0 | GO-2026-6348, GO-2026-6061, GO-2026-6443 |
| `golang.org/x/net` | v0.55.0 | v0.62.0 (now indirect) | GO-2026-6617, 6612, 6611, 6610, 6603; `h2c` dropped |
| `golang.org/x/text` | v0.37.0 | v0.43.0 | GO-2026-5970 (infinite loop, reachable through pdfcpu), GO-2026-6629 |
| `golang.org/x/image` | v0.41.0 | v0.47.0 | GO-2026-5061 (webp panic) |
| `golang.org/x/crypto` | v0.52.0 (indirect) | v0.58.0 (direct) | `argon2` for the new key derivation |
| `golang.org/x/sys` | v0.45.0 | v0.49.0 | File locks (`Flock`, `LockFileEx`), `MoveFileEx`, delete-on-close temporary files |
| `golang.org/x/term` | v0.43.0 (indirect) | v0.47.0 (direct) | Terminal detection for `dues reset` |
| `github.com/klauspost/compress` | v1.18.6 | v1.20.3 | GO-2026-5841 (s2 out-of-bounds read); `WithDecoderMaxMemory` |
| `google.golang.org/protobuf` | v1.36.11 | v1.36.12 | Required by grpc v1.85.0 |
| `github.com/edsrzf/mmap-go` | v1.2.0 | v1.2.0 (now indirect) | Ingest no longer maps evidence |

Modules left at their latest release:
- `github.com/vmihailenco/msgpack/v5` v5.4.1: DUES decodes through
  `internal/msgpackx` because of STO-25.
- `github.com/saferwall/pe` v1.6.5: GRA-22 and PE-U1 are worked around.

Indirect modules (otel, logr, genproto) moved with grpc.

### badger configuration

Not a version change, but these changes came with the badger work:
- `dbio.BadgerZSTDLevel` is 3, not 15. badger builds one process-wide encoder
  from the level of its first table, and level 15 kept about 800 MB of encoder
  state on a 16-CPU machine (FFI-4). Tables on `jean.e01` were the same size
  within 1.5%, and decoding does not depend on the level, so existing databases
  are unaffected.
- Read-only opens set `EncryptionKeyRotationDuration` to its maximum (CFG-14).
- The `ValueLogMaxEntries` override is removed, and value-log GC runs after
  store and `repair --fix` (CFG-9).

### Server transport

grpc-go is used for its generated types and `metadata.MD` only. DUES serves
Connect, gRPC and gRPC-Web over `net/http`, and never creates a grpc-go server
or client. `h2c` is replaced by `http.Server.Protocols` with `HTTP1` and
`UnencryptedHTTP2`, so the `Upgrade: h2c` handshake is no longer honoured;
such requests are served as HTTP/1.1, and HTTP/2 needs prior knowledge.

## Security scan

| | Reachable vulnerabilities (govulncheck, symbol level) |
|---|---|
| Baseline (`41dbc74`) | 4: GO-2026-6348 and GO-2026-6061 (grpc), GO-2026-5970 (x/text), GO-2026-5061 (x/image) |
| Before the toolchain line (W0) | 16: the 4 above, GO-2026-6629 (x/text), 5 in x/net, 6 in the standard library |
| Now | 0, in source and binary mode |

One finding remains at module level: GO-2026-5932, the unmaintained
`golang.org/x/crypto/openpgp` package. DUES never imports it, and no fix
exists.

## Compatibility impact

- **Graph store:** stays log-only (D6). A graph written by this release opens
  with any release that reads the log.
- **exFAT:** the libxfat renames affect only DUES's adapter. The adapter now
  indexes fragmented, empty and deleted files, which the old one dropped.
- **VHD/VHDX:** a differencing disk whose parent is not beside it, or whose
  chain is incomplete, now fails to open instead of being read with zeros
  where the parent's data belongs.
- **NTFS:** volumes open read-only. Compressed files that failed with LZNT1
  errors (4,842 on `jean.e01`) now decode.
- **badger:** databases written at table level 15 read unchanged.
- **Toolchain:** building needs Go 1.26.2 or later. go1.27.3 is fetched
  automatically unless `GOTOOLCHAIN=local` forbids it.

## Upstream defects found

None of these has been filed. The fix-request drafts are in the gitignored
`plans/upstream-*.json`.

| ID | Project | Defect | Status |
|---|---|---|---|
| PAR-25 | libvhdi | False "parent chain contains a cycle" for Hyper-V snapshots | Fixed in v0.4.1 |
| PAR-31 | libvhdi | Unaligned read in a partially present VHDX block starts at the sector's start | Fixed in v0.4.2; workaround deleted |
| PAR-33 | libvhdi | The last chunk's sector bitmap is taken as absent | Fixed in v0.4.2; workaround deleted |
| PAR-27 | libntfs | Compression units decoded at a quarter of their size | Fixed in v0.3.6 |
| PAR-34 | libntfs | MFT record count unbounded (out of memory on one flipped byte) | Fixed in v0.3.6; DUES keeps its own bound |
| NTFS-U1 | libntfs | WOF files read as zeros and reported readable | Detection fixed in v0.3.6; WOF decoding not provided |
| PAR-35 | libext | Inline inode size allocated before it is checked | Fixed in v0.4.2; workaround deleted |
| EXT-U1 | libext | `File.ReadAt` reads inline data as zeros | Fixed in v0.4.2 |
| EXT-U2 | libext | One unlistable directory ends `WalkDir` | Fixed in v0.4.2; DUES does not use `WalkDir` |
| FAT-U1 | libfat | Walks skip unreadable, depth-capped and revisited directories silently | Fixed in v0.3.5 (`OnSkip`) |
| XFAT-U1 | libxfat | The same, for exFAT | Fixed in v1.4.1 |
| TABLE-U1 | libtable | `ByteOffset`/`ByteSize` wrap; no warning for an entry past the image | Fixed in v0.2.4 |
| CFG-13a | badger | `y.Wrap` formats errors with `%+v`, so `errors.Is` fails | Present in v4.9.6; DUES also matches the text |
| CFG-13b | badger | A failed read-only open leaves the memtable log mapped | Present in v4.9.6; DUES checks for memtable logs first |
| CFG-14 | badger | A read-only open that rotates an old data key calls `log.Fatalf` | Present in v4.9.6; DUES turns rotation off for read-only opens |
| CFG-15 | badger | `DISCARD` is opened read-write on a read-only open, and a failure calls `log.Fatalf` | Present in v4.9.6; DUES opens from a shadow directory |
| STO-25 | msgpack | A declared slice length is preallocated (the 1M-element cap never applies) | Present in v5.4.1; `internal/msgpackx` checks before decoding |
| GRA-22 | saferwall/pe | `Close` unmaps the data even for a file made by `NewBytes` | Present in v1.6.5; DUES never closes such files |
| PE-U1 | saferwall/pe | `NewBytes` logs to stdout unless given a logger | Present in v1.6.5; DUES passes a discarding logger |
