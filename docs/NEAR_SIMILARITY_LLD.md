# NeAR similarity LLD

NeAR (Near Artefact Relation) finds the stored objects that share content
with a query. This document is for developers changing `lib/near`. It covers
what near reads, how it matches and attributes chunks, how it scores and
re-ranks matches, and what its report holds.

## Overview

- **Queries.** `near in` takes a stored object, named by a hash or an ID.
  `near out` takes a file outside the database.
- **Phase 1.** Each distinct chunk of the query is looked up in the reverse
  relations, which record where every stored chunk occurs. Each occurrence is
  credited to the most specific object that holds it: an indexed file, else a
  partition, else the evidence. Deep mode (`--deep`) also matches, by SimHash
  signature, the query chunks the exact lookup did not match.
- **Phase 2** (`--advanced-deep`). The best K matches are re-ranked by a
  SimHash of each whole file.
- **Report.** Every match, with its scores, goes to a JSON report, with
  coverage and notes.

## Code map

| File | What it holds |
| --- | --- |
| `lib/near/near.go` | `Options`, `Coverage`, `Outcome`; the per-call `run`; the capability check; chunk lookup; `In` |
| `lib/near/query.go` | The query's chunks (`storedQuery`, `fileQuery`); `Out`; exact matches; the deep pass; the query's full-file SimHash and its cache |
| `lib/near/attribute.go` | Owner resolution, the per-evidence interval index, object chunk counts |
| `lib/near/report.go` | Report types, `finish`, ordering, confidence, overall relatedness, writing |
| `lib/near/verify.go` | Alignment deviation, top-K selection, `verifyPhase2` |
| `lib/near/simhash.go` | `fileSimHashAccumulator`, `hammingSimilarity64` |
| `lib/dbio/revrel_scan.go` | `ForEachChunkOccurrence`, `ChunkOccursAt`, `RelationAt` |
| `lib/store/revrel_add.go` | `HasReverseRelations`, `HasSignatures`, `AddMissingReverseRelations` |
| `cli/cmdnear.go` | The `near in` and `near out` commands |

## Command line

```
dues near in  HASH [--deep|-e] [--advanced-deep|-a] [--top-k|-k N] [--out PATH] [--force] [global options]
dues near out FILE [--deep|-e] [--explain-exact|-t] [--advanced-deep|-a] [--top-k|-k N] [--out PATH] [--force] [global options]
```

| Flag | Default | Effect |
| --- | --- | --- |
| `--deep`, `-e` | off | Also match query chunks by SimHash signature |
| `--advanced-deep`, `-a` | off | Phase 2: re-rank the best K matches by full-file SimHash |
| `--top-k`, `-k` | `0` | K for phase 2; `0` chooses K from memory and threads. It must be 0 to 1000. |
| `--explain-exact`, `-t` | off | `near out` only: match the file's chunks even when the file is stored whole |
| `--out` | `near_report.json` | Where the report goes |
| `--force` | off | Replace the report file if it exists |

**HASH.** `HASH` is base64. `dbio.GuessFileTypes` looks for it as the ID of an
indexed file, partition or evidence record, and as a file hash in the hash
lookups.

- Every object it finds is an exact match.
- The last one found is the query. Evidence comes last in the lookup order,
  so an evidence is the query when one has that hash.
- No object found is an error.

Both commands open the database read-only (`dbopen.ReadOnly`).

| Exit code | When |
| --- | --- |
| 0 | Every stored evidence could be searched. |
| 2 | The report file exists, `--top-k` is out of range, or some stored evidence has no reverse relations (`lifecycle.WarningError`). |
| 1 | Any other failure, including `ErrNoReverseRelations` and `ErrNoStoredEvidence`. |

**TUI.** The TUI calls `near.In` with its deep option and `near.Out` without
it, both with `Force`. It never runs phase 2.

## Data near reads

| Key | Holds | Written by |
| --- | --- | --- |
| `R\|\|\|:<owner>\|\|\|<offset>` | The chunk hash at an offset of an evidence or a materialised object | Every store |
| `RA\|\|\|:`, `Я\|\|\|:`, `RAS\|\|\|:` | Reverse relations: where a chunk occurs. The three forms are one key per occurrence, the older per-offset maps, and segments. | `store --revrel`, `repair --fix --revrel` |
| `S\|\|\|:<chash>` | A chunk's 64-bit SimHash signature (`util.ChunkSimHash64`) | `store --simhash` |
| `F\|\|\|:<record key>` | A cached full-file SimHash | Phase 2, on a writable database |

