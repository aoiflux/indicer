# NeAR Similarity Engine — Technical Design Document

**DUES (Deduplicated Unified Evidence Store)**

---

## 1. Overview

The NeAR (Near Artefact Relation) engine finds files in the database that are
similar to a query file. It operates in two distinct phases:

- **Phase 1 — Chunk-Level Matching**: Coarse filtration over the entire database
  using shared physical chunk hashes and SimHash-based approximate matching.
  Produces a ranked candidate set.
- **Phase 2 — Full-File SimHash Re-ranking** (`--advanced-deep`): Fine
  re-ranking of the top-K candidates using alignment-independent whole-file
  SimHash comparison to correct for the inherent noise in Phase 1's fixed-offset
  chunk windows.

The engine supports two query modes:

- **Infile**: Query file is already stored in the database (referenced by its
  hash).
- **Outfile**: Query file is on disk outside the database (referenced by its
  path).

---

## 2. Fundamental Problem: Fixed-Offset Chunking and Alignment Noise

DUES stores files by dividing them into fixed-size physical blocks (default 256
KB, configurable via `--chonksize`). This is called a **chunk** or **chonk**.
Multiple logical files can share chunks when their byte content overlaps.

### 2.1 The Alignment Problem

Because chunking is fixed-offset (not content-defined), a logical file's byte
range and the physical chunk boundaries rarely coincide:

```
Physical chunks:
[    Chunk 0    |    Chunk 1    |    Chunk 2    |    Chunk 3    ]
0KB           256KB           512KB           768KB           1024KB

Logical file A (starts at 180KB, ends at 820KB):
                [  partial  |   full   |   full   |  partial  ]
                 ^edge chunk                        ^edge chunk
```

A logical file with `N` total chunks can have at most **2 edge chunks**: the
start boundary chunk and the end boundary chunk. For a file that fits entirely
within one chunk, that single chunk is a partial edge.

### 2.2 Impact on Similarity Scoring

| Chunk type                 | Phase 1 shallow (exact hash)                                                                 | Phase 1 deep (SimHash)                                                              |
| -------------------------- | -------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| Full interior chunk        | ✅ Reliable                                                                                  | ✅ Reliable                                                                         |
| Partial edge chunk (start) | ✅ Reliable (hash is of the full physical block — shared by all files that share that block) | ⚠️ Noisy — simhash signal includes bytes outside the logical file                   |
| Partial edge chunk (end)   | ✅ Reliable                                                                                  | ⚠️ Noisy                                                                            |
| Outfile chunk (any)        | ✅ Reliable if chunk aligns                                                                  | ⚠️ Fundamentally noisy — outfile's chunk grid is independent of all DB files' grids |

For most files of practical size (N ≥ 10 chunks), `2/N ≤ 20%` of Phase 1 deep
comparisons are noisy. Phase 2 corrects the ranking order introduced by this.

---

## 3. Data Model and DB Schema

Understanding what is stored helps understand what the similarity engine looks
up.

### 3.1 File Types

```
EvidenceFile  (namespace "E|||:")  — top-level container image (e.g. an evidence file)
  └─ PartitionFile  (namespace "P|||:")  — a partition inside an evidence file
       └─ IndexedFile  (namespace "I|||:")  — a logical file inside a partition
```

Each file stores `Start` (byte offset in the evidence file) and `Size`.

### 3.2 Chunk-Related Keys

| DB key pattern                 | Value                        | Purpose                                                                                                            |
| ------------------------------ | ---------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| `R\|\|\|:<ehash>\|\|\|<index>` | `<chash>`                    | Forward relation: evidence file → chunk at byte index                                                              |
| `Я\|\|\|:<chash>\|\|\|<index>` | `map[fileHash]struct{}`      | Reverse relation: chunk → all files that reference it at this index                                                |
| `S\|\|\|:<chash>`              | uint64 (8 bytes, big-endian) | Per-chunk 64-bit SimHash signature                                                                                 |
| `C\|\|\|:<chash>`              | encrypted chunk bytes        | The raw chunk data                                                                                                 |
| `F\|\|\|:<fid>`                | uint64 (8 bytes, big-endian) | **Cached** full-file Phase 2 SimHash signature (written on first `--advanced-deep` run, reused on subsequent runs) |

