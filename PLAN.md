# DUES / Indicer — Master Development Plan & Status Tracker

> Shared tracking file for the two of us. Update the **Status** column as work
> moves. Keep it honest — this is the single place to see where everything stands.
>
> **Legend:** ✅ done · 🟡 in progress · ⬜ planned / not started · ❌ blocked or cut ·
> 🔒 blocked by a dependency (see note)
>
> _Last updated: 2026-07-26._

---

## 0. Plan documents (the source-of-truth specs)

| Status | Doc | What it covers |
| --- | --- | --- |
| ✅ | [prodocs/PRODUCT_FEASIBILITY_ASSESSMENT.md](prodocs/PRODUCT_FEASIBILITY_ASSESSMENT.md) | Objective product feasibility; keep/abandon calls |
| ✅ | [prodocs/PRODUCT_DEVELOPMENT_PLAN.md](prodocs/PRODUCT_DEVELOPMENT_PLAN.md) | Two-pillar plan (qwikpak + forensics), phases, GTM |
| ✅ | [docs/ATTACK_PATH_GRAPH_LLD.md](docs/ATTACK_PATH_GRAPH_LLD.md) | Attack-path graph engine, rule engine, cross-case, retrospective |
| ✅ | [docs/LIBTSK_REMOVAL_PLAN.md](docs/LIBTSK_REMOVAL_PLAN.md) | Remove libtsk/CGo → pure-Go forensic stack |
| ✅ | [docs/AOIFLUX_LIBRARY_UPGRADE_PROMPTS.md](docs/AOIFLUX_LIBRARY_UPGRADE_PROMPTS.md) | Per-library planning prompts for the aoiflux libs |
| ✅ | [docs/AOIFLUX_VALIDATION_BUGS.md](docs/AOIFLUX_VALIDATION_BUGS.md) | Bugs found validating vs libtusk — **ALL RESOLVED + verified 2026-08-09** (libxfat v1.2.0, libntfs v0.3.1, libtable v0.2.2, libewf v0.2.1; libxfs v0.3.1 completeness **proven** via inobt report + `xfs_db`/kernel-mount external oracle in WSL) |

---

## 1. Repo hygiene

| Status | Item | Notes |
| --- | --- | --- |
| ✅ | Remove 5 stub products (cmd + internal/products + dispatcher) | Kept compression + ffi; build verified |
| ⬜ | Move `PRODUCT_IDEAS.md` / `PRODUCT_TECHNICAL_BLUEPRINT.md` under `vision/` | So they read as options, not roadmap |
| ✅ | Add licenses to vendored aoiflux libs (all 9 now MIT) | Gate 0 cleared 2026-07-26 |

---

## 2. Pillar A — qwikpak (compression / backup)  · _revenue first_

| Status | Phase | Item |
| --- | --- | --- |
| ✅ | — | Basic tier (archive create/extract/list/verify, formats, split, password) — **built** |
| ⬜ | A0 | Ship Basic free + benchmark page + launch (funnel) |
| ⬜ | A1 | Pro MVP: workspace + incremental snapshots + restore + single-key encryption |
| ⬜ | A2 ⭐ | The wedge: FTS search + version/near-dup intelligence over backup history |
| ⬜ | A3 | Parity: one S3-compatible cloud target + GFS retention |
| ⬜ | A4 | Later: CDC chunking, key rotation, more backends, conditions, RBAC |
| ⬜ | — | Gate: confirm Pro traction before ramping forensics (solo, sequential) |

---

## 3. Pillar B — Forensics / Attack-Path  · _the moat_

### 3.1 Attack-path engine (AP phases — see LLD §16)

| Status | Phase | Item |
| --- | --- | --- |
| ⬜ | AP-0 | Graphene re-baseline + entity-upsert + canonical-key/normalize lib |
| ⬜ | AP-1 | Layer split (observations at any level + entities); decouple graphing from extraction; `producer_version` tagging |
| ⬜ | AP-2 | Structured extractors: browser SQLite, registry watchlist, cmd/PS history, prefetch |
| ⬜ | AP-3 | Rule-engine migration (replace hard-coded builders, parity-gated) + web/exec/persistence packs |
| ⬜ | AP-4 | Timestamps/MACB + super-timeline + probabilistic rules |
| ⬜ | AP-5 | Path assembly + MITRE mapping + scoring + export |
| ⬜ | AP-6 | Multi-modal roots (pcap, memory) + `ParseProvider`/`MemoryProvider` interfaces |
| ⬜ | AP-7 | Cross-evidence correlation pack (SimHash clustering, shared C2/account) |
| ⬜ | AP-8 | Fragmented-file reassembly; $MFT lazy timeline; email/pcap depth |
| ⬜ | AP-9 | Case scope + cross-case entity index + campaign clustering (ACL-aware) |
| ⬜ | AP-10 | Retrospective: versioned layers, retro-hunt, re-derivation diff, retro-alerts |