An evidence record's `Features` say what it was stored with (`revrel`,
`simhash`).

## Per-call state

All of a search's state is in one `run`, made by `newRun`:

- `matches`: the matched objects, by record key;
- `exact`: the exact matches;
- `owners`: what each reverse relation owner names, resolved once;
- `cov`: the coverage;
- `stale`: the stale occurrences.

Nothing is kept in package variables. Concurrent calls do not share state
(SRC-6). `Options.Workers` bounds the chunks looked up at once; zero means
`cnst.GetMaxThreadCount()`.

## Capability check

`checkCapability` runs before any chunk is looked up. It visits every stored
evidence: `Completed`, and not `IngestStateDuplicate`.

- `store.HasReverseRelations` says whether the evidence's chunks have reverse
  relations. An evidence with the `revrel` feature, or an empty one, has them.
  A v2 evidence without the feature does not. For an older evidence, the
  reverse relation of its first chunk is looked up.
- In deep mode, `store.HasSignatures` says the same for chunk signatures (the
  `simhash` feature, or the first chunk's signature).

| Finding | Result |
| --- | --- |
| No stored evidence | `ErrNoStoredEvidence` |
| No stored evidence has reverse relations | `ErrNoReverseRelations`, naming `dues repair --fix --revrel`. No report is written. |
| Some evidence has none | A note and `evidence_without_reverse_relations`. The CLI exits 2. |
| Deep mode, and some evidence or query chunks have no signature | A note. For them, only exact chunk matches are found. |

Without reverse relations nothing can be found, so near refuses rather than
report no matches (SRC-12). Storing the same file again only records an alias,
so it does not add them. `dues repair --fix --revrel`
(`store.AddMissingReverseRelations`) writes them from each evidence's
relations and its materialised objects' relations. It is idempotent.

## The query's chunks

A `query` holds the query's distinct chunks, each once, with every offset
where it occurs in the query (`queryChunk.offsets`).

**`near in`** (`storedQuery`):

- Reads the query object's spans through `dbio.OpenLogical`, so a file's
  extents and materialised content give its own chunks.
- Counts holes and inline bytes as `unstored_spans`. They cannot be matched.
- Records each stored span's owner and chunk offset in `query.own`. These
  occurrences are the query's own bytes, not matches. So a file's own
  partition and evidence are not reported as related to it.
- In deep mode, reads each distinct chunk's signature. A chunk without one
  counts in `unsigned_query_chunks`.
- A query whose content cannot be read fails the command.

**`near out`** (`fileQuery`):

- Opens the file, which must be a regular file, and reads it in order with
  `ReadAt`. It does not map the file, and closes it when done (SRC-4).
- Cuts it in the database's chunk size (`dbmeta.ChunkSize`) and hashes each
  piece with the database's chunk hash (`dbmeta.ChunkHasher`).
- A file whose size is a multiple of the chunk size has no empty last chunk.
- In deep mode, computes each chunk's signature with `util.ChunkSimHash64`.
- Computes the file's full-file SimHash in the same pass.

## Exact matches

**`near in`.** Every object `GuessFileTypes` found for `HASH` is an exact match,
the query included.

**`near out`.** `store.HashContent` computes the file's plain hash and content
root without storing anything. `exactMatches` then collects:

- the records whose hash is the plain hash (`GuessFileTypes`);
- the evidence holding this content, whatever hash strategy stored it
  (`store.StoredByIdentity`).

When the file is stored whole and `--explain-exact` is not set, near reports
the exact matches and stops. It looks up no chunk and skips the capability
check, so `coverage` is all zeros. A note says how to match the chunks.

## Chunk lookup

`matchQuery` looks up each distinct chunk once, in one `safe` worker group of
`Workers` goroutines (SRC-5).

`matchChunk` calls `dbio.ForEachChunkOccurrence(db, chash, maxOccurrences, fn)`:

- It reads every form of reverse relation and reports each occurrence (owner
  and offset) once.
- It stops after `MaxOccurrences` (10,000) occurrences. A chunk that occurs
  in more places, such as zeros or a common header, says little about which
  objects are related. It is skipped and counted in `common_chunks_skipped`.
- An occurrence in `query.own` is skipped.
- Every other occurrence is attributed, and the objects it lands in are
  matched with confidence 1 and method `shallow-exact`.

A query chunk is matched when at least one occurrence outside the query was
attributed. The count is `matched_chunks`.

## Attribution

`attribute` resolves each occurrence's owner once per search (`ownerInfo`,
`resolveOwner`):