The chunk index is the physical byte offset (multiple of ChonkSize) within the
evidence file, not a sequential counter.

---

## 4. Phase 1: Chunk-Level Matching

### 4.1 Entry Points

**Infile** (`NearInFile`):

```
NearInFile(fhash string, db, deep bool, verify bool, topK int)
  → GuessFileType(fhash)  →  fid  (typed namespace prefix + raw hash bytes)
  → nearIndexFile / nearPartitionFile / nearEvidenceFile
     → getNearLogicalFile / getNearFile
        → getNear()  (generator goroutine)
           → countRList()  (worker goroutines)
```

**Outfile** (`NearOutFile`):

```
NearOutFile(fpath string, db, deep bool, explainExact bool, verify bool, topK int)
  → mmap the file
  → iterate chunks via getOutfileChonks()
     → processOutChunk() per chunk  →  buildNearGenForOutChunk()
        → countRList()
```

### 4.2 The `getNear` Generator (Infile Path)

`getNear(start, size int64, ehash []byte, db, deep bool) chan NearGen`

This function runs in a goroutine and feeds chunk results through a channel. For
each chunk index in the file's byte range:

1. Compute `dbstart = (start / ChonkSize) * ChonkSize` — snap start down to
   chunk boundary to cover the partial edge chunk.
2. Loop `nearIndex` from `dbstart` to `start + size` in steps of `ChonkSize`.
3. Look up the forward relation key `R|||:<ehash>|||<nearIndex>` to get `chash`
   (the hash of the physical block at that index).
4. Look up the reverse relation key `Я|||:<chash>|||<nearIndex>` to get `revmap`
   — the set of all other file hashes that share this exact chunk.

**Shallow path** (default, `deep=false`):

- If `len(revmap) < 2` (only the query file references this chunk), skip — no
  other file shares it, nothing to match.
- Otherwise emit the revmap as a match with `confidence=1.0`,
  `method="shallow-exact"`.

**Deep path** (`--deep` flag):

- If `len(revmap) < 2`, fall through to `partialMatch()` instead of skipping.
- `partialMatch` calls `partialChonkMatch()` which scans all `R|||:` keys,
  computes chunk-level SimHash similarity against each candidate's stored
  signature, and returns the best match exceeding
  `minDeepPartialConfidence = 0.75`.
- Emits match with `confidence ∈ [0.75, 1.0]`, `method="deep-simhash"`.

### 4.3 The Chunk SimHash Algorithm (`util.ChunkSimHash64`)

Location: `lib/util/util.go`

This is the **per-chunk** SimHash, computed at store time from the raw decrypted
256 KB block bytes and stored under `S|||:<chash>`.

```
Algorithm: SimHash-64 with FNV-64a and a 4-byte sliding window

Input: data []byte  (up to 256 KB)

For each 4-byte window at position i:
    h = FNV-64a(data[i:i+4])
    for bit in 0..63:
        weights[bit] += (+1 if bit set in h, -1 otherwise)

Output sig uint64:
    bit[k] = 1 if weights[k] >= 0
```

**Key property**: identical or similar 4-gram content produces similar weight
distributions and therefore similar 64-bit signatures, regardless of where in
the 256 KB block the content appears. This is what makes it tolerant of small
edits within a block.

**Stored at**: store time, alongside the chunk data. See
`dbio.SetBatchChonkSignature`.

### 4.4 Similarity Comparison (`util.HammingSimilarity64`)

```
HammingSimilarity64(a, b uint64) float64
    distance = popcount(a XOR b)      // bits.OnesCount64
    return (64 - distance) / 64       // fraction of matching bits
```

Range: `[0.0, 1.0]` — `1.0` means identical signatures, `0.5` means
random/unrelated.

The threshold `minDeepPartialConfidence = 0.75` requires at least 48 of 64 bits
to agree before a deep match is accepted. Below this threshold a chunk pair is
considered too dissimilar to contribute to a candidate's score.

