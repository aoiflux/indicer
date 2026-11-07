# DUES correctness report

This report closes the correctness pass on DUES. It lists every defect the pass
found, why each class of defect existed, what now holds, how that is tested,
what risk remains, and whether DUES is ready for production use.

The pass ran from 2026-10-05 to 2026-10-30. It started from commit `41dbc74`
and landed in these commits on `dev`:

| Workstream | Commit | Scope |
|---|---|---|
| W0, W1 | `71fef6f` | Test harness, fixtures, verify gate; dependency upgrades |
| W2 | `74844b5` | Manifest, keys, open path, logging, panics, signals, configuration |
| W3 | `c7ee4a2`, `12a83fa` | Storage integrity |
| W4 | `a867177` | Unified content reader; restore with verification |
| W5 | `02aa6b6` | Parsers, extent contract, image layer, streaming scan |
| W6 | `7f7bd6e` | Evidence source, ingest orchestration, indexing, idempotence |
| W7 | `e38697e` | Search, NeAR, full-text search |
| W8 | `e23a057` | Graph, enrichment, micro-artefacts |
| W9 | `177f694` | Server and API |
| W10 | `a272357` | FFI and the compression product |
| W11 | `375d6e6` | TUI |
| W12 | `b5a06c3` | Failure suites, scale, real corpus, performance, hygiene |
| Upstream releases | `3eea4d2` | Fixed library releases adopted, DUES workarounds removed |
| W13 | this commit | Reports and documentation, and the defects found while writing them |

Companion documents: `docs/DEPENDENCY_UPGRADE_REPORT.md` (every module, old and
new version, API and behaviour changes), `docs/TESTING.md` (how to run and
extend the tests), `docs/OPERATIONS.md` (logs, crash files, signals, exit
codes, read-only media, recovery).

## Summary

The pass found 191 defects. Every one is fixed, worked around with a
regression test, or listed below as a residual risk with its bound.

| Severity | Findings |
|---|---|
| CRIT | 9 |
| HIGH | 73 |
| MED | 75 |
| LOW | 34 |
| Total | 191 |

| Status | Findings |
|---|---|
| Fixed | 165 |
| Fixed (code removed) | 15 |
| Fixed upstream | 5 |
| Worked around | 5 |
| Residual | 1 |

The worst of them were on the default path: small files across a chunk
boundary restored at the wrong size (RST-1); every read of an indexed file
failed (ING-19); search attributed hits to the wrong file (SRC-1); `near in`
failed on any database stored with default flags (SRC-12); large partitions
failed to index after five minutes (ING-1). Container evidence (E01, VHD,
VHDX) was indexed through the wrong coordinates (ING-7), and every Hyper-V
snapshot read as empty (PAR-25, PAR-31, PAR-33). Container mode could return
another chunk's bytes without an error (STO-1).