| Owner | Result |
| --- | --- |
| A stored evidence | Its interval index (`buildEvidenceIndex`) |
| A materialised object of a stored evidence | The indexed file the object belongs to (`ObjectRecord.IndexedID`); the position is the object offset |
| Anything else: evidence not stored, or an object that no longer exists | No match; counted in `stale_occurrences` |

**The interval index** (`evidenceIndex`) is built once per evidence an
occurrence lies in:

- `stride` is the evidence's chunk size (`dbio.EvidenceChunkSize`).
- `parts` holds each partition's evidence range `[Start, Start+Size)`.
- `files` holds each member's evidence range `[Start, End+1)`, sorted by
  start, with `maxEnd`, the largest end so far. A member whose `End` is before
  its `Start` is not an evidence range, and is left out.
- A quarantined index names nothing. Its occurrences go to the evidence.

**`find(x)`** attributes the chunk at evidence offset `x`, which covers
`[x, min(x+stride, end))`:

1. The files whose ranges overlap it, found by binary search and a backward
   walk that `maxEnd` bounds. A file recorded with extents
   (`dbio.IndexedExtents`) must have a data extent in the chunk. So a
   fragmented file does not claim the files between its extents. A file of
   holes only holds no chunk. A file recorded as one range holds every chunk
   in it.
2. Otherwise, the partitions whose ranges overlap it.
3. Otherwise, the evidence.

A chunk that two files share is credited to both.

## Deep mode

`deepPass` runs after the exact lookups, for the query chunks that are not
matched and have a signature.

1. One pass over every `S|||:` key finds, for each such chunk, the closest
   stored signature: at least `minDeepConfidence` (0.75, 48 of 64 bits), and
   not the chunk's own hash.
2. `matchChunk` then looks up that stored chunk's occurrences. They count with
   the similarity as their confidence and method `deep-simhash`.

Only the single closest stored chunk is used for each query chunk.

`hammingSimilarity64(a, b)` is `(64 - popcount(a XOR b)) / 64`. 1.0 means the
same signature; unrelated content gives about 0.5.

`util.ChunkSimHash64` is a 64-bit SimHash. Each 4-byte window is hashed with
FNV-64a. Each bit of the hash adds 1 to that bit's weight when set, and
subtracts 1 when clear. A bit of the signature is set when its weight is at
least 0. Similar content gives similar weights wherever it lies in the chunk.

## Scores

`add` keeps, for each matched object, the best confidence at each of its chunk
positions (`match.best`), and its method. A position is an offset in its
evidence, or in its materialised object. `reportMatch` derives:

| Field | Definition |
| --- | --- |
| `chunk_match_count` | Positions matched |
| `shallow_match_count`, `deep_match_count` | Positions by method |
| `chunk_confidence_sum`, `chunk_confidence_avg` | Sum and mean of the best confidences |
| `weighted_chunk_confidence_sum`, `weighted_chunk_confidence_avg` | The same, each weighted by method: 1.35 for `shallow-exact`, 1.00 for `deep-simhash` |
| `object_chunks` | The chunks the object's bytes lie in (see below) |
| `confidence` | `min(1, chunk_confidence_sum / max(object_chunks, 1))`; 1 for an exact match |

`object_chunks` (`target.chunks`):

- a file recorded with extents: the distinct chunks its data extents lie in
  (`chunksOfExtents`);
- a materialised object: its size over its chunk size, rounded up;
- anything else: the chunks its evidence range lies in (`chunksOfRange`).

Confidence is therefore the share of the object's chunks the query matched. It
is never more than 1 (SRC-9).

**Phase 1 order** (`sortPhase1`):

1. Exact matches first.
2. `weighted_chunk_confidence_sum`, descending.
3. `chunk_match_count`, descending.
4. `confidence`, descending.
5. `size`, descending.
6. `id`, ascending.

### Alignment deviation

Chunks are cut at fixed offsets, so an object's range rarely starts or ends on
a chunk boundary. Its first and last chunks can hold bytes outside it. An
exact lookup of such a chunk is still exact, because the chunk hash covers the
whole chunk. A deep match of it compares a signature that includes the other
bytes, so it is noisy. A file outside the database is cut on its own grid,
which rarely lines up with a stored file's.

`computeAlignmentDeviation(start, size, stride)` estimates the share of noisy
chunks. It runs for each match that is not exact. `stride` is the chunk size
of the match's evidence or object.