### 3.2 Rule engine (LLD §9)

| Status | Item |
| --- | --- |
| ⬜ | JSON declarative evaluator (field_eq / shared_neighbor / hamming_within) |
| ⬜ | Mutant power-tier integration — T1 fixture boundary (path/JSON) |
| ⬜ | Mutant T2 in-process `Eval` API (owner-controlled addition to Mutant) |
| ⬜ | Migration parity gate: port current 2 builders → JSON rules, diff, delete builders |
| ⬜ | Mutant licensing posture for DUES (dual/relicense — owner decision) |

---

## 4. libtsk / libtusk removal → pure-Go stack (see LIBTSK_REMOVAL_PLAN.md)

| Status | Phase | Item |
| --- | --- | --- |
| ✅ | Gate 0 | Licenses added — all 9 aoiflux libs now **MIT** (verified 2026-07-26) |
| ✅ | Lib prereqs | **All 9 aoiflux libs upgraded & released (§5)** — P1–P3 fully unblocked, upstream-fix-free |
| ✅ | Contract | `lib/parser/core` interfaces (Image/PartitionTable/Filesystem/AbsRange/MACB/FileRecord) — built, vets clean |
| ✅ | P1 | ImageLayer `lib/parser/image` (raw/dd/split + EWF + VHD/VHDX + detect) + PartitionLayer `lib/parser/partition` (libtable) — **built + tested** |
| ✅ | P2 | FS reference adapters **NTFS + XFS** (`lib/parser/fs/{ntfs,xfs}`) — built + tested |
| ✅ | P3 | FS adapters **exFAT + FAT + ext + HFS+** (`lib/parser/fs/{exfat,fat,ext,hfs}`) + `fsutil` — built + tested; libxfat bumped → v1.1.0. **All 6 filesystems now implement core.Filesystem** |
| ✅ | P4a–c | **FSDetector + `fs` dispatch** (all 6 FS, tested) · **`scan` orchestrator** (image→partition→fs→FileRecord, streaming) · **`tskcompat`** (emits exact libtusk JSON) — all built + tested. **Whole pure-Go stack (13 pkgs) builds CGo-free & green** |
| ✅ | P4d | **`--parser=go\|tusk` switch wired** into `cli/cmdstore` (default `tusk`; `go` routes through `tskcompat`). Both builds pass (CGo-free **and** default libtusk); flag validates. Cautious-parallel: old path untouched |
| ✅ | Validate tool | **`dues parser-diff <image>`** runs both backends and reports parity (files, sizes, fragment offsets) — `lib/parser/pdiff` (tested) + CLI command |
| ✅ | E2E test | **`lib/parser/scan/e2e_test.go`** synthesizes a real FAT32 image (go-diskfs), runs the full stack, and verifies **file content read via absolute fragment offsets** — proves the DataRuns contract end-to-end for FAT/detect/scan |
| ✅ | Validate | **DONE 2026-08-07** — `parser-diff` run on the full `N:\dev\dataset` corpus. **File parity confirmed** on FAT16/FAT32/NTFS/ext4/exFAT/HFS+/GPT(multi-part); go **parses XFS + split-E01 that libtusk cannot** (tusk=0). Only non-file diffs are dirs + FS pseudo-files (by design). Fixes applied: NTFS data-run off-by-one; EWF `.E01/.E02` segment auto-discovery; exFAT strict→optimistic (libxfat bug); NTFS adapter panic-recovery (libntfs bug). 2 library bugs + 2 follow-ups filed → [AOIFLUX_VALIDATION_BUGS.md](docs/AOIFLUX_VALIDATION_BUGS.md) |
| 🟡 | P4e | Foundation ✅: `lib/parser/fragio.FragmentReader` (logical-over-fragments io.Reader/ReaderAt, sparse-aware) — tested, no working code touched. Remaining (gated on validation): wire into store chunker + multi-extent `IndexedFile` + restore |
| ✅ | P5 | **CUTOVER COMPLETE (2026-08-10) — libtusk/CGo fully removed.** Deleted `tusk_amd64.go`, `tusk_fallback.go`, `clib/` (both `.a` + ABI md), the `--parser` flag + `cnst.PARSER/SetParser`, the `parser-diff` command + `cmdparserdiff.go`, and the `!notusk` build tag. Store path now always routes through `tskcompat` (pure-Go). `build.sh`/`build.ps1` set `CGO_ENABLED=0` (no CC/MinGW/musl/zig); TOOLCHAIN_NOTES marked superseded. **Binary builds pure-Go on linux/amd64+arm64, darwin/arm64, windows (verified, no C toolchain).** `go build ./...` + tests green. (pdiff kept as a reusable regression tool.) |
| ⬜ | P4 | tskcompat JSON shim + FSDetector + shadow differential testing |
| ⬜ | P5 | Delete CGo (tusk_amd64.go, clib/*.a, !notusk tag, toolchain) → pure Go |
| ⬜ | P6+ | New image/volume/FS formats (§6 backlog) |

---

## 5. aoiflux library upgrades — ✅ ALL COMPLETE (verified 2026-08-07 at released versions)

_Prompts in AOIFLUX_LIBRARY_UPGRADE_PROMPTS.md. Every critical/feature/QoL gap addressed;
the whole pure-Go image + partition + filesystem foundation is ready. Remaining work is
DUES-side (§4 P1–P2)._

| Status | Library | Ver | License | What landed |
| --- | --- | --- | --- | --- |
| ✅ | libewf | **v0.2.1** | ✅ MIT | Size()/SectorSize, ErrEncrypted, MD5/SHA1+Verify, options; **+OpenPath (multi-segment)** |
| ✅ | libvhdi | v0.2.0 | ✅ MIT | auto parent-chain resolver, size-from-reader Open, VHDX log replay |
| ✅ | libtable | **v0.2.2** | ✅ MIT | AmbiguityPolicy/Prefer, Detect/ParseAll, warnings; **+GPT overlap fix & backup-GPT labeling** |
| ✅ | libfat | v0.2.0 | ✅ MIT | FragmentOffsets/FragmentResult (+deleted, provenance) |
| ✅ | libxfat | **v1.2.0** | ✅ MIT | NewFromReaderAt + Get*Time MACB; **+strict name-checksum/UTF-16 fix, validators** |
| ✅ | libntfs | **v0.3.1** | ✅ MIT | OpenStream/Streams, EA, Fragments/IsFragmented, UsnJrnl/LogFile; **+panic-safe ReadDir** |
| ✅ | libext | v0.2.0 | ✅ MIT | Extents/DataRuns, DeletedEntries/ScanDeleted, OrphanInodes, inline+fast-commit |
| ✅ | libhfs | v0.2.0 | ✅ MIT | GetTimes/CatalogTimes MACB, xattr, WalkDeleted/RecoverDeleted, classic HFS |
| ✅ | libxfs | **v0.3.1** | ✅ MIT | complex-directory hardening; **+Icount/Ifree, EnumerateAllocatedInodes (inobt), InodeCompletenessReport** — walk-completeness proven |

> **go.mod action:** bump `libxfat` v1.0.6 → **v1.1.0**; add the other eight at the versions
> above as the ImageLayer/FilesystemLayer adapters (§4 P1–P2) are built.

---

## 6. New libraries backlog (coverage beyond current formats)

| Status | Priority | Module | Layer |
| --- | --- | --- | --- |
| ⬜ | high | libraw (raw/dd/split) | Image |
| ⬜ | high | libqcow, libvmdk, libvdi | Image (VM forensics) |
| ⬜ | med | libaff4, libdmg | Image |
| ⬜ | high | libapfs | Filesystem (modern macOS) |
| ⬜ | high | libbtrfs | Filesystem |
| ⬜ | med | libiso (ISO9660/UDF), libufs | Filesystem |
| ⬜ | med | liblvm, libluks, libbde | Volume / Crypto |
| ⬜ | low | libzfs, librefs, libf2fs, libjfs, libreiser | Filesystem |

---

## 7. Open decisions (need a call)

| Status | Decision |
| --- | --- |
| ✅ | aoiflux libs license — **MIT** chosen for all 9 (done 2026-07-26) |
| ⬜ | Mutant license posture for use in DUES |
| ⬜ | Move vision docs to `vision/` subfolder |
| ⬜ | qwikpak Pro revenue-timeline pressure (affects A1→A2 pace) |
| ⬜ | Rule-engine `shared_neighbor` as a first-class JSON anchor kind (recommended yes) |