Production readiness: **ready**, on the conditions in
[Readiness assessment](#readiness-assessment). The main ones are to run
`repair --verify` and `repair --upgrade` on databases written before this pass,
and to accept the residual risks listed in [Residual risks](#residual-risks).

## Bug inventory

Each table row is one finding. IDs are stable: tests, skips and commit
messages cite them.

- **Sev** (severity):
  - CRIT: wrong evidence content without an error, stored evidence lost, or the process killed on a default path.
  - HIGH: a crash, hang, loss or wrong result in a supported mode, or an exposure to untrusted input.
  - MED: a wrong or incomplete result that has an error or a workaround, a leak, or an unsafe default.
  - LOW: a misleading message, a minor leak, or a defect with no effect on results.
  - MED-HIGH is counted as HIGH.
- **Defect and where**: what went wrong and the main file at the time.
- **Class**: the root-cause class (see [Root-cause analysis](#root-cause-analysis)).
- **Fix**: the workstream that fixed it, and how.
- **Regression test**: the tests that failed before the fix and pass after it. "(none)" is explained in [Readiness assessment](#readiness-assessment).
- **Status**:
  - Fixed: fixed in DUES.
  - Fixed (code removed): the defective code was deleted, with what replaced it tested.
  - Fixed upstream: the library's own release fixed it, and DUES adopted that release.
  - Worked around: the library defect remains, and DUES avoids it.
  - Residual: accepted, listed in [Residual risks](#residual-risks).

A defect found during the pass, in code the pass itself wrote or changed, is
marked with the workstream that found it, for example "(W12)".

### Configuration, manifest and encryption (CFG)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| CFG-1 | HIGH | Layout settings (`-c`, `-x`, `-i`, `-g`, hash strategy, revrel, simhash, quick) were process globals, never persisted. A mismatched `-c` made recovery fail good evidence and restore write wrong output (`cli/common.go`, `filestruct.go`). | config | W2: manifest fixes the layout at creation; a conflicting flag exits 2. W4: per-evidence chunk size. W13: validation too (STO-26). | `TestLayoutConflicts`, `TestRestoreUsesEachEvidenceChunkSize`, `TestValidateUsesEachEvidenceChunkSize`, `TestRewrittenFilesAvoidLayoutGlobals` | Fixed |
| CFG-2 | HIGH | Quick mode dropped the encryption key and value compression: `--preset quick --password` stored plaintext (`dbio.go`). | config | W2: quick mode only turns off table compression; encryption comes from the manifest. | `TestQuickNeverDropsEncryption` | Fixed |
| CFG-3 | MED | The key was an unsalted single SHA3 of the password; the empty password gave a constant, public key (`util.go`). | trust | W2: Argon2id with a per-database salt, HKDF subkeys and a key check; no password means an unencrypted database with a notice (D8). | `TestKeys`, `TestCreateEncrypted`, `TestCreateUnencrypted` | Fixed; legacy SHA3 databases stay readable (residual) |
| CFG-4 | HIGH | `--low` and the low-resource preset set `NumCompactors=1`, so badger refused to open any database (`dbio.go`). | config | W2: `clamp(threads, 2, 8)`. | `TestConnectDBLowResourceMode` | Fixed |
| CFG-5 | HIGH | `--chonksize` was unvalidated: 0 divided by zero, a negative value panicked, a huge one ran out of memory or deadlocked `SetMaxPendingTxns(0)`. | trust | W2: `appconfig.Resolve` (4 to 65536 KiB, a multiple of 4); `GetMaxBatchCount` clamped to [1, 64] (now `cnst.Tuning.MaxBatchCount`). | `TestResolveRejects`, `TestMaxBatchCountBounded`, `FuzzResolve` | Fixed |
| CFG-6 | MED | Worker, queue and compression-level overrides were unbounded or unvalidated; `--hash-algo` was honoured by store and server only; the TUI changed it per store; the staged pipeline hardcoded BLAKE3. | config | W2: one validated configuration; W3: the staged hasher comes from the layout; W11: the TUI shows the layout read-only. | `TestResolveReportsEveryBadFlag`, `TestStagedPipelineRoundTrip`, `TestMenuShowsTheDatabase` | Fixed |
| CFG-7 | MED | `list` discarded the resolved data root and opened `./graph` in the working directory, creating an empty graph there (`cmdlist.go`). | config | W2/W8: the graph lives under the data root and read commands open it read-only. | `TestReadCommandsCreateNoGraph` | Fixed |
| CFG-8 | MED | The database was not closed on error paths of store, stats, search and micro, and close errors were dropped. | silent | W2: every command closes through `closeDB(db, &err)` after its workers return. | `TestTUICmdClosesTheDatabase`, `TestSignalledCommandExitsWithSignalCode` | Fixed |
| CFG-9 | MED | Value-log GC never ran (`ValueLogMaxEntries=NumGoroutines`); a wrong key gave badger's raw mismatch error. | config | W2: value-log GC after store and `repair --fix`; the override removed; the password is checked before badger opens. | `TestCompactValueLog`, `TestCreateEncrypted` | Fixed |
| CFG-10 | LOW | `InitBatch` leaked its write batch on error; `EnsureBlobPath` had a dead `IsExist` branch (`util.go`). | logic | W2: the batch is made after the count succeeds. W13: the dead branch removed. Pass 7: the count is passed in, so `InitBatch` cannot fail. | `TestMaxBatchCountBounded` | Fixed |
| CFG-11 | MED | Recovery ran on every command, read-only ones included; `repair` recovered before inspecting, so it was not read-only and `--fix` did nothing (`common.go`, `cmdrepair.go`). | silent | W2/W3: recovery only on writable opens; repair inspects `E\|\|\|` records first; `--fix` applies the recovery table. | `TestRepairInspectsEvidenceRecordsOnly`, `TestLegacyReadsChangeNothing` | Fixed |
| CFG-12 | MED | Read commands opened badger read-write and failed on write-protected media; no Windows retry for sharing violations; no orphan-temp sweep; the graph lock always opened read-write. | config | W2: read-only opens with a shared root lock; `fsx` retries; W8: read-only graph lock; W12: the CFG-15 shadow and the full temp sweep. | `TestReadOnlyOpenOnReadOnlyMedia`, `TestReplaceFileRetriesWhileTargetBusy`, `TestWritableOpenSweepsTemps`, `TestReadOnlyMediaDatabase` | Fixed |
| CFG-13 | MED | (W2 regression) A read-only open never recovered a database whose process died: badger wraps `ErrTruncateNeeded` so `errors.Is` fails, and a failed read-only open leaks its memtable mapping on Windows. | upstream | W9: memtable logs present means a writable recovery open (or `ErrNeedsRecovery` when another process holds the lock). | `TestReadOnlyOpenRecoversUncleanDatabase` | Worked around (badger, not filed) |
| CFG-14 | CRIT | (W2 regression) A read-only open of an encrypted database whose newest data key was over 7 days old killed the process: badger rotates the key and `log.Fatalf`s when it cannot write it. | upstream | W10: key rotation is disabled on read-only opens. | `TestReadOnlyOpenOfAnOldEncryptedDatabase` | Worked around (badger, not filed) |
| CFG-15 | HIGH | A database on write-protected media, or with files the user may only read, could not be read: badger opens `DISCARD` and the value logs read-write and `log.Fatalf`s. | upstream | W12: such opens point badger at a temporary shadow directory of links; `dbio.KVDir` names the real directory. | `TestReadOnlyOpenOfUnwritableDatabase`, `TestReadOnlyMediaDatabase` | Worked around (badger, not filed) |
| CFG-16 | HIGH | On Linux a store on a nearly full disk died of SIGBUS: badger extends memory-mapped files sparsely. | upstream | W12: 128 MiB free-space reserve, checked at open and every 16 MiB or 250 ms of an ingest. | `TestCheckKeepsTheReserve`, `TestGuardChecksInSteps`, `TestStoreDiskFullMidway` | Fixed; reserve not absolute (residual) |
| CFG-17 | LOW | (W13) `dbopen.Handle.Close` never unregistered the database's layout, so every closed database stayed reachable from the registry. | unbounded | W13: `Close` calls `dbmeta.UnregisterLayout`. | `TestCloseForgetsTheDatabase` | Fixed |
| CFG-18 | LOW | (W12) A manifest write killed part way left its temporary file in the root forever; the sweep covered only `blob/` and files over an hour old. | integrity | W12: a writable open removes every DUES temporary file in the root, `kvdb/`, `blob/`, `graph/` and `uploads/`. | `TestWritableOpenSweepsTemps`, `TestCrashWritingTheManifest` | Fixed |
| CFG-19 | LOW | (W12) A read-only open of a root holding a manifest but no badger files failed with badger's directory error. | logic | W12: `ErrNoDatabase`, saying creating or resetting did not finish. | `TestReadOnlyOpenOfUnfinishedDatabase` | Fixed |
| CFG-20 | LOW | (W2) Presets were judged by scanning argv: a negated flag (`--no-quick`) was ignored and an operand that looked like a flag counted as one. | logic | W2: whether a flag was set comes from kingpin's parse. | `TestPresetHonoursExplicitFalse`, `TestPresetDefaultsYieldToExplicitFlags` | Fixed |

### Logging, panics and signals (LOG)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| LOG-1 | HIGH | The log was opened `O_TRUNC` in a CWD-relative `log/`; failing to open it was fatal; each start destroyed the last crash's log; two processes corrupted its framing. | silent | W2: per-process NDJSON files, a directory precedence, rotation (50 MB x 10), never fatal. | `TestInitNeverTruncatesEarlierLogs`, `TestInitFallsBackAndNeverFails`, `TestRotationAndRetention` | Fixed |
| LOG-2 | HIGH | No top-level or goroutine panic capture: crashes never reached the log. | silent | W2: `debug.SetCrashOutput` crash files, a top-level recover (exit 70), and `internal/safe` for goroutines and untrusted code. | `TestUnguardedPanicIsCaptured`, `TestCallRecoversPanic`, `TestGoDeliversExactlyOnce` | Fixed |
| LOG-3 | MED | store, restore and repair ignored SIGINT and SIGTERM: temporary files stayed, the database stayed open, containers were not synced. | concurrency | W2: one root context cancelled by signals; exit 130/143. W12: every command interrupted at a fault point. | `TestSignalledCommandExitsWithSignalCode`, `TestInterruptedWrites`, `TestInterruptedReads` | Fixed |
| LOG-4 | LOW | The logger was initialised lazily without synchronisation. | concurrency | W2. | `TestConcurrentInitAndUse` | Fixed |

### Storage integrity (STO)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| STO-1 | CRIT | Container names were `hash("container_%d")` with a per-session index from 0, so each session appended to the same file but recorded offsets from 0. Restore could return another chunk silently: GCM had no AAD binding (`container.go`). | integrity | W3: containers v2, `ctr-<uuidv7>.blob` created `O_EXCL`, framed with CRCs. W2: blob codec v2 binds each chunk to its key. | `TestContainerSessionsNeverShareFile`, `TestV2RejectsMisplacedPayload`, `TestMatrixStoreRestoreRoundTrip` | Fixed; damage already written stays (residual) |
| STO-2 | CRIT | Quick mode's container compression deleted the previous session's `.blob.zst`. | integrity | W3: container compression retired; legacy `.blob.zst` stays readable. | `TestContainerSessionsNeverShareFile` | Fixed |
| STO-3 | HIGH | A short write or ENOSPC left bytes appended without advancing the offset, so every later offset was wrong. | integrity | W3: positioned writes; the offset advances only on a full frame; a failure truncates back and rotates. | `TestContainerFailedWriteKeepsOffsets` | Fixed |
| STO-4 | HIGH | Blobs were written in place without fsync, and `ErrExist` made any existing blob valid forever, so a truncated blob poisoned every later dedup. Quick-mode reads had no integrity check. | integrity | W3: new blobs synced before their metadata; torn blobs replaced atomically; `ChunkStored` checks size. W2: v2 codec verifies every read. | `TestReingestRewritesTruncatedBlob`, `TestWriteChonkCreatesAndReplaces`, `TestV2RejectsDamage` | Fixed |
| STO-5 | HIGH | The write batch committed `C\|\|\|` and relation keys before the blob or container bytes were durable; dedup trusts `C\|\|\|`. | integrity | W3: a durability barrier releases `C\|\|\|` only after the material is synced. | `TestChunkMetaVisibleOnlyAfterSync`, `TestChunkMetaBarrierThresholds` | Fixed |
| STO-6 | HIGH | The block index swallowed read errors and rewrote a bucket without its old records (`blockindex.go`). | silent | W3 (D9): the write path deleted; the read path propagates errors; `repair --upgrade` moves records into `C\|\|\|`. | `TestBlockIndexRejectsDamage`, `TestUpgradeBlockIndexMovesRecords` | Fixed (code removed) |
| STO-7 | HIGH | The block index merged with an unstable sort and kept an arbitrary duplicate. | integrity | W3: write path deleted. | `TestBlockIndexLookup` | Fixed (code removed) |
| STO-8 | HIGH | The block index flushed on almost every chunk (O(N^2)) through a fixed `.tmp` name. | unbounded | W3: write path deleted. | (none) | Fixed (code removed) |
| STO-9 | HIGH | The staged pipeline dropped repeated chunks before relations and hocho ran, so the evidence could not be restored. | integrity | W3: duplicates pass to the writer with `dup: true`. | `TestStagedPipelineRoundTrip` | Fixed |
| STO-10 | HIGH | Chunk and container paths were stored absolute, built from a relative path: databases could not move and could read another case's blobs. | coordinates | W3: new paths are `blob/<name>` under the root; legacy paths resolve by base name inside this root only. | `TestStoreRestoreAfterRelocation`, `TestResolveLegacyBlobPaths` | Fixed |
| STO-11 | HIGH | The async file writer had an unbounded queue, was a process singleton shared by ingests, surfaced errors only at Close and never synced. | unbounded | W3: removed; `--async-fio` is a no-op with a warning. | `TestConfigAsyncFIODeprecated` | Fixed (code removed) |
| STO-12 | HIGH | The legacy container cache decompressed an over-budget container in memory on every read, held a global lock while decompressing, left plaintext temporary files after a crash, and cached nothing under `--low`. | unbounded | W3: one decode per container, spilled to delete-on-close files, bounded count and size. | `TestCompressedContainerDecodedOnce`, `TestCompressedContainerCacheBounded`, `TestCreateTempDeleteOnCloseAfterCrash` | Fixed |
| STO-13 | HIGH | simhash copied each chunk and started its goroutine before taking a slot, so memory grew with the evidence. | unbounded | W3: the slot is taken first. | `TestSimhashWriterBoundsInFlight` | Fixed |
| STO-14 | MED | Reverse-relation buffers grew without bound, and segment read-modify-write lost relations under concurrent ingest. | unbounded | W3: one key per occurrence, written straight to the batch. | `TestRevrelConcurrentIngestsKeepEveryRelation` | Fixed |
| STO-15 | MED | The io_uring EINTR/EAGAIN fallback wrote the data twice. | concurrency | W3: positioned writes; fall back only when the kernel never saw the request. | `TestIoUringWriteNeverDuplicates`, `TestIoUringWriteAtRealRing` | Fixed |
| STO-16 | MED | Allocation sizes came from on-disk counts (containers, block index). | trust | W3: counts bounded by file size before allocating. | `TestBlockIndexRejectsDamage`, `FuzzScanContainer`, `FuzzLookupBlockIndex` | Fixed |
| STO-17 | MED | `GetChonkNode` mapped every error to "not found"; `getSealedChunkSize` re-encoded a whole chunk to learn its size and failed in quick mode. | silent | W3: the locator separates missing from read errors. W4: the size helper deleted; chunk maps use recorded sizes. | `TestReingestFindsChunksInLegacyBlockIndex`, `TestChunkMapBudget` | Fixed |
| STO-18 | HIGH | `dbio.GetNode` copied a value onto itself (`item.ValueCopy(val)` inside `item.Value`). For a value in badger's value log (over 1 MiB) in an unencrypted database, the copy wrote into the log's read-only mapping on a read-only open and killed the process (`unexpected fault address`). Rated LOW at the audit; raised in W13. | logic | W13: `item.ValueCopy(nil)`. | `TestGetNodeOwnsItsValue` | Fixed |
| STO-19 | MED | Blobs and containers were created 0644 and data-root directories 0755, so other local users could read evidence content (`const.go`). | trust | W13: directories 0700, blobs and containers 0600 (Unix). | `TestStoredFilesArePrivate`, `TestNewDataRootIsPrivate` | Fixed; existing roots keep their modes (residual) |
| STO-20 | LOW | Legacy sealed blobs use a GCM nonce derived from the key. | trust | Read-only compatibility; new encrypted databases use the v2 codec with a random nonce. | (none) | Residual |
| STO-21 | HIGH | Hierarchical mode wrote no `C\|\|\|` key: cross-session dedup always missed, and FLUSHED validation always failed. | integrity | W3 (D9): BIDX retired; the locator reads legacy block indexes. | `TestReingestFindsChunksInLegacyBlockIndex` | Fixed |
| STO-22 | MED | EH, PH and IH lookups were read-modify-write lists: O(n^2) for popular hashes, conflicting under concurrency. | unbounded | W3: one key per member (`HM\|\|\|`). | `TestHashLookupAppendsConcurrently`, `TestPopularHashLookup` | Fixed |
| STO-23 | MED | Re-ingest compressed and encrypted every first-seen chunk even when its blob existed. | unbounded | W3: `C\|\|\|` is checked before materialising. | `TestReingestSkipsStoredChunks` | Fixed |
| STO-24 | MED | `stats` failed on any database stored with `--revrel` and held every reverse relation in a map. | unbounded | W11: streaming count over every key form. | `TestStatsWithReverseRelations`, `TestCountSharedChunksReadsEveryForm` | Fixed |
| STO-25 | HIGH | msgpack preallocated slices at their declared length (its cap check compares a flag with 1). Five bytes in any record asked for gigabytes and the OOM killer ended the process; a crafted `.duesarc` could kill an FFI host. | upstream | W12: `internal/msgpackx` checks every value before decoding; all 18 decodes go through it. | `TestUnmarshalBoundsAllocation`, `TestDecodesGoThroughMsgpackx`, `FuzzCheck`, `FuzzDecodeRecord` | Worked around (msgpack, not filed) |
| STO-26 | MED | (W13) `ValidateEvidence` stepped at the database's chunk size, not each evidence's: in a legacy database with mixed `-c`, recovery failed good FLUSHED evidence and `repair --verify` missed damage. | config | W13: `dbio.EvidenceChunkSize`. | `TestValidateUsesEachEvidenceChunkSize` | Fixed |
| STO-27 | MED | (W13) Validation reported a legacy compressed container (`.blob.zst`) missing, so recovery could fail good evidence and `repair --verify` reported it damaged. | integrity | W13: `fio.ContainerHolds` resolves the compressed sibling. | `TestValidateCompressedLegacyContainer` | Fixed |
| STO-28 | MED | (W6) Two writers of one new blob in one process collided: the second took the first's open file for a torn blob and renamed over it, which Windows refuses. | concurrency | W6: writers of one blob take turns. | `TestWriteChonkWaitsForAnOpenWriter`, `TestWriteChonkConcurrentSameChunk` | Fixed |

### Restore and read path (RST)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| RST-1 | HIGH | `trimLogicalChunkData` sliced without bounds checks: workers panicked, and a small file straddling a chunk boundary restored at the wrong size. Search, near and chunk maps used it too. | coordinates | W3: `dbio.Intersect` and a stride check. W4: one reader for every consumer. | `TestRestoreLogicalRangeAcrossChunkBoundaries`, `TestIntersect`, `TestChunkBoundaryMatrix` | Fixed |
| RST-2 | HIGH | Restore strode by the current process's chunk size. | config | W4: each evidence's own chunk size. | `TestRestoreUsesEachEvidenceChunkSize` | Fixed |
| RST-3 | HIGH | Restored output was never verified, per chunk or whole. | integrity | W4: every chunk checked; the whole content checked against its recorded hash (`--verify full` default). | `TestRestoreRejectsSubstitutedChunk`, `TestRestoreVerifiesRecordedHash` | Fixed |
| RST-4 | MED | Restore replaced the target silently, deleted an unrelated `<target>.bak`, renamed a directory target, deleted the finished file on a Windows rename failure, and left temporary files on Ctrl-C. | destructive | W3: atomic write beside the target; existing file refused without `--force`; directories refused; `.bak` never touched. | `TestRestoreTargetSafety` | Fixed |
| RST-5 | MED | Restore held one read transaction for its whole run, had no backpressure, and could not be cancelled. | unbounded | W3: bounded reorder window, per-batch transactions, context. | `TestRestoreReorderWindowIsBounded` | Fixed |
| RST-6 | MED | `[:16]` slicing panicked on short hashes, reachable from the server. | trust | W9: the slicing is gone; the server recovers and sanitises errors. | `TestRPCErrorsAreSanitised`, `TestPanicBecomes500` | Fixed |
| RST-7 | LOW | (W12) A restore or archive write killed part way left `.<target>.dues-tmp-*` beside the target forever. | integrity | W12: a write first removes what killed writes to the same path left. | `TestWriteAtomicRemovesStaleTemps`, `TestCrashDuringRestore`, `TestCrashWritingAnArchive` | Fixed |

### Ingest lifecycle and indexing (ING)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| ING-1 | HIGH | Partition indexing had a hard 5-minute timeout; on timeout its goroutine leaked and kept writing, so large partitions failed predictably (`cmdstore.go`). | concurrency | W6: a synchronous index pass under the command's context (`index.Run`). | `TestIndexFailureKeepsStorage`, `TestStorageFailureResumes` | Fixed (code removed) |
| ING-2 | HIGH | The indexer sent its error twice: a partial index was committed after the failure and the second send blocked forever. | concurrency | W3: one send; W6: the legacy index path deleted. | `TestIndexFailureKeepsStorage`, `TestInterruptedPassIsDeletable` | Fixed (code removed) |
| ING-3 | MED | `go store.Store` ran under a 2-minute timeout on an unbuffered channel and leaked. | concurrency | W3: `StoreInputFile(ctx) error`; W6: `ingest.Run`. | `TestStorageFailureResumes` | Fixed (code removed) |
| ING-4 | HIGH | Evidence was marked COMPLETED before indexing ended, and no index state was kept; an unsupported filesystem counted as success. | silent | W6: `IndexInfo` state and coverage on every evidence; an incomplete index exits 2. | `TestStoreRecordsIndexState`, `TestIndexFailureKeepsStorage`, `TestRecoveryInterruptsIndexPass` | Fixed |
| ING-5 | MED | Cancelling leaked the store goroutine; the evidence mapping and handle were released only on success; the database stayed open on error. | concurrency | W3: release in a defer, wait on cancel; W6: no mapping at all. | `TestStoreReleasesEvidenceFile`, `TestSignalledCommandExitsWithSignalCode` | Fixed |
| ING-6 | HIGH | Re-ingest was not idempotent: fresh UUIDs meant the pre-store check never matched, duplicating records and search hits. | integrity | W6 (D1): one commit transaction; identical content becomes an alias. | `TestReingestIsAnAlias`, `TestCommitAliasesStoredContent`, `TestConcurrentIngestsHaveOneWinner` | Fixed |
| ING-7 | CRIT | E01/VHD/VHDX: offsets in the decoded image were applied to the raw container file, so hashes and types were computed over compressed bytes, or indexing aborted. | coordinates | W6 (D5): evidence decoded at ingest and stored as the disk image; `repair --upgrade` quarantines old indexes. | `TestOpenDecodesContainers`, `TestQuarantineRawContainerIndexes`, `TestIngestFixtures` | Fixed; old indexes quarantined |
| ING-8 | MED-HIGH | Lost update: the completion transition wrote a stale snapshot over the partition map, and `ErrConflict` was never retried. | concurrency | W3: every evidence write is one retried read-modify-write transaction. | `TestIngestKeepsPartitionLinks`, `TestUpdateEvidenceConcurrentWritersKeepEveryChange`, `TestEvidenceWritesUseUpdateEvidence` | Fixed |
| ING-9 | MED-HIGH | When the file hash failed after FLUSHED, recovery completed the evidence with an empty hash and no lookup entry; recovery also completed FLUSHED evidence without entering its identity. | integrity | W6: recovery validates, then runs the same commit as ingest. | `TestRecoveryCommitsFlushedAttempts` | Fixed |
| ING-10 | MED | Recovery failed evidence on transient errors; validation buffered every hash and checked only metadata; outside quick mode it failed every real FLUSHED ingest (relation values are compressed). | silent | W3: validation v2 (locations, material, content), streaming; only known-missing material fails an attempt. | `TestRecoveryCompletesFlushedEvidenceInEveryMode`, `TestRecoveryKeepsFlushedEvidenceOnTransientError`, `TestValidateEvidenceLevels`, `TestValidateFourMillionChunks` | Fixed |
| ING-11 | MED | Zero-byte files could not be mapped and aborted a folder ingest; a FIFO blocked; a failing source drive raised SIGBUS; one error aborted the whole folder. | silent | W3: regular files only, empty files not mapped, folders continue with a summary. W6: no mapping (`ReadAt`). | `TestStoreZeroByteFile`, `TestStoreFolderContinuesPastErrors`, `TestStoreReleasesEvidenceFile` | Fixed |
| ING-12 | MED | The evidence hash came from a separate read, so a file changing mid-ingest was COMPLETED with a hash of other bytes. | integrity | W6: an ordered digest tap over the bytes ingested. | `TestChunkFeedDigests` | Fixed |
| ING-13 | MED | A partition record held every member's offset in one map, which does not scale (see ING-21). | unbounded | W3: one key per member (`PX\|\|\|`). | `TestPartitionMembersOutsideRecord`, `TestMillionPartitionMembers` | Fixed |
| ING-14 | MED | Hocho reuse with async indexing gave non-deterministic partition hashes. | concurrency | W6: logical hocho reuse deleted; partition hashes from the digest tap. | `TestReindexRebuildsTheSameIndex`, `TestIngestKeepsPartitionLinks` | Fixed (code removed) |
| ING-15 | MED | A `tskcompat.Analyze` failure was only logged, and the fallback indexed nothing. | silent | W5/W6: the scan reports every partition with a status, and the index state records it. | `TestScanAnnouncesEveryPartition`, `TestStoreRecordsIndexState` | Fixed |
| ING-16 | HIGH | `dues reset` deleted any non-empty directory after one y/N. | destructive | W3: only a DUES root, under its lock; refuses home, root and CWD; deletes only DUES entries; typed confirmation or `--yes`. | `TestResetRefusals`, `TestResetDeletesOnlyDUESEntries`, `TestResetDataConfirmation` | Fixed |
| ING-17 | LOW | `uuid.NewV7` errors and one helper panicked. | logic | W6/W12: errors returned; the code paths deleted. | (none) | Fixed |
| ING-18 | LOW | On a store error, `drainIndexError` waited for indexing to finish completely. | concurrency | W6: a storage failure cancels the index pass. | `TestStorageFailureResumes` | Fixed (code removed) |
| ING-19 | CRIT | Indexed-file names were prefixed with the raw 16-byte evidence UUID but parsed as base64, so every read of an indexed file failed: restore, near, micro, entropy and FTS content. | coordinates | W3: tolerant parse. W6: explicit `EvidenceID`/`PartitionID` on every record. | `TestGetEvidenceFileHashAcceptsRawPrefix`, `TestReaderServesFixtures`, `TestIngestFixtures` | Fixed |
| ING-20 | MED | Logical hocho hashes depended on where a file sat relative to chunk boundaries, so identical files got different hashes. | coordinates | W6: partitions and indexed files always get plain content hashes. | `TestRunIndexesFixtures` | Fixed |
| ING-21 | HIGH | A partition record over badger's 64 MiB value limit (about 1M files) could not be written, so very large volumes failed to index. | unbounded | W3: members outside the record. | `TestPartitionMembersOutsideRecord`, `TestMillionPartitionMembers` | Fixed |
| ING-22 | LOW | (W13) A server upload's attempt key (`EP\|\|\|`) was written under the part file's path and cleared under the client's, so it was never cleared. | coordinates | W13: the attempt is keyed by the recorded source path. | `TestCommitClearsAttemptKey` | Fixed |
| ING-23 | MED | (W12) A reset killed part way left a database whose blobs were gone: entries were removed in directory order. | destructive | W12: records first, the manifest last. | `TestResetRemovesRecordsFirst`, `TestCrashDuringReset` | Fixed |
| ING-24 | MED | (W12) Recovery and `repair --fix` validated with `context.Background()`, so Ctrl-C did nothing until every chunk was checked. | concurrency | W12: recovery takes the command's context and stops between records. | `TestRecoveryStopsWhenCancelled` | Fixed |
| ING-25 | MED | (W12) `dues list` was quadratic in the number of evidence: it scanned the database for legacy partitions once per evidence (9,000 files took over five minutes). | unbounded | W12: records written since W6 are not scanned; older ones share one scan. | `TestListScansForLegacyPartitionsOnce` | Fixed |
| ING-26 | LOW | (W12) `list` showed no index state and listed a quarantined index's partitions: the W8 rewrite dropped the W6 change. | silent | W12: `listOutput` shows the state and hides quarantined partitions. | `TestQuarantineRawContainerIndexes` | Fixed |
| ING-27 | LOW | (W3) A progress-bar error ("max must be greater than 0") failed store, restore, search and near on empty input. | silent | W3: progress errors never fail an operation. | `TestStoreZeroByteFile` | Fixed |

### Parsers and index completeness (PAR)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| PAR-1 | HIGH | Only NTFS recovered from panics; every other parser call, and the PE/ELF/PDF/EVTX/registry parsers, could kill the process. | trust | W5: `internal/safe` around every adapter call, read, table parse and image open; W8: micro parsers. | `TestGuardContainsPanics`, `TestDetectContainsPanics`, `TestExtractContainsParserPanics` | Fixed |
| PAR-2 | HIGH | The scan dropped data silently: a filesystem open error dropped the partition, a Stat failure the file; run errors were ignored; unknown and encrypted partitions were never registered. | silent | W5: every table entry announced with a status; failures emitted with issues. | `TestScanAnnouncesEveryPartition`, `TestScanReportsCounts` | Fixed |
| PAR-3 | HIGH | Files with no fragments (resident, inline, compressed, empty) were never indexed, fragmented files were skipped, and exFAT omitted all fragmented files. | silent | W5: content contract; W6 (D2): extents, inline and materialised content. | `TestRunIndexesFixtures`, `TestIngestFixtures`, `TestFixtureInvariants` | Fixed |
| PAR-4 | HIGH | XFS content included block slack, and holes collapsed. | coordinates | W5: runs with file offsets, normalised and cut to the size. | `TestNormalizeExtents`, `TestFixtureInvariants` | Fixed |
| PAR-5 | HIGH | ext treated unwritten (preallocated) extents as data. | coordinates | W5. | `TestFixtureInvariants` | Fixed |
| PAR-6 | HIGH | Deleted entries were given the data of records reused since (NTFS without sequence checks; ext, XFS and exFAT likewise). | coordinates | W5: sequence and allocation checks; stale entries are metadata only. | `TestFixtureDeletedRecovery` | Fixed; an ext4 deleted file is unrecoverable (residual) |
| PAR-6a | HIGH | Deleted NTFS files were never recovered: libntfs refuses records not in use. | silent | W5: MFT record, sequence check, attribute runs and `$Bitmap` reallocation check. | `TestFixtureDeletedRecovery` | Fixed |
| PAR-7 | HIGH | exFAT directory cycles caused a stack overflow or OOM; cluster lists were sized from claimed geometry. | trust | W5: cycle-safe, depth-capped walk; no unbounded cluster lists. | `TestNoUnboundedClusterLists`, `TestCorruptImageDirectoryCycle`, `FuzzScanExFAT` | Fixed |
| PAR-8 | HIGH | `registerIndexedRange` overflowed int64, and one bad fragment aborted the evidence. | trust | W5: overflow-safe; W6: replaced by extent validation. | `FuzzNormalizeExtents`, `TestContentValidate` | Fixed (code removed) |
| PAR-9 | HIGH | VHD parent resolution opened host, UNC or `..` paths read from the evidence. | trust | W1: sibling-only resolver, base names only, fail closed. | `TestSiblingResolverStaysInChildDir`, `TestSiblingResolverRejectsUnsafeNames`, `TestOpenVHDFileFailsClosed` | Fixed |
| PAR-10 | MED | tskcompat built the whole image listing as JSON and re-parsed it for every partition. | unbounded | W5/W6: streaming scan; tskcompat is a debug renderer only. | `TestScanReportsCounts` | Fixed (code removed) |
| PAR-11 | MED | Walker recursion was unbounded, and path memory grew with depth squared. | unbounded | W5: explicit-stack tree walker with depth and entry caps. | `TestTreeWalkerDeepTree`, `TestTreeWalkerEntryBudget`, `TestTreeWalkerCycle` | Fixed |
| PAR-12 | MED | Offset arithmetic could wrap in several adapters and the partition code. | trust | W5: overflow-safe extents; adoption: libtable `ByteRange`. | `TestNormalizeExtentsRejects`, `FuzzNormalizeExtents`, `TestConvertRefusesUnaddressableRanges` | Fixed |
| PAR-13 | MED | The MBR fallback multiplied byte values by 512 again, so it never found a partition. | coordinates | W1: fixed; W6: the fallback deleted (the scan uses libtable). | `TestParseMBR` | Fixed (code removed) |
| PAR-14 | MED | Legacy `IndexEXFAT`: cluster-0 entries wrapped, and one zero-length entry aborted the partition. | coordinates | W1: cluster errors skip the entry; W6: deleted. | (none) | Fixed (code removed) |
| PAR-15 | MED | Adapters skipped subtrees and content silently. | silent | W5: unreadable and partial directories reported; adoption: libfat/libxfat `OnSkip`. | `TestTreeWalkerUnreadableAndPartial`, `TestWalkReportsSkippedDirectories` | Fixed |
| PAR-16 | LOW | `fs.Detect` ignored I/O errors. | silent | W5: `DetectErr`. | `TestDetectReadError` | Fixed |
| PAR-17 | MED | libtable warnings were ignored, and partitions were not checked against the image size. | silent | W5: warnings reported, partitions cut to the image. | `TestParseClampsToImage`, `TestParseReportsTableWarnings` | Fixed |
| PAR-18 | MED | EWF chunk checksum failures were ignored. | silent | W5/W6: stored and computed digests, chunk-table failures and bad ranges on the evidence; a mismatch exits 2; `--require-verified`. | `TestIngestChunksVerifiesEWF`, `TestEWFDigestMismatch` | Fixed |
| PAR-19 | MED | exFAT checksum-mismatch and placeholder names were indexed as real; FAT virtual entries appeared as files and orphans were not walked. | silent | W5: provenance flags. | `TestFixtureInvariants` | Fixed |
| PAR-20 | MED | NTFS compressed streams were not handled. | silent | W5/W6 (D10): decoded and materialised. | `TestReaderContentKinds`, `TestDecoderMatchesLibntfs` | Fixed |
| PAR-21 | MED | Invalid UTF-8 names became U+FFFD, so distinct names collided. | coordinates | W5: percent-escaped names, raw name kept. | `TestEscapeName`, `FuzzEscapeName` | Fixed |
| PAR-22 | MED | Two partitions with the same start both indexed the first one's files. | coordinates | W6: partitions by table slot with deterministic IDs. | `TestRunIndexesFixtures` | Fixed (code removed) |
| PAR-23 | LOW | EWF segment discovery accepted any `.E??` sibling, such as `case.Exe`. | trust | W5: `libewf.SegmentPaths`. | `TestOpenSourceEWF` | Fixed |
| PAR-24 | LOW | Partition names were printed raw, allowing terminal-escape injection. | trust | W2/W6/W11: `termsafe` on every evidence-derived string. | `TestStringEscapesControls`, `FuzzString`, `TestEvidenceStringsAreMadeSafe` | Fixed |
| PAR-25 | CRIT | libvhdi v0.2.0 reported a false parent cycle for every Hyper-V `.avhdx`, and DUES ignored the error, so every snapshot leaf read as 1 unknown partition and 0 files. | upstream | W1: libvhdi upgraded; an incomplete chain is an open error. | `TestOpenVHDFileResolvesSiblingChain`, `TestOpenVHDFileFailsClosed` | Fixed upstream |
| PAR-26 | HIGH | btrfs, classic HFS, an HFS wrapper and a FAT32 with only its backup boot sector were dropped silently. | silent | W5: detected and opened, or reported unsupported. | `TestScanAnnouncesEveryPartition` | Fixed |
| PAR-27 | HIGH | 4,842 NTFS files of jean.e01 failed with LZNT1 errors: libntfs took the compression-unit exponent as a cluster count. | upstream | W5: DUES decodes LZNT1 itself; libntfs v0.3.6 fixed it (adopted). | `TestDecoderMatchesLibntfs`, `TestLZNT1`, `FuzzLZNT1` | Fixed upstream; DUES decoder kept |
| PAR-28 | HIGH | 3,394 fragmented NTFS files had a runs digest different from their `Open` digest: the runs read compressed clusters as content. | coordinates | W5: same root cause as PAR-27. | `TestDecoderMatchesLibntfs` | Fixed |
| PAR-29 | HIGH | FAT deleted files were emitted with wrong content. | silent | W5: assumed contiguous, flagged `assumed`. | `TestFixtureDeletedRecovery` | Fixed |
| PAR-30 | HIGH | ext symlinks were indexed as files whose content is the target path. | logic | W5: `KindSymlink`. | `TestFixtureInvariants` | Fixed |
| PAR-31 | CRIT | libvhdi served an unaligned read in a partially present VHDX block from the start of its sector: the ubuntu snapshot leaf read 0 files, without error. | upstream | W1: workaround; libvhdi v0.4.2 fixed it, workaround removed. | `TestOpenVHDFileReadsUnalignedInPartialBlocks` | Fixed upstream |
| PAR-32 | HIGH | A source device lost mid-scan became about 177,000 per-file errors, and the scan "succeeded". | silent | W5: a source read error ends the partition; W6: it fails the ingest. | `TestScanSourceReadErrorIsFatal`, `TestIngestChunksSourceReadError` | Fixed |
| PAR-33 | CRIT | libvhdi misread a differencing VHDX's last chunk when the disk ends inside it: the snapshot's newest sectors there were replaced by older ones (the last 3 GiB of a 127 GiB disk). | upstream | W5: workaround; libvhdi v0.4.2 fixed it, workaround removed. | `TestOpenVHDFileReadsPartialLastChunk` | Fixed upstream |
| PAR-34 | HIGH | The NTFS MFT sweep sized a set from a forged record count: one flipped byte asked for 378 GB and killed the process. | trust | W5: the count is cut to what the partition can hold. | `FuzzScanNTFS` | Fixed |
| PAR-35 | HIGH | libext allocated an inline-data inode's claimed size before reading it: one flipped bit asked for 192 GiB. | upstream | W10: workaround; libext v0.4.2 fixed it, workaround removed. | `TestForgedInlineSizeIsRefused`, `FuzzScanExt` | Fixed upstream |

### Graph, enrichment and micro-artefacts (GRA)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| GRA-1 | HIGH | micro named files by a sha256 hex ID while enrichment wrote base64 record IDs, so micro extracted nothing and exited 0. | coordinates | W8: one identity, the record's own ID. | `TestMicroExtract` | Fixed |
| GRA-2 | MED | `Upsert` never updated an existing node, so evidence and partition nodes stayed at their stubs; index work was deferred outside the transaction. | silent | W8: full records merge and replace their index keys. | `TestUpsertReplacesStoredNode`, `TestUpsertFillLeavesExistingNode` | Fixed |
| GRA-3 | MED | Index entries stayed unsynced until the next commit, Close errors were dropped, and re-runs created duplicates. | integrity | W8: index entries inside the transaction; Close errors propagate. | `TestIndexEntriesCommitWithTheBatch`, `TestRepeatedUpsertWritesNothing` | Fixed |
| GRA-4 | MED | One failed batch index operation skipped the rest. | silent | W8. | `TestIndexEntriesCommitWithTheBatch` | Fixed |
| GRA-5 | MED | Read-only views opened graphene writable and stamped the marker. | config | W8: `OpenReadOnly`; a missing store is `ErrNoGraph`. | `TestReadOnlyOpenOfMissingStoreCreatesNothing`, `TestReadCommandsCreateNoGraph` | Fixed |
| GRA-6 | MED | Every open replayed the whole log under a global lock. | unbounded | W8: opens outside the lock; a warning past 1 GiB or 5M records. | `TestSlowOpenDoesNotBlockOtherStores`, `TestLargeLogIsReported` | Fixed; WAL-only by design (D6) |
| GRA-7 | LOW | The marker was written without fsync through a fixed `.tmp`, so a crash could leave an empty marker that refused every open. | integrity | W8: atomic write; an empty marker counts as absent. | `TestEmptyMarkerCountsAsAbsent` | Fixed |
| GRA-8 | LOW | A shared lock holder deleted other holders' info; the registry key ignored symlinks and Windows case. | concurrency | W8: per-holder info files; resolved, case-folded keys. | `TestSharedHolderKeepsOtherHoldersInfo`, `TestRegistryKeyFoldsSpellings` | Fixed |
| GRA-9 | MED | A benign store without a marker was refused, and errors advised a command that does not exist. | logic | W8. | `TestMarkerlessStoreOfBuiltinKindsOpens`, `TestNoAdviceToRunMissingCommands` | Fixed |
| GRA-10 | LOW | `props.Unpack` hid msgpack errors; writes to a nil graph were silent no-ops. | silent | W8. | `TestUnpack_NeverNil`, `TestClosedRepositoryRefusesWrites` | Fixed |
| GRA-11 | LOW | Hard-coded versions in the graph stamps. | logic | W8: from build info. | `TestStampsNameLinkedGraphene` | Fixed |
| GRA-12 | MED | `micro --force` did nothing; `artefact_count` was never written; enrichment re-ran per file; failures exited 0; `graph.html` went into the database directory. | silent | W8: replace on `--force`, counts in the same transaction, exit 2 on failures, `--out` refusing to overwrite. | `TestMicroExtract` | Fixed |
| GRA-13 | MED | The PE detector received text, so it never fired; the registry detector was dead code; PE section slicing could panic. | logic | W8: raw bytes for format detectors, hive recognition, clamped sections. | `TestServicePEDetectorGetsRawBytes`, `TestPESectionPastEndDoesNotPanic`, `TestParseRecognisesRegistryHives`, `TestRegistryDetectorWalksAHive` | Fixed |
| GRA-14 | HIGH | PDF decompression bomb. | trust | W8: a pre-scan bounds inflation (16 MiB per stream, 64 MiB total, ratio 200). | `TestPDFInflationIsBounded` | Fixed |
| GRA-15 | HIGH | The PE and ELF parsers never closed their files: handles and mappings leaked, and temporary copies of evidence bytes stayed in `%TEMP%`. | integrity | W8: parsed from memory; no temporary files. | `TestServicePEDetectorGetsRawBytes`, `TestPEMetadataDetectorParsesRawPE` | Fixed |
| GRA-16 | MED | Content was cut to 4 MiB before parsing, so larger binaries and PDFs failed and looked like files with no artefacts. | silent | W8: 64 MiB for parsers; outcomes `parse_failed` and `too_large`. | `TestServiceOutcomes`, `TestParseFailureKeepsStrings` | Fixed |
| GRA-17 | MED | Memory blow-ups: log-line capacity from the line count, pairwise relations, whole-graph loads. | unbounded | W8/W11/W12: bounded detectors, star relations, top-N views. | `TestLogLineDetectorKeepsAtMostMax`, `TestRelationEdgesAreBounded`, `TestTopFiles`, `TestMicroExtractPrintsTopFiles` | Fixed |
| GRA-18 | LOW | Timestamp underflow, UTF-8 splitting, in-place spec filters, a missing nil guard. | logic | W8. | `TestFiletimeBeforeEpoch`, `TestTruncateKeepsRunes`, `TestSpecFiltersCopyOnWrite` | Fixed |
| GRA-19 | LOW | Entropy read errors showed as "not computed", and enrichment re-read the evidence about three times. | silent | W8: entropy computed by the index pass; read errors counted, exit 2. | `TestEnrichmentGraph`, `TestEnrichAllCreatesFileNodesAtAllLevels`, `TestEntropy` | Fixed |
| GRA-20 | MED | Store-time enrichment keyed evidence by the raw ID bytes, enrichment by base64, so graph identities did not match. | coordinates | W8: base64 everywhere; the old path deleted. | `TestEnrichmentGraph` | Fixed |
| GRA-21 | LOW | A read-only graph open took graphene's exclusive lock, so two readers excluded each other. | concurrency | W8: graphene's shared lock. | `TestReadOnlyOpensShareTheStore` | Fixed |
| GRA-22 | MED | saferwall/pe `Close` unmaps the data even for a file made from caller memory. | upstream | W8: such files are never closed. W13: the comment corrected (a heap buffer is not unmapped). | (none) | Worked around (saferwall, not filed) |
| GRA-23 | LOW | pdfcpu created a configuration directory under the user's config directory. | logic | W8: `DisableConfigDir`. | (none) | Fixed |
| GRA-24 | MED | With a graph present, `list` listed only the evidence the graph held. | silent | W8: the database is authoritative; the graph only annotates. | `TestListShowsEvidenceTheGraphLacks` | Fixed |
| GRA-25 | LOW | `micro list`, the extract summary and the TUI micro view named no file. | logic | W11: names read from the file node. | `TestTopFiles` | Fixed |
| GRA-26 | LOW | (W12) Registry strings kept their NUL terminator, and a DWORD or QWORD of 0 was reported empty. | logic | W12. | `TestRegistryDetectorWalksAHive` | Fixed |

### Search, NeAR and full-text search (SRC)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| SRC-1 | HIGH | The count cache was keyed by chunk only while the state was trimmed per file, so hits were attributed to the wrong file. | coordinates | W7: streaming matcher; cache keyed by chunk and span. | `TestSearchCountsAcrossChunkBoundaries`, `TestMatcherStreamsAcrossCuts` | Fixed |
| SRC-2 | HIGH | The chunk-boundary window underflowed on a long query (remotely triggerable panic), missed matches across boundaries, and read past EOF. | trust | W7: the matcher carries state across spans; query caps. | `TestSearchLongQueryAcrossChunks`, `TestSearchCountsAcrossChunkBoundaries`, `TestValidateTermsCapsQueries`, `FuzzStreamCount` | Fixed |
| SRC-3 | HIGH | FTS-assisted search was silently incomplete: 5,000 candidates, indexed files only, first 128 KiB, a different tokeniser, the raw query parsed as bleve syntax. | silent | W7: coverage reported; literal terms. | `TestFullTextModeReportsCoverage`, `TestSearchCandidatesTreatsTermsLiterally` | Fixed; FTS limits reported (residual) |
| SRC-4 | HIGH | `near out` unmapped memory while workers ran (fatal), leaked its producer and workers, copied the whole file and never closed it. | concurrency | W7: sequential read, no mapping, worker groups. | `TestNearOutReadsTheFile`, `TestFileSimHashInPieces` | Fixed |
| SRC-5 | HIGH | NeAR cost blew up: every common chunk rescanned all relations, and every matched chunk re-decoded the partition record. | unbounded | W7: each distinct chunk looked up once; very common chunks skipped and counted; interval index. | `TestNearSkipsCommonChunks`, `TestNearAttributesToFiles` | Fixed |
| SRC-6 | MED | `near in` leaked goroutines on early return; global result stores were reset on every call. | concurrency | W7: per-call state. | `TestNearRunsAreIndependent` | Fixed |
| SRC-7 | MED | bleve opened without a bolt timeout and could hang; the index was opened per call; FTS errors were swallowed; non-UTF-8 chunks dropped; a search built an index. | silent | W7: one shared handle, timeouts, counted errors, read-only search. | `TestIndexHeldElsewhereTimesOut`, `TestConcurrentUseSharesOneIndex`, `TestIndexFilesCountsUnreadableText`, `TestTextExtractorAcrossPieces`, `TestReadOnlyDatabaseSearchesWithoutWriting` | Fixed |
| SRC-8 | MED | A `\|\|\|` in a file name broke reports. | coordinates | W7: names from the record helpers. | `TestSearchReportNamesObjects` | Fixed |
| SRC-9 | MED | Occurrences were counted three times; a stale size was reused; confidence was unclamped. | logic | W7: counts per level; confidence is a share. | `TestSearchCountsAcrossChunkBoundaries`, `TestNearConfidenceIsAShare` | Fixed |
| SRC-10 | LOW | Cancellation returned without waiting for workers; nested semaphores allowed W+W^2 in flight; `ConcMap.GetData` returned the map unlocked. | concurrency | W7: one worker group per phase; `ConcMap` deleted. | `TestSearchReturnsCanceled`, `TestNearRunsAreIndependent` | Fixed |
| SRC-11 | LOW | Reports overwrote files in the working directory; NaN `--rank-alpha` was accepted; an empty last chunk was emitted; a hint named a retired flag. | destructive | W2/W7: `--out` refuses an existing file without `--force`. | `TestSearchReportRefusesExistingFile`, `TestNearReportRefusesExistingFile`, `TestNearOutReadsTheFile`, `TestResolveRejects` | Fixed |
| SRC-12 | HIGH | On a database stored without `--revrel`, `near in` failed at once with a bare "Key not found". | silent | W7: capability check, actionable error, `repair --fix --revrel`. | `TestNearNeedsReverseRelations`, `TestNearCommand` | Fixed |
| SRC-13 | LOW | (W13) Full-text search re-sorted its results ignoring `--rank-alpha`. | logic | W13. | `TestFTSBoostKeepsRankAlpha` | Fixed |

### Server and API (SRV)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| SRV-1 | HIGH | The server listened on every interface without auth, reflected any CORS origin, had no timeouts, used deprecated h2c, and had no message-size limit. | trust | W1/W9 (D3): loopback by default, bearer token for other addresses, CORS allowlist, Host check, timeouts and limits. | `TestDefaultAddressesAreLoopback`, `TestBearerToken`, `TestCORS`, `TestHostCheckDefeatsRebinding`, `TestSlowHeadersAreCut`, `TestRPCMessageLimit` | Fixed |
| SRV-2 | HIGH | Upload sessions: TTL from creation, no lock or offset, no size limits or session cap, finalize racing chunks, the reaper started in `init()`. | concurrency | W9: upload v2 manager with offsets, limits and one finalize. | `TestUploadV2`, `TestUploadLimits`, `TestUploadFinalizeOnce`, `TestUploadExpiry` | Fixed |
| SRV-3 | HIGH | Finalize looked evidence up as `E\|\|\|:<content hash>`, invalid since evidence keys are UUIDv7: every upload stored the file and then failed, leaving an orphan; the client's hash was never checked. | coordinates | W9: the hash is verified server-side and the ingest returns the evidence ID. | `TestUploadHashMismatchStoresNothing`, `TestUploadV2`, `TestUploadOfStoredContent` | Fixed |
| SRV-4 | MED | `StreamFile` returned no error when Close failed and leaked descriptors and partial files when a client disconnected. | silent | W9. | `TestStreamFile` | Fixed |
| SRV-5 | MED | `AppendIfExists` let any client rename any evidence. | trust | W9: records an alias, never renames. | `TestAppendIfExistsRecordsAlias` | Fixed |
| SRV-6 | MED | Listing responses were unbounded and ignored ctx, and raw internal errors went to clients. | unbounded | W9: paging, budgets, sanitised errors with request IDs. | `TestGetEviFilesPages`, `TestChunkMapBudget`, `TestRPCErrorsAreSanitised` | Fixed |
| SRV-7 | HIGH | Shutdown closed the database under in-flight work. | concurrency | W9: drain, cancel, wait; work still running leaves the database for badger's recovery. | `TestShutdownCancelsRequests`, `TestShutdownWithStuckWorkIsUnclean`, `TestShutdownWaitsForWork` | Fixed |
| SRV-8 | LOW | Handlers used the global `cnst.DB`, nil outside the server. | config | W9: `cnst.DB` deleted. | (none) | Fixed (code removed) |

### FFI and the compression product (FFI, CMP)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| FFI-1 | HIGH | No recover in `DuesDispatchJSON` or the async tasks: a panic killed the host process. | trust | W10: every entry point and task runs under `internal/safe`. | `TestDispatchContainsPanics`, `TestAsyncTaskPanicIsContained`, `TestDispatchAlwaysAnswers` | Fixed |
| FFI-2 | HIGH | Concurrent tasks mutated the process's layout globals. | config | W10: storage-engine operations run one at a time (`withStoreRuntime`). | `TestConcurrentArchiveOperations` | Fixed (interim gate, residual) |
| FFI-3 | MED | ctx was ignored, library code wrote to stdout, and jsonbridge dropped Marshal errors. | silent | W2: `internal/console`; W10: ctx plumbed, an envelope always returned. | `TestDispatchResultThatCannotBeEncoded`, `TestDispatchHonoursCancellation`, `TestNoTerminalWritesOutsideMain` | Fixed |
| FFI-4 | HIGH | Two process-wide zstd encoders kept about 1.6 GB for the life of the process on a 16-CPU machine. | unbounded | W10: badger level 3; encoders sized by workers; per-operation encoders in the product. | `TestAcquireIsBounded`, `TestArchiveOperationsReleaseEncoders` | Fixed |
| CMP-1 | HIGH | Decompression bombs: zstd `DecodeAll` had no limit, nor did `decompress_file` or zip/tar extraction. | trust | W10: bounded decoder, limited readers, extraction limits on bytes written, dedup-bomb check. | `TestDecodeAllIsBounded`, `TestDecompressBytesIsBounded`, `TestDecompressFileIsBounded`, `TestExtractLimits`, `TestExtractCompressionRatio`, `TestDuesarcDeduplicationBomb` | Fixed |
| CMP-2 | HIGH | Archive writer Close errors were ignored, so a corrupt archive was reported as success; a failure left a partial archive. | silent | W10: writers closed in order, errors returned, temporary file renamed into place. | `TestArchiveWriteFailureLeavesNoOutput` | Fixed |
| CMP-3 | MED | Outputs were overwritten silently; split parts overwrote each other and stale parts were concatenated; the `.bak` logic deleted a user's file; `image.001` was taken for an archive part. | destructive | W10: atomic outputs, split sidecar, `on_conflict`. | `TestSplitArchiveSidecar`, `TestSplitReplacesEarlierOutput`, `TestNumberedFileIsNotASplitPart`, `TestExtractConflictPolicy` | Fixed |
| CMP-4 | MED | Absolute blob paths in an archive were resolved outside it; evidence names collided on restore. | trust | W3: blob paths resolve inside the root; W10: manifest v2. | `TestResolveLegacyBlobPaths`, `TestExtractV1Archives` | Fixed |
| CMP-5 | MED | compress and decompress did not round-trip; the CLI exited 0 on errors; `--async` was meaningless; the password went on argv; `verify-archive` had no password; work directories leaked. | logic | W10. | `TestCompressFileRoundTrip`, `TestErrorExitsNonZero`, `TestAsyncCommandsRemoved`, `TestPasswordSources`, `TestVerifyArchiveChecksContent`, `TestSweepStaleWorkDirs` | Fixed |
| CMP-6 | MED | Extraction on Windows wrote alternate data streams, reserved device names and names with trailing dots or spaces. | trust | W10: names validated through `os.Root`; refused entries reported, never renamed. | `TestExtractRefusesUnsafeNames`, `FuzzLocalName` | Fixed |
| CMP-7 | CRIT | A `.duesarc` of a folder or of several files did not give back what was archived: the tree came back flat, same-named files overwrote each other, and duplicate content was missing. | coordinates | W10: manifest v2 maps every path to its evidence. | `TestDuesarcFolderRoundTrip`, `TestDuesarcMultiFileRoundTrip` | Fixed |
| CMP-8 | HIGH | An encrypted `.duesarc` exposed the absolute paths and names of its contents in a plaintext manifest. | trust | W10: the full manifest lives in the encrypted database. | `TestEncryptedArchiveHidesItsPaths` | Fixed; chunk-name oracle (residual) |

### TUI

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| TUI-1 | HIGH | The reset dialog defaulted to Yes, and reset left the graph and full-text index stale. | destructive | W3: one reset routine; W11: No by default, typed folder name, unavailable while jobs run. | `TestResetNeedsTheNameAndDefaultsToNo`, `TestHandleResetEmptiesAndStaysUsable` | Fixed |
| TUI-2 | HIGH | Restore truncated the target before the hash was validated. | destructive | W3/W11: restore through the CLI's atomic path; an existing file needs confirmation. | `TestRestoreAsksBeforeReplacing`, `TestTUIActions` | Fixed |
| TUI-3 | HIGH | Quitting closed the database under running commands. | concurrency | W11: job runner; quit asks, cancels, waits; a stuck job leaves the database for recovery. | `TestQuitWhileJobsRunAsks`, `TestRunWaitsForJobs`, `TestTUICmdLeavesTheDatabaseToAStuckJob` | Fixed |
| TUI-4 | MED | `q` could not be typed in inputs; an empty filter panicked; typing `d` toggled deep mode; search cancel was unreachable; Cancel quit the program. | logic | W11. | `TestQIsText`, `TestNearDeepIsACheckbox`, `TestNearModeListCannotPanic`, `TestCancelReturnsToMenu`, `TestEscKeepsTheJob` | Fixed |
| TUI-5 | MED | Unbounded rendering, terminal-escape injection, the selection off screen, the clipboard written in `Update`, the micro view opened writable. | trust | W11: windowed lists, `termsafe`, commands for side effects, read-only micro view. | `TestListWindowFollowsTheSelection`, `TestEvidenceStringsAreMadeSafe`, `TestClipboardIsACommand`, `TestTopFiles` | Fixed |

### Hygiene and tooling (HYG)

| ID | Sev | Defect and where | Class | Fix | Regression test | Status |
|---|---|---|---|---|---|---|
| HYG-1 | LOW | staticcheck: 46 U1000 (dead code), 8 SA1006, 2 SA1024, SA1019 (h2c). | logic | W1 (SA1019), W10 (SA1006/SA1024), W12 (U1000). staticcheck reports 0 on both tag sets. | (none) | Fixed |
| HYG-2 | MED | `BenchmarkGetChonkMap` failed at baseline, so benchmark runs had no chunk-map numbers. | logic | W2: benchmarks encode through the codec. | `BenchmarkGetChonkMap` | Fixed |
| HYG-3 | LOW | (W12) The restore and ingest benchmarks silenced output by swapping `os.Stdout`, which does nothing since W2, so benchstat dropped the restore results. | logic | W12: `console.Quiet()`. | `BenchmarkRestoreDataPath` | Fixed |
| HYG-4 | LOW | (W13) The benchmark scripts parsed `Stored in:`, which store no longer prints, and looked for revrel sub-benchmarks that no longer exist; the revrel benchmarks ran with reverse relations off and timed empty calls. | logic | W13. | `BenchmarkProcessRevRel` | Fixed |
| HYG-5 | LOW | (W13) `dues help repair` and `dues help reindex` printed "Unknown command"; help described retired flags. | logic | W13: help for every command. | `TestEveryCommandHasHelp` | Fixed |

### Upstream defects with their own IDs

These were found in libraries while fixing the defects above. `docs/DEPENDENCY_UPGRADE_REPORT.md` has the full list, with the upstream defects that carry DUES IDs (PAR-25, PAR-27, PAR-31, PAR-33, PAR-35, CFG-13 to CFG-16, STO-25, GRA-22).

| ID | Library | Defect | What DUES did | Status |
|---|---|---|---|---|
| NTFS-U1 | libntfs | WOF-compressed files read as zeros and were reported readable | DUES reports their content unavailable | Fixed in v0.3.6 (`ReadSupport().WOFCompressed`) |
| EXT-U1 | libext | `File.ReadAt` read inline data as zeros | DUES read inline data itself | Fixed in v0.4.2 |
| EXT-U2 | libext | One unlistable directory ended `WalkDir` | DUES never uses `WalkDir` | Fixed in v0.4.2 |
| FAT-U1 | libfat | Walks skipped unreadable, depth-capped and revisited directories silently, and lost a live subtree after a deleted one | `PreorderTracker` probes, replaced by `OnSkip` | Fixed in v0.3.5 |
| XFAT-U1 | libxfat | The same, for exFAT | As FAT-U1 | Fixed in v1.4.1 |
| TABLE-U1 | libtable | `ByteOffset`/`ByteSize` wrapped; no warning for an entry past the image | DUES computed ranges itself | Fixed in v0.2.4 (`Table.ByteRange`) |
| PE-U1 | saferwall/pe | The default logger writes to stdout | DUES passes a discarding logger | Not filed |

## Root-cause analysis

Ten root-cause classes cover every finding. Six were named at the audit. Four
more emerged during the pass: unbounded work, destructive defaults, upstream
defects and plain logic errors.

| Class | Findings |
|---|---|
| config | 12 |
| concurrency | 20 |
| integrity | 18 |
| silent | 37 |
| coordinates | 20 |
| trust | 24 |
| unbounded | 20 |
| destructive | 7 |
| upstream | 11 |
| logic | 22 |

### config: process-global configuration with no persisted contract

Findings: CFG-1, CFG-2, CFG-4, CFG-6, CFG-7, CFG-9, CFG-12, STO-26, RST-2, GRA-5, SRV-8, FFI-2.

**Why it happened.** Layout settings (chunk size, chunk hash, storage mode,
encryption, compression) were command-line flags, copied into package globals
at each start. Nothing recorded which settings a database was written with.
Every reader assumed the current process's flags matched them. Quick mode was
a flag that changed several of these at once, encryption among them.

**What fixed the class.**
- `lib/dbmeta` and `lib/dbopen` fix the layout when a database is created: a
  manifest beside the database, a mirror inside it, and an in-database marker
  that detects an edited manifest.
- A flag that contradicts an existing database is a usage error (exit 2).
- Evidence records carry their own chunk size and hash algorithms, and readers
  use `dbio.EvidenceChunkSize`.
- `internal/appconfig.Resolve` validates every flag in one pure function.

**What keeps it fixed.**
- `TestRewrittenFilesAvoidLayoutGlobals` fails when a rewritten file reads the
  layout globals.
- `TestLayoutConflicts` covers contradicting flags, and `FuzzResolve` the
  configuration.

**What is left.** The globals still exist: `ApplyToGlobals` sets them at open,
and the command-line setup writes them. The storage code reads each
database's layout, tuning and encoder through `engine.For`, and the record
and chunk encoders write each database's own value encoding. The FFI
serialises storage-engine operations because of this (residual).

### concurrency: goroutines, channels and timeouts instead of structured concurrency

Findings: LOG-3, LOG-4, STO-15, STO-28, ING-1, ING-2, ING-3, ING-5, ING-8, ING-14, ING-18, ING-24, GRA-8, GRA-21, SRC-4, SRC-6, SRC-10, SRV-2, SRV-7, TUI-3.

**Why it happened.** Work was started with `go f()`. Results came back on
channels, guarded by `time.After`. A failure path could send twice or not at
all. A timeout abandoned a goroutine that kept writing. Shared resources
(the database, a mapping, the terminal) were closed while workers still used
them. Evidence records were rewritten from stale snapshots, and conflicts were
not retried.

**What fixed the class.**
- Every command handler is `func(ctx) error` under one root context that
  signals cancel.
- Worker groups (`internal/safe.Group`) contain panics and wait for every
  worker.
- Ingest orchestration is synchronous (`ingest.Run`). The chunk pass and the
  index pass have separate cancel causes.
- Every evidence state change is one retried read-modify-write transaction
  (`dbio.UpdateEvidence`).
- The server drains before it closes the database, and so does the TUI's job
  runner. Work that does not stop leaves the database for badger's recovery,
  never closed under it.

**What keeps it fixed.**
- `TestEvidenceWritesUseUpdateEvidence` bans other evidence writes.
- Every package runs under `-race` in WSL.
- The interrupt suite signals each command part way.

### integrity: "exists means valid" without a durability or integrity protocol

Findings: CFG-18, STO-1, STO-2, STO-3, STO-4, STO-5, STO-7, STO-9, STO-21, STO-27, RST-3, RST-7, ING-6, ING-9, ING-12, GRA-3, GRA-7, GRA-15.

**Why it happened.** Several things were taken as proof that data was complete
and correct, without being checked:
- the presence of a file: a blob's `ErrExist`;
- a metadata key: `C|||`;
- a state: COMPLETED.

Writes were not synced before the metadata that pointed at them. Reads were
not verified. Legacy encryption did not bind a chunk to its key, so a chunk
read from the wrong offset decrypted cleanly.

**What fixed the class.**
- New blobs are synced before their metadata. A durability barrier releases
  chunk metadata only after the material is synced.
- Containers are new files, with framed and checksummed records.
- The v2 blob codec binds every chunk to its key (AES-GCM AAD) and verifies it
  on every read.
- Ingest records PENDING, then FLUSHED (every relation durable), then
  COMPLETED (identity committed in one transaction).
- Recovery validates the material before it commits.
- Restore verifies every chunk and the recorded hash.
- Every output goes through `lib/fsx`: a temporary file, sync, then rename.

**What keeps it fixed.**
- `TestChunkMetaVisibleOnlyAfterSync`.
- The crash suite kills the process at each of 20 fault points, and
  `TestEveryPointIsWired` fails when a point is never reached.
- `repair --verify` detects every injected corruption (`TestValidateEvidenceLevels`).

### silent: errors turned into empty or partial results

Findings: CFG-8, CFG-11, LOG-1, LOG-2, STO-6, STO-17, ING-4, ING-10, ING-11, ING-15, ING-26, ING-27, PAR-2, PAR-3, PAR-6a, PAR-15, PAR-16, PAR-17, PAR-18, PAR-19, PAR-20, PAR-26, PAR-29, PAR-32, GRA-2, GRA-4, GRA-10, GRA-12, GRA-16, GRA-19, GRA-24, SRC-3, SRC-7, SRC-12, SRV-4, FFI-3, CMP-2.

**Why it happened.** Library errors were logged or dropped, and the caller
carried on as if nothing were there:
- a partition that failed to mount had no files;
- a file whose runs failed was skipped;
- an index that failed left COMPLETED evidence;
- a parse failure looked like "no artefacts".

Commands exited 0 either way.

**What fixed the class.**
- Every partition is announced with a status.
- Every record carries its issues and provenance.
- Every evidence carries an index state and coverage counters.
- Reports carry what they could not read.
- A command that finished but could not do everything exits 2
  (`lifecycle.WarningError`).
- Close errors propagate.

**What keeps it fixed.**
- The fixture invariants check that files seen equal files indexed plus
  skipped.
- `TestScanAnnouncesEveryPartition`.
- The CLI tests check exit codes.

### coordinates: coordinate-space and identity confusion

Findings: STO-10, RST-1, ING-7, ING-19, ING-20, ING-22, PAR-4, PAR-5, PAR-6, PAR-13, PAR-14, PAR-21, PAR-22, PAR-28, GRA-1, GRA-20, SRC-1, SRC-8, SRV-3, CMP-7.

**Why it happened.** Offsets and identities in different spaces were passed
around as plain integers and strings:
- the raw bytes of an E01 against its decoded image;
- a volume against the whole image;
- a chunk against a file inside it;
- a raw UUID against base64, and hex IDs against base64 IDs;
- absolute paths against paths relative to the data root.

Parents were recovered by splitting names on `|||`.

**What fixed the class.**
- Evidence is decoded at ingest, so the stored evidence is the disk image.
- Records carry explicit `EvidenceID` and `PartitionID` fields, read through
  one set of helpers.
- Extents carry both file and image offsets (`core.Extent`).
- IDs are deterministic.
- `dbio.Intersect` computes chunk ranges.
- One reader (`dbio.OpenLogical`) serves every consumer: restore, search,
  near, full-text, entropy, micro.

**What keeps it fixed.**
- `TestChunkBoundaryMatrix`, `TestReaderServesFixtures` and
  `TestSearchCountsIndexedContent`.
- `FuzzNormalizeExtents`.

### trust: no trust boundary for untrusted input

Findings: CFG-3, CFG-5, STO-16, STO-19, STO-20, RST-6, PAR-1, PAR-7, PAR-8, PAR-9, PAR-12, PAR-23, PAR-24, PAR-34, GRA-14, SRC-2, SRV-1, SRV-5, FFI-1, CMP-1, CMP-4, CMP-6, CMP-8, TUI-5.

**Why it happened.** Evidence images, archives, server requests and the
database's own files can all be crafted, but they were treated as well formed:
- parsers ran without panic containment;
- sizes and counts read from disk sized allocations;
- VHD parents were opened by the paths the evidence named;
- the server listened on every interface without authentication;
- archive entry names were trusted.

**What fixed the class.**
- `internal/safe` contains every panic in untrusted code.
- Counts and sizes are checked against what the input can hold before
  anything is allocated: extents, directories, container records, block
  indexes, msgpack values (`internal/msgpackx`).
- VHD parents resolve to siblings only.
- The server listens on loopback by default and needs a token for any other
  address. It checks CORS and Host and limits every request.
- Extraction goes through `os.Root`, with names validated and limits counted
  on the bytes written.
- `internal/termsafe` neutralises evidence-derived strings before they reach a
  terminal.
- Data-root files are private to their owner.

**What keeps it fixed.**
- 32 fuzz targets. All but one run with `DUES_SAFE_STRICT=1`, so a contained
  panic still fails.
- `TestDecodesGoThroughMsgpackx`.
- `TestNoUnboundedClusterLists`.

### unbounded: work and memory without bounds

Findings: CFG-17, STO-8, STO-11, STO-12, STO-13, STO-14, STO-22, STO-23, STO-24, RST-5, ING-13, ING-21, ING-25, PAR-10, PAR-11, GRA-6, GRA-17, SRC-5, SRV-6, FFI-4.

**Why it happened.** Designs fitted the test data, not real cases:
- whole-structure maps in one value;
- read-modify-write lists that grow with every member;
- a goroutine or a copy per item;
- caches and responses without a limit;
- scans repeated per item.

**What fixed the class.**
- One key per member.
- Streaming iteration in bounded transactions.
- Bounded pools, windows and caches.
- Paged responses.
- Top-N views.

**What keeps it fixed.**
- `TestMillionPartitionMembers`, `TestValidateFourMillionChunks` and
  `TestFolderOfTenThousandFiles` (long tests), with heap bounds.

### destructive: unsafe defaults for destructive operations

Findings: RST-4, ING-16, ING-23, SRC-11, CMP-3, TUI-1, TUI-2.

**Why it happened.** Commands that delete or overwrite acted on a single
keypress or by default:
- reset deleted any directory;
- restore replaced the target and deleted a `.bak` it had not made;
- reports overwrote files in the working directory;
- the TUI's reset defaulted to Yes.

**What fixed the class.**
- Overwriting needs `--force`.
- Reset needs a DUES root, its lock, and the folder's name typed (or `--yes`).
- Outputs are replaced atomically, and nothing outside DUES's own files is
  touched.

**What keeps it fixed.** `TestRestoreTargetSafety`, `TestResetRefusals`,
`TestExtractConflictPolicy` and the report tests.

### upstream: defects in dependencies

Findings: CFG-13, CFG-14, CFG-15, CFG-16, STO-25, PAR-25, PAR-27, PAR-31, PAR-33, PAR-35, GRA-22.

**What fixed the class.**
- Each upstream defect got a DUES workaround and a regression test that
  reproduces it.
- The aoiflux libraries released fixes, and the pass adopted them in
  `3eea4d2`, deleting the workarounds those releases made unnecessary.
- The badger, msgpack and saferwall defects remain in their latest releases,
  so their workarounds stay.

None is filed upstream yet. Filing needs the maintainers' permission. The
reproductions are in `docs/DEPENDENCY_UPGRADE_REPORT.md`.

### logic: logic errors with no wider class

Findings: CFG-10, CFG-19, CFG-20, STO-18, ING-17, PAR-30, GRA-9, GRA-11, GRA-13, GRA-18, GRA-23, GRA-25, GRA-26, SRC-9, SRC-13, CMP-5, TUI-4, HYG-1, HYG-2, HYG-3, HYG-4, HYG-5. Each was fixed in place, with its test.

## Invariants and how they are verified

### Storage, ingest and restore

| # | Invariant | Verified by |
|---|---|---|
| I1 | No chunk location record (or legacy block-index record) becomes visible before its blob or container bytes are synced. | `TestChunkMetaVisibleOnlyAfterSync` (both pipelines, file and container mode); the kill points before and after the barrier (`TestCrashDuringIngest`). |
| I2 | FLUSHED means every relation and chunk location is durable. COMPLETED means FLUSHED, plus this evidence won the content-identity commit, plus its lookup keys are written. | `TestRecoveryCompletesFlushedEvidenceInEveryMode`, `TestRecoveryFailsFlushedEvidenceWithDamagedMaterial`, `TestRecoveryCommitsFlushedAttempts`, the crash suite. |
| I3 | Layout comes only from the manifest or the evidence record, never from a runtime flag on a read path. | `TestRewrittenFilesAvoidLayoutGlobals`, `TestLayoutConflicts`, `TestRestoreUsesEachEvidenceChunkSize`, `TestValidateUsesEachEvidenceChunkSize`. Older code still reads globals set from the manifest at open (residual). |
| I4 | Every content read is verified against its chunk hash unless verification is turned off. Chunks written by this release are verified even then, because the v2 codec checks as it decodes. | `TestRestoreRejectsSubstitutedChunk`, `TestV2RejectsDamage`, `TestV2RejectsMisplacedPayload`, `TestLogicalReadAt`. |
| I5 | Every evidence state change is one read-modify-write transaction, retried on conflict. | `TestEvidenceWritesUseUpdateEvidence` (a guard over the source), `TestUpdateEvidenceConcurrentWritersKeepEveryChange`. |
| I6 | One ingest has exactly one terminal outcome (stored, aliased or failed), and no goroutine outlives it. | `TestConcurrentIngestsHaveOneWinner`, `TestStorageFailureResumes`, `TestIndexFailureKeepsStorage`, the interrupt suite. No general goroutine-leak check runs: `testutil.NoLeakedGoroutines` exists, but only its own test uses it (residual). |
| I7 | Ingest, index, repair, reindex and upgrade are idempotent. | `TestReingestIsAnAlias`, `TestRunIndexesFixtures` (a second pass writes the same keys), `TestReindexRebuildsTheSameIndex`, `TestRecoveryCommitsFlushedAttempts` (a second recovery does nothing), `TestUpgradeBlockIndexMovesRecords`. |

### Index integrity

These are checked on fixtures with known ground truth: a manifest of every
file's path, size, SHA-256, kind and flags.

| # | Invariant | Verified by |
|---|---|---|
| 1 | Every manifest file can be found with list, search and restore, with the correct content hash. That covers live, deleted, fragmented, sparse, resident and empty files. | `TestIngestFixtures` (list and restore of every record), `TestSearchFindsEveryIndexedFile`, `TestRunIndexesFixtures`. |
| 2 | Coverage reconciles: files seen = indexed + skipped, by reason. | `TestRunIndexesFixtures`. |
| 3 | Ingesting twice changes no counts. | `TestReingestIsAnAlias`, `TestRunIndexesFixtures`. |
| 4 | Restored bytes equal the bytes the filesystem library's own reader returns, and the fixture's hash. | `TestFixtureContentMatchesOpen`, `TestReaderServesFixtures`, `TestIngestFixtures`. |
| 5 | `reindex` produces the same records as the first ingest. | `TestReindexRebuildsTheSameIndex`. |
| 6 | A kill at any fault point, followed by recovery and a resumed store, gives the same listing as an uninterrupted run. | `TestCrashDuringIngest`, `TestKilledDuringIndexing`, and the other crash tests; `TestEveryPointIsWired`. |
| 7 | `repair --verify` detects every injected corruption: a truncated or deleted blob, a flipped bit, a missing relation, a wrong container offset, a missing block-index record. | `TestValidateEvidenceLevels`, `TestRepairVerify`. |

## Test matrix

`docs/TESTING.md` explains how to run each part. The gate is
`scripts\verify.ps1`; with `-Long` it also runs the long tests.

| Dimension | Covered |
|---|---|
| Platforms | Windows 11: every test, with and without `-tags faultinject`, plus the long tests. WSL Ubuntu 24.04 on ext4: race, fault injection, filesystem failures, long tests, fuzz smoke. Release builds for windows, linux and darwin on amd64 and arm64. darwin is built, not run. |
| Storage modes | default (unencrypted), encrypted, quick and container, for every store and restore test (`testdb.ForEachMode`). 13 legacy databases written by the release before the pass (`41dbc74`): sealed, raw, container, container with block index, SHA3, absolute paths, raw E01, and more. |
| Filesystems | Parser fixtures with ground-truth manifests: FAT12/16/32, exFAT, NTFS (including compressed, sparse, resident, alternate data streams, hard links, deleted records), ext4 (inline data, preallocated, deleted), XFS, HFS+ (empty volume) and classic HFS, GPT and MBR layouts, E01 single and multi-segment, fixed and dynamic VHD, VHDX. A registry hive fixture for the registry detector. |
| Corrupt input | Every raw fixture truncated at 25, 50 and 75%, and given 32 and 2048 bit flips with three seeds; a crafted directory cycle; 32 fuzz targets (60 s each in the gate) over parsers, the image layer, partition tables, the blob codec, containers, block indexes, records, msgpack, micro-artefact parsers and detectors, the server's origin check and uploads, the FFI dispatcher, extraction names and the archive manifest. |
| Crash and kill | 20 fault points, each killed (`exit:3`, or `block` then a real kill), then recovered, verified and stored again: `TestCrashDuringIngest`, `TestKilledDuringIndexing`, `TestCrashWritingTheManifest`, `TestCrashDuringRestore`, `TestCrashDuringReset`, `TestCrashDuringUpload`, `TestCrashWritingAnArchive`. |
| Interrupts | Ctrl-Break on Windows, SIGINT and SIGTERM on Linux, sent to store, `repair --fix`, reindex, restore, search, near, enrich, `micro extract`, the server and the archive commands (`TestInterruptedWrites`, `TestInterruptedReads`, `TestInterruptedGraphCommands`, `TestInterruptedServer`, `TestInterruptedArchiveCommands`). |
| Filesystem failures | A 16 MB and a 384 MB tmpfs for a full disk, a read-only tmpfs and a read-only bind mount, and a non-root user for permissions (`TestStoreDiskFullRefusedUpFront`, `TestStoreDiskFullMidway`, `TestOutputsDiskFull`, `TestLogDirDiskFull`, `TestReadOnlyMediaDatabase`, `TestPermissionUnreadableBlob`, `TestPermissionUnreadableSource`, `TestPermissionReadOnlyDataRoot`, `TestPermissionLogDir`). |
| Scale | Chunk sizes from 4 KiB to 64 MiB against file sizes around chunk boundaries (`TestChunkBoundaryMatrix`); 1M partition members (`TestMillionPartitionMembers`); 200,000 members of one hash (`TestPopularHashLookup`); 4M-chunk validation (`TestValidateFourMillionChunks`); a 10,000-file folder with an E01 set (`TestFolderOfTenThousandFiles`). |
| Back-compat | Every legacy database opens read-only and changes nothing (`TestLegacyReadsChangeNothing`), takes new evidence in its own encoding (`TestStoreIntoLegacyDatabase`), restores (`TestLegacyRestore`), searches and runs near (`TestLegacySearchAndNear`). |
| Concurrency | Parallel read processes and refused second writers (`TestConcurrentProcesses`); concurrent uploads (`TestConcurrentUploads`); concurrent ingests of one content (`TestConcurrentIngestsHaveOneWinner`); concurrent archive operations (`TestConcurrentArchiveOperations`); `-race` over every package. |
| Guards over the source | Layout globals (`TestRewrittenFilesAvoidLayoutGlobals`), evidence writes (`TestEvidenceWritesUseUpdateEvidence`), terminal writes outside `main` (`TestNoTerminalWritesOutsideMain`), msgpack decodes (`TestDecodesGoThroughMsgpackx`), fault points (`TestEveryPointIsWired`), unbounded cluster lists (`TestNoUnboundedClusterLists`), help for every command (`TestEveryCommandHasHelp`). |
| Real corpus | 55 images in `E:\dataset` and `E:\hyper\vhd`: raw and E01 disks, synthetic FAT and HFS sets, VHDX bases and Hyper-V snapshot chains up to eight deep. W12 compared parse manifests from the W5 build and the W12 build: 2,833,128 file entries, no difference. Upstream adoption: 53 images identical; two snapshot leaves differ only in their WOF streams, which are now indexed. Store, then restore with full verification: 42 raw images against their own SHA-256, jean.e01 against its embedded MD5 and SHA-1, and the six VHDX bases and two snapshot leaves against their ingest hashes. The decoded streams of all five snapshot leaves hash the same before and after the upstream adoption. |

## Performance

These are W12 results. The W0+W1 tree (`71fef6f`) and the W12 tree ran
interleaved, round by round, on one machine: Ryzen 9 5980HS, Windows, ReFS on a
file-backed virtual disk. The raw data is in `O:\tmp\dues-corpus\perf`.

| Measure | W0 | W12 | Change |
|---|---|---|---|
| `dues store`, 512 MiB random file, default flags (median of 5) | 1.43 s | 1.26 s | 12% faster |
| `dues restore` of it (median of 5) | 0.88 s | 0.93 s | 6% slower |
| `BenchmarkRestoreDataPath` | | | 14 to 24% faster, 21 to 24% fewer allocations |
| `BenchmarkIngestDataPath`, `BenchmarkIngestMetadataPath`, `BenchmarkProcessRevRel` | | | No significant change in time; metadata path 1.5 to 6% less memory |
| `BenchmarkInspectEvidenceRepairs` (5,000 records) | 0.88 / 4.6 / 13.4 ms | 1.26 / 6.6 / 16.1 ms | 20 to 42% slower |

Notes:
- The defaults differ. W0 encrypted with a key derived from the empty
  password; W12 stores without encryption unless a password is given (D8).
- W12 syncs every new blob and syncs before chunk metadata is visible.
- The faster store comes mostly from badger's table compression at zstd level 3
  (FFI-4) and from dropping the empty-password encryption.
- The slower restore is the cost of checking every chunk and the whole
  content's hash.
- `BenchmarkGetChonkMap` failed at W0 (HYG-2), so it has no comparison.
- `InspectEvidenceRepairs` is the one regression over 10%. Its CPU profile has
  the same shape in both trees: decoding each evidence record. The record grew
  from 13 fields (128 bytes) to 42 fields (512 bytes) with the W6 additions.
  Decoding the same record takes 0.88 µs into the old type and 1.26 µs into the
  new one, which accounts for the whole difference. At a million records that
  adds about 0.4 s to a `repair`. It is accepted. A follow-up could decode only
  the state fields.

## Residual risks

Each risk below is known and bounded, with the reason it is accepted.

**Data and formats**
1. **Data written before the pass may already be damaged.** That covers
   containers written in several sessions (STO-1), blobs truncated by a crash
   (STO-4), staged-pipeline and hierarchical evidence (STO-9, STO-21), and
   indexes of E01 and dynamic VHD/VHDX evidence (ING-7). `repair --verify`
   finds damaged evidence. `repair --upgrade` quarantines indexes built on a
   container's raw bytes; storing the original evidence again stores and
   indexes its decoded disk. `reindex` rebuilds indexes that failed or were
   interrupted. Bytes that were never stored cannot be recovered without the
   original evidence. The legacy container fixtures show
   such damage, and their tests skip it as a known defect.
2. **Legacy encryption stays as it was.** Older encrypted databases keep their
   SHA3-derived keys (CFG-3), and their blobs keep key-derived GCM nonces
   (STO-20). Both stay readable. There is no re-encryption migration.
3. **Data roots made before W13 keep their modes** (0755 and 0644, STO-19).
   `docs/OPERATIONS.md` gives the `chmod` that closes them.
4. **Content-addressed names leak membership.** A chunk blob's name is a hash
   of its plaintext, so someone with a candidate file can test whether an
   encrypted database or archive holds it (CMP-8). This is inherent to the
   design.

**Untrusted input**
5. **A hung third-party parser cannot be stopped in process.** Panics are
   contained, but an infinite loop inside a library call ignores cancellation.
   There is no per-file deadline (the plan proposed one), so a hang stops the
   index pass until the user interrupts. A second Ctrl-C exits with code 130,
   and recovery marks the index interrupted. Subprocess isolation would remove
   this risk.
6. **Decoding amplification.** msgpack still turns small elements into larger
   structs: a record of s bytes can take about 112 x s bytes while it decodes
   (STO-25). A compressed record is first decompressed, up to the decoder's
   256 MiB cap.

**Dependencies**
7. **Upstream defects are not filed.** The badger defects (CFG-13 to CFG-16)
   are still present in v4.9.6, and the msgpack (STO-25) and saferwall (GRA-22,
   PE-U1) defects in their latest releases. The DUES workarounds depend on how
   those libraries behave today. Their regression tests will show if a future
   release changes that.
8. **The free-space reserve is not absolute** (CFG-16). It covers a memtable
   and its flush. A compaction of a very large database writes more, and
   another program filling the disk can still bring badger's mapped writes to
   SIGBUS on Linux.
9. **graphene stays WAL-only** (D6). Opening a graph costs time in proportion
   to its write history. DUES warns past 1 GiB or 5M records. Compaction is a
   later decision.

**Platform and operation**
10. **Sync semantics vary.** Filesystems differ in what fsync guarantees, and
    Windows cannot sync a directory. Antivirus and sharing locks are retried
    and reported, not prevented.
11. **Closing the Windows console kills the process after about 5 s**, and it
    can be interrupted mid-commit. Recovery handles it; the window cannot be
    removed.
12. **The server serves plain HTTP.** TLS is not served: use a proxy or a
    tunnel for remote clients. Upload v1 is still accepted for one release,
    with a deprecation header.
13. **The FFI runs storage-engine operations one at a time**
    (`withStoreRuntime`), because some code still reads process globals. The
    work is disk-bound, so throughput suffers little.

**Coverage gaps**
14. **Some fixtures could not be built here.** A populated HFS+ volume needs
    macOS. Windows-written NTFS and differencing VHDX fixtures need Windows
    administrator rights. Those paths rely on the real corpus and on unit
    fakes. A deleted ext4 file can be recovered only from the journal, which
    DUES does not read (PAR-6).
15. **Three snapshot leaves were not round-tripped.** Their chains (76 to
    127 GiB) did not fit the scratch volume. Their decoded streams were checked
    by digest instead.
16. **The full-text mode reports its limits rather than removing them.** It
    holds the first 128 KiB of text per file and offers up to 5,000 candidates.
17. **No general goroutine-leak check runs** (invariant I6).
18. **darwin is built, not tested.**

**Small known gaps**
19. These do not affect stored data:
    - `dues restore <hash>` does not look up an evidence's content root (the
      hocho root), only its plain hash, record ID and lookups, so restoring a
      sync or async evidence by its content root reports not found.
    - The `pending` index state is declared but never set.
    - NeAR's `duration_ms` does not include phase 1.
    - The TUI's `near out` has no deep mode.
    - The graph property `evidence_hash` holds the evidence ID, not a hash
      (kept for compatibility).
    - The TUI stores without reverse relations, full-text index or enrichment.
    - VHDX blocks marked unmapped read as zeros, as the specification allows,
      so a parent's older data there is not surfaced. That is a candidate for
      deep recovery.

## Readiness assessment

The pass set four acceptance criteria and a list of deliverables.

| Criterion | Evidence | Met |
|---|---|---|
| No known crash, panic, silent-failure, leak or corruption path | Every finding in the inventory is fixed, removed, fixed upstream or worked around. The exceptions are the residual risks, each bounded. Untrusted code runs under panic containment, and the fuzz targets run with containment made strict. | Yes, with residuals 5 to 8 |
| Deterministic recovery | One recovery table (`store.RecoverEvidence`) for writable opens and `repair --fix`. A kill at each of 20 fault points recovers to the uninterrupted listing (index-integrity invariant 6). Interrupts exit 130 or 143 and leave a recoverable database and a released lock. | Yes |
| All `github.com/aoiflux/*` dependencies at their latest releases | graphene v0.9.0, libewf v0.2.1, libext v0.4.2, libfat v0.3.5, libhfs v0.3.2, libntfs v0.3.6, libtable v0.2.4, libvhdi v0.4.2, libxfat v1.4.1 and libxfs v0.4.1. `go list -m -u` reported no newer version on 2026-10-30. | Yes |
| A regression or failure-injection test for every finding | Each row of the inventory names the tests that failed before its fix, except: STO-8, STO-20, ING-17, PAR-14, GRA-22, GRA-23, SRV-8, HYG-1. These are code that was deleted (the block-index write path, the cluster-0 indexer, `cnst.DB`, the panicking helpers), a residual (STO-20), a library behaviour avoided by not calling it (GRA-22) or a one-line setting (GRA-23), and staticcheck findings, which the gate checks at 0. | Yes, with those exceptions |

Deliverables:
- this report, with the bug inventory, root-cause analysis, invariants, test
  matrix, residual risks and this assessment;
- `docs/DEPENDENCY_UPGRADE_REPORT.md`;
- `docs/OPERATIONS.md` and `docs/TESTING.md` (new), and `docs/SERVER_API.md`
  (W9);
- updated `README.md`, `STORE_LLD`, `RESTORE_LLD`, `SEARCH_LLD`,
  `NEAR_SIMILARITY_LLD`, `UUID_HASH_DESIGN_DECISIONS`, `CONTAINER_MODE_LLD`,
  `HIERARCHICAL_INDEX`, `ENRICHMENT_LLD`, `MICRO_ARTEFACT_DEEP_DIVE`,
  `TUI_QUICKSTART`, `TUI_IMPLEMENTATION`, `COMPRESSION_ABI_REFERENCE`,
  `TOOLCHAIN_NOTES` and `BENCHMARKING`;
- removed: `STORE_BOTTLENECKS.md` and `STORE_STAGED_PIPELINE_PLAN.md`
  (historical notes that `STORE_LLD` supersedes) and
  `CONTAINER_MANAGER_CURRENT.md` (a pointer to `CONTAINER_MODE_LLD`).

Gate results:
- W12 (`b5a06c3`) and the upstream adoption (`3eea4d2`) each passed the full
  gate with the long tests. That is format, vet, the six-target build,
  staticcheck (0 findings), govulncheck (0 reachable), the tests with and
  without fault injection, and in WSL race, fault injection, filesystem
  failures, long tests and the fuzz smoke of all 32 targets.
- W13 (this commit): the same full gate with the long tests; every step
  passed.

**Verdict: ready for production use**, on these conditions:
1. Before relying on a database written by an earlier release, run
   `dues repair --verify` (reads every chunk) and `dues repair --upgrade`
   (quarantines indexes built on raw container bytes, and moves legacy block
   indexes into the database). Store the original of any quarantined E01,
   VHD or VHDX again to index its decoded disk, and run
   `dues reindex --failed` for indexes that failed.
2. Expose the server beyond loopback only with a token, behind TLS
   termination.
3. On Linux and macOS, close the modes of data roots created before this
   release (residual 3).
4. Keep `scripts\verify.ps1 -Long` green for every change, and add a test that
   fails first for every new finding, citing its ID.
5. The maintainers accept residuals 1 to 19, and decide whether to file the
   upstream defects (`docs/DEPENDENCY_UPGRADE_REPORT.md`).