```
totalChunks = ceil(size / stride)
edgeChunks  = 1 for a partial first chunk + 1 for a partial last chunk
              (a single chunk counts once)
deviation   = edgeChunks / totalChunks
```

| Deviation | `phase1_deviation_warning` |
| --- | --- |
| 0 | None |
| Below 0.1 | Low alignment noise |
| Below 0.3 | Moderate alignment noise |
| 0.3 or more | High alignment noise |

## Phase 2: full-file SimHash

Phase 2 runs when `--advanced-deep` is set, there is at least one match, and
the query's signature is known. It does not run when `near out` stops at an
exact match.

**K.** K is `--top-k`. When it is 0, `computeDynamicTopKWithReason` chooses:

```
memoryGiB = cnst.GetCacheLimit() / 1 GiB
K = int(memoryGiB × cnst.GetMaxThreadCount()), at least 5, at most 100
```

- `cnst.GetCacheLimit` is a quarter of the available memory, between 64 MiB
  and 8 GiB. It is 64 MiB in low-resource mode.
- An automatic K is printed with the values that chose it.
- K is never more than the number of matches.

**The candidates** are the first K matches in phase 1 order. Exact matches come
first, so they take places in the K.

**The query's signature:**

- `near in`: `storedSimhash` of the query object, read as for a candidate
  (below). When its content cannot be read, a note says so and phase 2 is
  skipped.
- `near out`: computed while the file's chunks are read. It is never cached.

**A candidate's signature** (`verifyPhase2`):

1. The cached `F|||:<record key>` value, when there is one.
2. Otherwise `storedSimhash`: the object's bytes through `dbio.OpenLogical`
   and `Stream`, with the search's context. The bytes are the object's own,
   so edge chunks add no noise.
3. A candidate that cannot be read keeps a similarity of 0 and sorts last. A
   cancelled context stops phase 2.

**The cache.** `cacheSimhash` writes the signature under `F|||:<record key>`
only when the database is writable. It ignores write errors. `dues near` opens
the database read-only, so it normally only reads the cache.

**Re-ranking.** `phase2_file_similarity` is `hammingSimilarity64` of the two
signatures. The K candidates are sorted by it, descending; the sort is stable,
so ties keep their phase 1 order. `phase2_rank` is set to 1 to K. Matches past
K are not touched.

**The accumulator** (`fileSimHashAccumulator`) computes the same SimHash as
`util.ChunkSimHash64` does for content of 4 bytes or more, over bytes that
arrive in pieces:

- It keeps the last 3 bytes of each piece as a carry.
- The windows that start in the carry are built from the carry and the first
  3 bytes of the next piece.
- All other windows are read in place, so a piece is never copied.
- No window across a piece boundary is lost or counted twice.

## Overall relatedness and final rank

`computeOverallRelatedness` gives each match one score:

| `relatedness_basis` | When | `overall_relatedness` |
| --- | --- | --- |
| `exact` | `match_method` is `exact-file-hash` | 1.0 |
| `phase1+phase2` | `phase2_rank` > 0 | `0.60 × phase2_file_similarity + 0.40 × confidence` |
| `phase1` | Otherwise | `confidence` |

Phase 2 weighs more because it does not depend on chunk alignment. Phase 1
confidence still says how much of the object was matched.

The final `rank` is a stable sort by `overall_relatedness`, descending, over
the order phase 1 and phase 2 left.

## The report

`finish` builds the report, runs phase 2 and writes the file.

- **Path.** `Options.Out`, or `DefaultReportPath` (`near_report.json`).
  `checkReportPath` runs first, before the query is looked up or opened. An
  existing path is refused without `Force` (`ErrReportExists`); a directory is
  always refused.
- **Writing.** JSON with 2-space indents, written with `fsx.WriteAtomic`
  (`NoOverwrite` unless `Force`, `RemoveStale`). A path that appeared during
  the search fails with `ErrReportExists`.

Top-level fields:

| Key | Meaning |
| --- | --- |
| `generated_at` | When report building started (RFC 3339) |
| `duration_ms` | The whole search: from its start (hashing the file for `near out`) through reading the query, phase 1, phase 2 and the report |
| `deep_mode` | `--deep` |
| `advanced_deep_mode` | Phase 2 was enabled for this run |
| `verify_top_k`, `verify_duration_ms` | K and phase 2's time, when phase 2 ran |
| `similarity_warning` | Set in deep mode: deep confidence depends on chunk alignment. `near out` has its own wording. |
| `notes` | What the user should know about how the search ran |
| `coverage` | See below |
| `input` | `query_hash_base64`, `query_id`, `query_type` (`evidence`, `partition`, `indexed`, or `external` for `near out`), `query_names`, `exact_file_match`, `explain_exact` |
| `summary` | `total_matches`, `evidence_matches`, `partition_matches`, `indexed_matches` |
| `matches` | The matches, in final rank order |