### 4.5 `countRList` / `countEviFile` / `countPartiFile` / `countIdxFile`

These functions walk the type hierarchy (evidence → partition → indexed) to
determine _which specific_ file object to credit with the match:

```
countRList(inputHash, idmap, nearGen, db)
  For each (nearIndex, revList) in nearGen.RevMap:
    For each revhash in revList:
      countEviFile(nearIndex, confidence, method, inputHash, revhash, idmap)
        → GetEvidenceFile → if has InternalObjects (partitions):
            countPartiFile
              → countFile (range check: is nearIndex within this partition's Start..End?)
              → GetPartitionFile → if has InternalObjects (indexed files):
                  countIdxFile
                    → countFile (range check)
                    → idmap.Set(idxFileID, confidence)
```

The range check `isInRange(start, end, index)` is critical: it ensures that a
chunk match only gets credited to a file if that file's byte range actually
covers that chunk index. This prevents false positives from container-level
sharing.

The `idmap` (`ConcMap`) accumulates raw confidence scores (sum of per-chunk
contributions) keyed by file ID. The final normalisation happens in
`updateConfidence`.

### 4.6 `updateConfidence` — Score Normalisation

After all chunks are processed:

```
for each candidate in idmap:
    rawScore  = sum of per-chunk confidence contributions
    totalChunks = ceil(fileSize / ChonkSize)
    normalised = (rawScore / totalChunks) * 100
```

This converts the raw sum into a `[0, 100]` percentage representing the fraction
of the candidate's chunks that matched the query. A file with 10 chunks, 5 of
which matched at full confidence (1.0 each), gets `(5.0 / 10) * 100 = 50%`.

### 4.7 Ranking (Phase 1 Final Sort)

Candidates are sorted by the following criteria in priority order:

1. `ExactMatch` (exact file hash match) — always first
2. `WeightedChunkConfidenceSum` — sum of per-chunk scores weighted by match
   method:
   - `shallow-exact`: weight `1.35` (cryptographically certain identity)
   - `deep-simhash`: weight `1.00` (approximate)
3. `ChunkMatchCount` — more matching chunks = broader similarity
4. `WeightedChunkConfidenceAvg`
5. `ChunkConfidenceAvg`
6. `ChunkConfidenceSum`
7. `Confidence` (normalised percent)
8. `Size` (larger file as tiebreaker)

---

## 5. Phase 1 Alignment Deviation Estimate

### 5.1 Purpose