For `near in`, `query_hash_base64` is `HASH` and `query_id` is the base64 record
key of the query. For `near out`, both are the file's plain hash.

`coverage` (`Coverage`):

| Key | Meaning |
| --- | --- |
| `query_chunks` | Chunk positions in the query |
| `distinct_chunks` | Distinct chunks among them |
| `matched_chunks` | Distinct chunks found outside the query |
| `unstored_spans` | `near in`: parts of the query held by no chunk |
| `common_chunks_skipped` | Chunks with more than 10,000 occurrences |
| `stale_occurrences` | Occurrences in evidence that is not stored, or in objects that no longer exist |
| `stored_evidence` | Stored evidence the capability check counted |
| `evidence_without_reverse_relations` | Of those, the evidence near could not search |
| `unsigned_query_chunks`, `evidence_without_signatures` | Deep mode: query chunks and stored evidence without signatures |

Each match:

| Key | Meaning |
| --- | --- |
| `rank` | Final rank |
| `id` | Base64 of the record key |
| `type` | `evidence`, `partition` or `indexed`. A materialised object's file is `indexed`. |
| `exact_match`, `match_method` | `exact-file-hash` for an exact match; otherwise the method of its first listed chunk |
| `hash_base64` | Base64 of the raw ID (the record key without its namespace) |
| `names` | The object's name, from its record |
| `start`, `end`, `size` | From the record |
| `internal_object_size` | An evidence's partitions, or a partition's members |
| `confidence`, `confidence_percent`, `object_chunks`, chunk counts and sums | See [Scores](#scores) |
| `overall_relatedness`, `overall_relatedness_percent`, `relatedness_basis` | See above |
| `matching_chunks` | Up to 1000 (`maxListedChunks`): `index` (the offset in the query), `method`, `confidence`, `confidence_percent` |
| `phase1_deviation_estimate`, `phase1_deviation_warning` | See [Alignment deviation](#alignment-deviation) |
| `phase2_file_similarity`, `phase2_file_similarity_pct`, `phase2_rank` | Phase 2; left out when zero |

Names come from the records through the record helpers (`dbio.RecordPath`),
never by splitting a name on `|||`, which a path may contain.

## Cost

| Step | Cost |
| --- | --- |
| Capability check | One pass over the evidence records, and one probe per older stored evidence without the feature |
| Lookup | One `ForEachChunkOccurrence` per distinct query chunk, at most 10,000 occurrences each |
| Interval index | Built once per evidence that holds an occurrence: its partition records and member lists, decoded once |
| Attribution | A binary search per occurrence; each file's record and extents read once |
| Deep pass | One pass over all `S\|\|\|:` keys, comparing each with every unmatched signed query chunk, then one lookup per chunk found |
| Phase 2 | Per candidate, one `F\|\|\|:` read when cached, or one read of the object's bytes |

## Tests

| Test | What it checks |
| --- | --- |
| `TestNearNeedsReverseRelations` | The error, the repair, then a half-shared evidence at confidence 0.5 |
| `TestNearConfidenceIsAShare` | A chunk held many times counts once per chunk of the match, so confidence stays at most 1 |
| `TestNearAttributesToFiles` | Two FAT16 images: the other image's files match; none of the query's own objects do |
| `TestNearOutReadsTheFile` | Exact, explained and half-stored files; the file is closed |
| `TestNearReportRefusesExistingFile` | `--out` and `--force` |
| `TestNearRunsAreIndependent` | Concurrent runs equal the same runs alone |
| `TestNearDeepMatchesSimilarChunks`, `TestNearSkipsCommonChunks` | Deep mode and the common-chunk skip |
| `TestChunksOfExtents`, `TestFileSimHashInPieces` | Chunk counts of extents; the accumulator over any cut |
| `TestForEachChunkOccurrenceReadsEveryForm` (`lib/dbio`) | Every reverse relation form |
| `TestNearCommand` (`cli`), `TestLegacySearchAndNear` | The commands, and every legacy fixture through the binary |

The kill-point suite (`internal/e2e/interrupt_test.go`) stops `near in` at
`faultinject.NearMid`.