Before Phase 2 runs (or even if it doesn't), each candidate in the report gets
an analytical noise estimate, so consumers understand how much to trust the
Phase 1 score.

### 5.2 `computeAlignmentDeviation` (Location: `lib/near/verify.go`)

```go
func computeAlignmentDeviation(start, size int64) (edgeChunks int, deviation float64, warning string)
```

```
totalChunks = ceil(size / ChonkSize)

if totalChunks == 1:
    // Only an edge chunk when the file does not perfectly fill the block.
    // A file that starts on a chunk boundary and fills it exactly has zero noise.
    edgeChunks = 1   if (start % ChonkSize != 0) OR ((start + size) % ChonkSize != 0)
    edgeChunks = 0   otherwise
else:
    edgeChunks += 1   if start % ChonkSize != 0    (start is mid-block)
    edgeChunks += 1   if (start + size) % ChonkSize != 0    (end is mid-block)

deviation = edgeChunks / totalChunks
```

| Deviation | Warning level | Interpretation                                                            |
| --------- | ------------- | ------------------------------------------------------------------------- |
| 0.0       | None          | All chunks are fully aligned. Phase 1 deep scores are reliable.           |
| < 0.10    | Low           | < 10% of chunk comparisons are noisy. Scores are generally reliable.      |
| < 0.30    | Moderate      | 10–30% noise. Consider `--advanced-deep`.                                 |
| ≥ 0.30    | High          | Small or heavily misaligned file. `--advanced-deep` strongly recommended. |

This is computed per-candidate in `buildNearReportMatch` and stored as
`phase1_deviation_estimate` and `phase1_deviation_warning` in the JSON report.
It is present for all matches **except** those with
`match_method: "exact-file-hash"`; exact hash matches carry no chunk-alignment
noise by definition, so both fields stay at their zero/empty defaults.

---

## 6. Phase 2: Full-File SimHash Re-ranking (`--advanced-deep`)

### 6.1 Motivation

Phase 1 compares individual 256 KB chunks. A logical file's content can appear
at any byte alignment relative to those chunk boundaries, causing the same
content to produce different chunk hashes and different SimHash signatures.
Phase 2 bypasses the chunk grid entirely by computing a SimHash over the
complete, logically-contiguous file bytes.

### 6.2 Dynamic K Selection (`computeDynamicTopK` / `computeDynamicTopKWithReason`)

Location: `lib/near/verify.go`

When `--top-k 0` (the default), K is chosen automatically:

```
freeGB = available_system_memory_bytes / 4 / GB    (same limit used everywhere in DUES)
k = int(freeGB * GetMaxThreadCount())
k = clamp(k, min=5, max=100)
k = min(k, matchCount)
```

Rationale: Phase 2 requires one sequential chunk stream per candidate. Each
stream saturates approximately one I/O thread; free memory governs OS-level
chunk file caching effectiveness. Scaling K with `freeGB × threads` keeps Phase
2 time proportional to available resources, preventing it from overwhelming the
host.

Users can override with `--top-k N` for an explicit fixed K.

**Auto-selection transparency**: when K is auto-selected (i.e. `--top-k` was not
provided), DUES prints the selection basis to stdout so the user can see exactly
what drove the decision:

```
[advanced-deep] auto-selected top-K = 12 (6.0 GB available memory × 2 CPU threads, bounded [5, 100], capped at 47 total matches)
```

This message is suppressed when the user supplies an explicit `--top-k N` value.

Two functions implement this:

- `computeDynamicTopK(matchCount int) int` — pure value, used when the reason
  string is not needed.
- `computeDynamicTopKWithReason(matchCount int) (k int, freeGB float64, threads int)`
  — also returns the raw inputs so `writeNearJSONReportWithInput` can format the
  explanatory message.

### 6.3 The Full-File SimHash (`fileSimHashAccumulator`)

Location: `lib/near/simhash.go`

The accumulator implements the same SimHash algorithm as `ChunkSimHash64` but is
designed for **streaming** across multiple calls (one per chunk) while correctly
handling 4-gram windows that straddle chunk call boundaries.

```
State:
    weights [64]int     — running bit weight counters
    carry   []byte      — last 3 bytes from previous Write call (window - 1 bytes)
    hasher  fnv.Hash64a — reused FNV-64a instance

write(data []byte):
    combined = carry + data         // prepend carry to handle cross-boundary windows
    for i in 0 .. len(combined) - 4:
        h = FNV-64a(combined[i:i+4])
        for bit in 0..63:
            weights[bit] += (+1 if bit set in h, -1 otherwise)
    carry = combined[len(combined)-3:]   // save last 3 bytes for next call

finalize() uint64:
    sig bit[k] = 1 if weights[k] >= 0
```

**The carry buffer is the key design detail.** Without it, the 3 windows
straddling each chunk boundary (`data[-3:-1+i]` for `i=1,2,3`) would be silently
dropped, introducing a systematic bias: longer files would lose proportionally
more boundary windows relative to short files, corrupting similarity scores for
large files compared with short ones.

### 6.4 `verifyPhase2`

Location: `lib/near/verify.go`

```go
func verifyPhase2(querySig uint64, topKMatches []nearReportMatch, db *badger.DB) error
```

For each of the K candidate matches:

1. Decode the candidate's file ID from its base64 JSON representation.
2. Create a fresh `fileSimHashAccumulator`.
3. Call `dbio.StreamLogicalFileBytes(candidateID, db, acc.write)` — this
   iterates the candidate's chunks in order, applying the same edge-trimming as
   `GetChonkData` (strips bytes outside the logical file's `[Start, Start+Size)`
   range), and feeds each trimmed chunk to the accumulator.
4. Call `acc.finalize()` to get the candidate's 64-bit full-file signature.
5. Compute `hammingSimilarity64(querySig, candidateSig)` — same formula as Phase
   1 but now across the entire file, not a single chunk.
6. Store `Phase2FileSimilarity` and `Phase2FileSimilarityPct` on the match.

After all K candidates are scored, `sort.SliceStable` re-orders the top-K slice
by descending `Phase2FileSimilarity`. Ties preserve Phase 1 rank (stable sort).
`Phase2Rank` is then assigned `1..K`.

Candidates outside the top-K retain their Phase 1 rank; only the top-K subset is
reordered.

Stream errors for individual candidates are non-fatal — a candidate with an
unreadable chunk gets `Phase2FileSimilarity = 0.0` and sorts to the bottom.

### 6.5 `dbio.StreamLogicalFileBytes`

Location: `lib/dbio/dbio.go`

This is the infrastructure function that Phase 2 depends on. It mirrors the
restore pipeline (`store/restore.go`) but calls a callback instead of writing to
disk:

```go
func StreamLogicalFileBytes(fid []byte, db *badger.DB, fn func([]byte) error) error
```

Internally:

1. Resolves the file type from the namespace prefix to get `start`, `size`,
   `ehash`.
2. Computes `dbstart = GetDBStartOffset(start)` to cover partial start edge
   chunk.
3. Iterates `restoreIndex` from `dbstart` to `start + size` in steps of
   `ChonkSize`.
4. For each index, looks up `R|||:<ehash>|||<restoreIndex>` → `chash` →
   `C|||:<chash>`.
5. Calls `GetChonkData(restoreIndex, start, size, dbstart, end, ckey, db)` which
   applies the two trimming rules:
   - **Start edge**: if `restoreIndex == dbstart`, strip `start - restoreIndex`
     leading bytes (the part of the block before the logical file starts).
   - **End edge**: if `restoreIndex + ChonkSize > end`, strip bytes after
     `end - restoreIndex`.
6. Passes trimmed bytes to `fn`.

This ensures that `Phase2FileSimilarity` is computed only over the logical
file's actual bytes, not the padded physical block — eliminating the edge
alignment noise that affects Phase 1.

---

## 7. Query Signature Computation

### 7.1 Infile Query

```go
// In NearInFile, before Phase 1 starts — cache-first:
if verify {
    if cached, err := dbio.GetFileSimhash(fid, db); err == nil {
        vcfg.querySig = cached   // O(1) — reuse stored sig from a previous run
    } else {
        acc := newFileSimHashAccumulator()
        dbio.StreamLogicalFileBytes(fid, db, func(chunk []byte) error {
            acc.write(chunk)
            return nil
        })
        vcfg.querySig = acc.finalize()
        dbio.SetFileSimhash(fid, vcfg.querySig, db)  // persist for future runs
    }
}
```

The query file's bytes are streamed from DB using the same
`StreamLogicalFileBytes` path. This guarantees the query signature is computed
from exactly the same byte range as candidate signatures — alignment-consistent
comparison.

On the first `--advanced-deep` run against a given DB file the full chunk stream
is read and the resulting uint64 is written under `F|||:<fid>`. Every subsequent
run retrieves it in a single 8-byte DB read.

### 7.2 Outfile Query

```go
// In NearOutFile, immediately after mmap:
if verify {
    acc := newFileSimHashAccumulator()
    acc.write(mappedFile[:size])     // entire file in one write call (carry buffer unused)
    vcfg.querySig = acc.finalize()
}
```

The outfile is already fully in memory as a mmap'd slice
(`github.com/edsrzf/mmap-go`). A single `write` call covers the entire file. The
carry mechanism is still correct (carry remains empty after a single call since
no subsequent call follows).

**No caching for outfile query signatures**: the query file is external to the
database and has no stable DB key, so its signature is always computed fresh
from the mmap. Candidate signatures are still cache-first (`F|||:` lookup).

**Important caveat**: the outfile's chunk boundaries are completely independent
of any DB file's chunk boundaries. Even though `Phase2FileSimilarity` removes
the chunk alignment noise, it compares byte content holistically. If an outfile
is a slightly modified version of a DB file, the full-file SimHash should still
score high because the 4-gram weight distribution across the entire file will be
similar.

---

## 8. Report Structure

### 8.1 Top-Level (`nearJSONReport`)

```json
{
  "generated_at": "2026-04-16T12:00:00Z",
  "duration_ms": 1240,
  "deep_mode": true,
  "advanced_deep_mode": true,
  "verify_top_k": 12,
  "verify_duration_ms": 380,
  "similarity_warning": "...",
  "input": { ... },
  "summary": { ... },
  "matches": [ ... ]
}
```

`similarity_warning` is set at the top level when deep mode is on, explaining
Phase 1 alignment limitations. More specific for outfile queries.

### 8.2 Per-Match Fields

```json
{
  "rank": 1,
  "id": "<base64>",
  "type": "indexed|partition|evidence",
  "exact_match": false,
  "match_method": "shallow-exact|deep-simhash|exact-file-hash",
  "start": 180000,
  "end": 820000,
  "size": 640000,
  "confidence": 0.65,
  "confidence_percent": 65.0,
  "chunk_match_count": 3,
  "deep_match_count": 1,
  "shallow_match_count": 2,
  "chunk_confidence_sum": 2.75,
  "chunk_confidence_avg": 0.916,
  "weighted_chunk_confidence_sum": 3.4,
  "weighted_chunk_confidence_avg": 1.133,

  // phase1_deviation_estimate and phase1_deviation_warning are omitted / zero
  // when match_method == "exact-file-hash" (no alignment noise possible).
  "phase1_deviation_estimate": 0.333,
  "phase1_deviation_warning": "High alignment noise...",

  "phase2_file_similarity": 0.84375,
  "phase2_file_similarity_pct": 84.375,
  "phase2_rank": 1,

  // Single synthesised score — see §8.3 for the blend formula.
  "overall_relatedness": 0.7094,
  "overall_relatedness_percent": 70.94,
  "relatedness_basis": "phase1+phase2",

  "matching_chunks": [ ... ]
}
```

`rank` is determined by `overall_relatedness` descending — the most strongly
related artefact is always rank 1 using the best available signal. `phase2_rank`
captures the ordering within the Phase 2 top-K pass specifically. When
`verify_mode=false`, `phase2_*` fields are absent from the JSON.

### 8.3 Overall Relatedness Formula

`overall_relatedness` is the single score a consumer should use to answer _"how
strongly are these two artefacts related, given everything that ran?"_

| `relatedness_basis` | When set                                                     | Formula                                             |
| ------------------- | ------------------------------------------------------------ | --------------------------------------------------- |
| `"exact"`           | `match_method == "exact-file-hash"`                          | `1.0` (identity)                                    |
| `"phase1+phase2"`   | `--advanced-deep` ran for this candidate (`phase2_rank > 0`) | `0.60 × phase2_file_similarity + 0.40 × confidence` |
| `"phase1"`          | Shallow/deep chunk matching only, no Phase 2                 | `confidence` (chunk-level coverage ratio)           |

Phase 2 receives 60% weight because it is alignment-independent and operates
over the whole file; Phase 1 confidence (40%) captures chunk-level coverage
breadth and remains a meaningful co-signal.

The final `rank` in the report is a re-sort by `overall_relatedness` descending,
applied after Phase 2 completes (or immediately after Phase 1 when
`--advanced-deep` is not used). `rank` therefore always reflects the best
combined signal rather than just Phase 1 ordering.

---

## 9. CLI Flags

```
dues near in  <hash>  [--deep] [--advanced-deep] [--top-k N]
dues near out <path>  [--deep] [--explain-exact] [--advanced-deep] [--top-k N]
```

| Flag              | Short | Default  | Effect                                                                                                           |
| ----------------- | ----- | -------- | ---------------------------------------------------------------------------------------------------------------- |
| `--deep`          | `-e`  | false    | Enable Phase 1 SimHash matching for chunks with no exact reverse relation (slow, O(DB) scan per unmatched chunk) |
| `--advanced-deep` | `-a`  | false    | Enable Phase 2 full-file SimHash re-ranking of top-K candidates                                                  |
| `--top-k`         | `-k`  | 0 (auto) | Number of candidates for Phase 2. 0 = auto-select based on available memory and CPU                              |
| `--explain-exact` | `-t`  | false    | Outfile only: run chunk drilldown even when an exact file hash match exists                                      |

---

## 10. Complexity Summary

| Operation                          | Complexity                  | Notes                                                                 |
| ---------------------------------- | --------------------------- | --------------------------------------------------------------------- |
| Phase 1 shallow (per chunk)        | O(1)                        | DB point lookup for `Я                                                |
| Phase 1 deep (per unmatched chunk) | O(D)                        | Full scan of `R                                                       |
| Phase 1 total (infile, shallow)    | O(N)                        | N = number of chunks in query file                                    |
| Phase 1 total (infile, deep)       | O(N × D)                    | Worst case; only runs for chunks with no exact match                  |
| Phase 1 total (outfile)            | O(N) shallow, O(N × D) deep | Same structure                                                        |
| `updateConfidence`                 | O(M)                        | M = number of candidate files found                                   |
| Phase 2 query sig (infile, cold)   | O(N)                        | Stream N chunks from DB; result stored under `F\|\|\|:<fid>`          |
| Phase 2 query sig (infile, warm)   | O(1)                        | Single 8-byte `F\|\|\|:<fid>` lookup on subsequent runs               |
| Phase 2 query sig (outfile)        | O(F)                        | F = file size; single pass over mmap; never cached (no stable DB key) |
| Phase 2 per candidate (cold)       | O(Nc)                       | Nc = chunks in candidate file; result stored under `F\|\|\|:<fid>`    |
| Phase 2 per candidate (warm)       | O(1)                        | Single 8-byte DB lookup on subsequent runs                            |
| Phase 2 total (all warm)           | O(K)                        | K point lookups; typical case after first run                         |
| Phase 2 total (all cold)           | O(K × Nc_avg)               | K bounded by `computeDynamicTopK`; typically 5–100                    |

---

## 11. Source File Map

| File                   | Responsibility                                                                                                                                             |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `lib/near/near.go`     | Phase 1 core: `getNear`, `partialChonkMatch`, `checkInSignature`, `countRList/EviFile/PartiFile/IdxFile`, `countFile`, `isInRange`                         |
| `lib/near/infile.go`   | Infile entry point (`NearInFile`), report structs, `buildNearReportMatch`, `computeOverallRelatedness`, `updateConfidence`, `writeNearJSONReportWithInput` |
| `lib/near/outfile.go`  | Outfile entry point (`NearOutFile`), `processOutChunk`, `buildNearGenForOutChunk`, `getOutfileChonks`                                                      |
| `lib/near/simhash.go`  | `fileSimHashAccumulator` (Phase 2 streaming SimHash), `hammingSimilarity64`                                                                                |
| `lib/near/verify.go`   | `computeAlignmentDeviation`, `computeDynamicTopK`, `computeDynamicTopKWithReason`, `verifyPhase2`, `verifyConfig`                                          |
| `lib/near/common.go`   | `GetNames`, `getUniqueNames`                                                                                                                               |
| `lib/util/util.go`     | `ChunkSimHash64` (Phase 1 chunk SimHash), `HammingSimilarity64`, `GetDBStartOffset`                                                                        |
| `lib/dbio/dbio.go`     | `StreamLogicalFileBytes`, `GetChonkData` (edge trimming), `SetBatchChonkSignature`, `GetChonkSignature`, `SetFileSimhash`, `GetFileSimhash`                |
| `lib/store/restore.go` | Reference implementation for chunk streaming (Phase 2 mirrors this)                                                                                        |
