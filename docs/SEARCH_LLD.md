# Search LLD

`dues search` counts query terms in the stored bytes of every stored object
and writes a ranked report. This document is for developers changing
`lib/search` or `lib/fts`. It covers how a query is parsed and matched, which
objects are read, how hits are ranked and reported, and what the optional
full-text index does.

## Code map

| File | What it holds |
| --- | --- |
| `lib/search/query.go` | `parseQuery`: the terms and the operator |
| `lib/search/match.go` | The streaming matcher: `matcher`, `termState` |
| `lib/search/engine.go` | `Run`: which objects are read, in what order, with which caches; `Result`, `Hit`, `Coverage` |
| `lib/search/bm25.go` | `rankBM25`, `DefaultRankAlpha` |
| `lib/search/search.go` | `SearchReport`: full-text candidates, ranking, report v2, the report file |
| `lib/fts/fts.go` | The full-text index: the shared handle, `IndexFiles`, `SearchCandidates` |
| `lib/structs/report.go` | `structs.SearchReport` and its parts |
| `cli/cmdsearch.go` | `dues search`: checks, summary lines, exit codes |
| `lib/server/rpc_files.go` | The `Search` RPC |
| `cli/cmdtui.go` | The TUI's search action |

## Command line

```
dues search QUERY [--rank-alpha|-r N] [--fts|-F] [--out PATH] [--force] [global options]
```

| Flag | Default | Effect |
| --- | --- | --- |
| `--rank-alpha`, `-r` | `0.35` | Weight of raw occurrence counts in the ranking. It must be a finite number >= 0. |
| `--fts`, `-F` | off | Full-text mode: read only the indexed files the full-text index offers. |
| `--out` | `report.json` | Where the report goes. |
| `--force` | off | Replace the report file if it exists. |

`dues search` (`cli.SearchCmd`):

1. Checks the query (`search.ValidateQuery`) and `--rank-alpha` before it
   opens the database.
2. Opens the database read-only (`dbopen.ReadOnly`).
3. Runs `search.SearchReport`.
4. Prints the notes, what the search covered, and each object it could not
   read.

| Exit code | When |
| --- | --- |
| 0 | The search read every object it planned to read. |
| 2 | A bad query or `--rank-alpha`, or the report file exists. Also when the search finished but some objects could not be read (`lifecycle.WarningError`). |
| 1 | Any other failure. |

## Query language

`parseQuery` (`query.go`) splits the query into terms:

- **OR.** A query that contains `|` is an OR query. Each part between bars,
  trimmed of surrounding space, is one term. Spaces and quotes inside a part
  stay in the term.
- **AND.** Any other query is an AND query. Words separated by white space are
  terms. A double-quoted span is one term, spaces included.
- **Case.** Terms are lower-cased with `strings.ToLower`.
- **Literal.** A term is matched as a literal byte sequence. There is no
  regex, wildcard or fuzzy matching.

`validateTerms` (`engine.go`) refuses a query before anything is read:

| Query | Error |
| --- | --- |
| No terms, or a term shorter than 2 bytes | `cnst.ErrSmallQuery` |
| More than `MaxTerms` (16) terms, or a term longer than `MaxTermBytes` (1024 bytes) | `ErrQueryTooLong` |

The caps also bound the bytes each term carries from one span to the next.

## The streaming matcher

`newMatcher` (`match.go`) builds one `matcher` per term.

- Matches are counted leftmost first and without overlap, as `bytes.Count`
  counts them. `aa` occurs twice in `aaaa`.
- Case is ignored.

A term is matched in one of two modes:

| Term | Mode | How it compares |
| --- | --- | --- |
| ASCII, or not valid UTF-8 | Bytewise | Byte by byte, with ASCII letters folded. It finds candidate positions with `bytes.IndexByte` on both cases of the first byte, so a scan is linear. Other bytes must be equal. |
| Valid UTF-8 with non-ASCII runes | By rune | Decodes the data rune by rune and compares `unicode.ToLower` of each rune with the term's lower-cased runes. An invalid data byte matches no rune. |

- A term that is not valid UTF-8 is matched bytewise. Decoded by rune, each of
  its invalid bytes would read as `U+FFFD` and match any other invalid byte.
- Folding by rune uses `unicode.ToLower`. A mapping that changes the number of
  runes, such as `ß` and `SS`, does not match.
- `window` is the most bytes one match can span: the term's length bytewise,
  or `utf8.UTFMax` times its runes by rune.

### Matches across span boundaries

An object's bytes arrive as spans: the part of one stored chunk, a piece of a
hole, or inline bytes. A `termState` follows one term through one object.

- `feed(data)` first calls `junction`. It decides the positions held in the
  carry, using at most `window` bytes from the start of the new span.
- `feed` then calls `scan` for the matches that start in the span itself.
- The bytes at the end of a span that could still start a match are kept as
  the carry. The carry is always shorter than the window.
- A span too short to decide the carry is added to the carry whole.
- A match across a boundary is counted once.
- At the end of the object the carry is dropped. A match that would run past
  the end is not counted, and nothing past the end is read.

## Which objects are searched

`search.Run(ctx, db, terms, Options)`:

1. Validates the terms.
2. Lists every evidence record (`E|||:`).
3. For each evidence, in key order:
   1. Skips it if its content is not stored, and counts it in
      `Coverage.NotStored`. Stored means `Completed` and not
      `IngestStateDuplicate`, so pending and failed evidence are skipped.
   2. Reads the evidence itself.
   3. Stops there if its index is quarantined (`Index.State` is
      `structs.IndexInvalid`), and counts it in `Coverage.Quarantined`.
   4. Reads its partitions, then its indexed files (the members that
      `dbio.ForEachPartitionMember` lists). Both are sorted by start offset in
      the evidence, then by key.

Every object is read through `dbio.OpenLogical`. A file's extents, holes,
inline bytes and materialised content are searched as the file's own bytes:
the same bytes restore writes.

The order serves the caches. The evidence comes first, so its partitions and
files find most chunks already counted. Files go in evidence order, so
neighbouring small files share decoded chunks.

`Options`:

| Field | Meaning |
| --- | --- |
| `Workers` | Objects read at once in a phase, and chunk reads in flight across the whole search. Zero means the database tuning's `MaxThreads()`. |
| `Candidates` | When not nil, only these indexed files (record keys) are read. Evidence and partitions are not read. Full-text mode sets it. |
| `Progress` | Called after each object with the objects done and the objects planned. |

### Coverage and unreadable objects

`scanObject` never fails the search for one object:

- An object with no content to read (`dbio.ErrContentUnavailable`: metadata
  only, or a symlink) is counted in `Coverage.Unavailable`.
- An object whose open or read fails is counted in `Coverage.Unreadable`. The
  first 20 (`maxProblems`) are named, with their errors, in
  `Coverage.Problems`. The search goes on.
- A bad partition ID, or a partition record that cannot be read, is counted
  the same way.
- `Coverage.Incomplete()` is true when `Unreadable` is above zero. The CLI then
  exits 2.

The search fails only when its context ends, when a worker panics outside a
chunk read, or when the evidence records or a partition's members cannot be
listed.

| `Coverage` field | Report key | Meaning |
| --- | --- | --- |
| `Evidence`, `Partitions`, `Files` | `evidence_searched`, `partitions_searched`, `files_searched` | Objects read, per level |
| `Bytes` | `bytes_searched` | Bytes the readers served |
| `NotStored` | `evidence_not_stored` | Evidence not searched because its content is not stored |
| `Quarantined` | `evidence_index_quarantined` | Evidence whose partitions and files were skipped |
| `NotCandidates` | `not_full_text_candidates` | Full-text mode: objects the index did not offer |
| `Unavailable` | `files_without_content` | Indexed files with nothing to read |
| `Unreadable`, `Problems` | `unreadable`, `problems` | Objects whose read failed, and the first 20 of them |

### Concurrency and memory

- Each phase (evidence, partitions, files) is one `safe` worker group
  (`safe.WithContext`). A panic in a worker becomes its error.
- The evidence phase reads one object. The partition and file phases read up
  to `Workers` objects at once.
- An object's spans come in batches. The stored spans of a batch are read in
  parallel. Each read holds a token from one channel of `Workers` tokens that
  the whole search shares. So at most `Workers` chunk reads are in flight,
  however many objects are open.
- A batch holds at most
  `min(dbio.SpanBatch(chunkSize), max(1, readBudget / (chunkSize × parallel)))`
  spans, where `readBudget` is 256 MiB. The decoded chunks held for matching
  stay near 256 MiB.
- The spans of a batch are matched in order once they are read.

### Caches

Each search has two caches:

| Cache | Key | Holds | Budget |
| --- | --- | --- | --- |
| `dbio.ChunkCache`, passed as `ReadOptions.Cache` | Chunk hash | Decoded chunks, shared by every reader of the search | `dbio.DefaultChunkCacheBytes` (64 MiB); the least recently used chunk goes first |
| `countCache` | Chunk hash and span (`Lo`, `Hi`) | Per term, the matches a scan from the span's first byte found and where it stopped; the span's first `window` bytes and last `window - 1` bytes | 64 MiB; once full, it keeps what it has |

The count cache lets the partitions and files after an evidence skip the
chunks the evidence pass already counted.

A span's counts are cached only when:

- the span is part of a stored chunk, not a hole or inline bytes;
- it is at least as long as the longest term window;
- every term's scan of it started at its first byte, so no match from the
  previous span ran into it.

Cached counts are used only when, for every term, the junction with the carry
also ends at the span's first byte. Otherwise the span is read and scanned.

The key includes the span, not only the chunk. A file that covers part of a
chunk must not take the whole chunk's count (SRC-1).

### Results and per-level counts

`Run` returns a `Result`:

- `Terms`
- `Hits`, sorted by record key
- `Coverage`

A `Hit` is an object that holds at least one term: `Key`, `Level`,
`EvidenceID`, `Size` (the bytes its reader serves) and `Counts` (per term).

The same stored bytes are counted once at each level they fall in: the
evidence, its partition and a file. Levels must not be added together.
`Result.LevelTotal(level, i)` sums term `i` over one level. The occurrences in
the stored data are `LevelTotal(LevelEvidence, i)` (SRC-9).

## Ranking

`rankHits` passes the hits of all levels to `rankBM25` (`bm25.go`), which
ranks them together.

| Symbol | Meaning |
| --- | --- |
| `N` | Objects read: `Evidence + Partitions + Files` |
| `df` | Hits that hold the term |
| `TF` | Occurrences of the term in the object |
| `TFsum` | `TF` summed over the terms |
| `L` | The object's `Hit.Size` |
| `Lavg` | Mean size of the candidates with a positive size (1 when there are none) |
| `k1`, `b` | 1.5 and 0.75 |
| `alpha` | `--rank-alpha` (`DefaultRankAlpha`, 0.35); a negative value counts as 0 |

```
IDF   = ln(1 + (N - df + 0.5) / (df + 0.5))
TFn   = TF × (k1 + 1) / (TF + k1 × (1 - b + b × L / Lavg))
BM25  = sum, over the terms the object holds, of IDF × TFn
rank  = BM25 + alpha × ln(1 + TFsum)
```

- **Candidates.** AND ranks the objects that hold every term. OR ranks the
  objects that hold any term (`buildCandidateSet`).
- **Order.** `rank` descending, then `BM25` descending, then `TFsum`
  descending, then record key ascending. The order is deterministic.
- **Full-text boost.** In full-text mode, `applyFTSBoost` adds `0.05` times the
  index's best score for the file to its BM25 score. It then sorts by that
  score, then `TFsum`, then key. This sort does not use `alpha`.

## Report v2

`SearchReport(ctx, db, Request)`:

1. Takes the report path from `Request.Out`, or `DefaultReportPath`
   (`report.json`).
2. Parses and validates the query.
3. Refuses a path that exists unless `Request.Force` is set
   (`ErrReportExists`). A directory is always refused. Nothing has been
   searched yet.
4. In full-text mode, asks the index for candidates.
5. Runs `Run`, and falls back to a full search when full-text mode finds
   nothing (see below).
6. Ranks the hits and builds the `structs.SearchReport`.
7. Writes the report atomically with `fsx.WriteAtomic` (`NoOverwrite` unless
   `Force`, `RemoveStale`). If the path appeared during the search, the write
   fails with `ErrReportExists`.

It returns an `Outcome`: `ReportPath`, `Mode`, `Result`, `Ranked` (the objects
ranked) and `Notes`.

Top-level fields:

| Key | Meaning |
| --- | --- |
| `schema_version` | `v2` (`structs.SearchReportSchemaVersion`) |
| `query` | The query as given |
| `terms` | The parsed terms |
| `operator` | `and` or `or` |
| `mode` | `scan`, or `full-text` when only the full-text candidates were read |
| `executive_summary` | One sentence; it adds the unreadable count when there is one |
| `summary` | Per level (`evidence`, `partitions`, `indexed_files`): `objects` and `occurrences` |
| `coverage` | The coverage table above |
| `occurances` | The ranked objects, in rank order. The spelling is kept for compatibility. |

`summary` counts every hit, whatever the operator. `occurances` lists only the
ranked objects, so an AND query lists only objects that hold every term.

Each entry of `occurances`:

| Key | Meaning |
| --- | --- |
| `artefact` | Base64 of the object's raw ID (its record key without the namespace) |
| `level` | `evidence`, `partition` or `indexed` |
| `path` | The file's path (indexed files only) |
| `size` | Bytes searched |
| `count` | `TFsum` |
| `term_counts` | Count per term, when the query has more than one term |
| `bm25_score` | The BM25 score, with the full-text boost in full-text mode; not `rank` |
| `files` | The object's name: the evidence name, the partition name or the file path |
| `evidence_file` | `evidence_file_hash` and `evidence_file_names`, then `partition` (`partition_hash`, `partition_part_names`) and, for a file, `indexed` (`indexed_file_hash`, `indexed_file_names`) |

`evidence_file_names` is the evidence name, then the paths of its aliases that
differ from it. Names come from the records through the record helpers
(`dbio.RecordPath`, `dbio.RecordPartitionID`, `dbio.Aliases`). A name is never
found by splitting on `|||`, which a path may contain (SRC-8).

## Full-text mode (`--fts`)

Full-text mode uses the index to choose which indexed files to read. The
counts always come from reading the files.

`fullTextCandidates` (`search.go`) calls `fts.SearchCandidates` with the parsed
terms, the operator and a limit of `ftsCandidateLimit` (5000).

- **Candidates offered.** `Run` reads only those indexed files. Evidence,
  partitions and other files are counted in `not_full_text_candidates`. A note
  says what was not searched. Another note says when the index matched more
  documents than the limit (`Candidates.Capped`).
- **Fallback.** The search reads every stored object instead, with a note
  saying why, when:
  - the database has no index (`fts.ErrIndexNotReady`);
  - the index cannot be searched, for example because another process holds
    it;
  - the index offers no candidate;
  - the candidates hold no match.

Full-text mode can miss matches. The index holds only indexed files, their
names, and the text of their first 128 KiB. The notes say so.

## The full-text index (`lib/fts`)

The index is a bleve index at `<database root>/fts` (`util.FTSPath`).

### Building it

- `dues store --fts` and `dues reindex --fts` build it. A reindex also
  rebuilds it for an evidence stored with the `fts` feature. Both call
  `IndexFiles(ctx, db, jobs)` with one job per indexed file: its record key
  and its path.
- Each indexed file is one document. Its ID is the record key. It has:
  - `name`: the path, then copies of it with `/`, `_`, `\` and `.` replaced by
    spaces, so each part is a word;
  - `content`: the text of the file's first `MaxTextBytes` (128 KiB);
  - `doc_type`: `indexed`.
- Name and text are one document, so an AND query can match a word of the
  path and a word of the content.
- Documents are written in batches of 256.
- A file whose content cannot be read still has its name indexed, and
  indexing goes on. `IndexFiles` then returns its `IndexStats` and a
  `*TextError` (`Failed`, `First`). The ingest records the index as partial,
  and the command exits 2.
- `DeleteIndexedFiles(db, ids)` removes documents. A reindex calls it for the
  files of the old index. A database without an index has nothing to delete.

**Older indexes.** Indexes from older versions kept two documents per file:
`<key>` and `<key>#text`. They are still read: `Candidates.IDs` and
`Candidates.FileScores` remove the suffix. `IndexFiles` deletes the `#text`
document when it writes a file again, and `DeleteIndexedFiles` removes both.

**Text extraction** (`extractText`, `textExtractor`):

- Reads the file through `dbio.OpenLogical` and `Stream`. A file without
  content has no text.
- Stops after 128 KiB of content, or 80 KiB of text (`maxTextRunes`).
- Completes a rune split by a chunk boundary from the next piece.
- Turns a piece that is not valid UTF-8, or less than 85% printable
  (`isLikelyText`), into one space.
- Keeps letters, digits, white space and `._-/:\`, lower-cased. Other
  characters become spaces.

### The handle

- `acquire` and `release` keep one open per index per process, shared and
  reference counted. The key is the index's absolute, cleaned path, lower-cased
  on Windows. Bleve's store takes a file lock, so a second open in the same
  process would wait on the first (SRC-7).
- A database opened read-only gets the index read-only (`read_only`).
- Every open of an existing index sets `bolt_timeout` to 2 s. An index another
  process holds is an error, not a hang.
- Only `IndexFiles`, on a writable database, creates a missing index. Every
  other caller gets `ErrIndexNotReady`. A search never builds an index.
- `Acquire(db, create)` returns an `Index`, a reference held until
  `Release`. Its `IndexFiles` and `DeleteIndexedFiles` work on the held
  index. An ingest session holds one, so a folder store or a reindex opens
  the index once instead of once per evidence (STORE_LLD, Sessions).

### Queries

`SearchCandidates(ctx, db, terms, mode, limit)`:

- Builds one match-phrase query per term, trimmed and lower-cased.
  `QueryModeAnd` joins them in a conjunction, `QueryModeOr` in a disjunction.
- Never parses a term as bleve query syntax. `name:foo` and `"x` are literal
  (SRC-3).
- Returns up to `limit` documents (`DefaultLimit`, 5000, when `limit <= 0`) as
  `Candidates`:
  - `Scores` by document ID;
  - `Total`, the documents the query matched;
  - `Capped()`, true when `Total` is more than were returned.

## Other callers

**Server.** The `Search` RPC (`lib/server/rpc_files.go`) calls `search.Run`
with one term: the keyword, trimmed. The keyword is not parsed, so `|` and
quotes are literal. Every stored object is read.

- It has a 2-minute budget (`searchBudget`), and at most 2 searches run at once
  (`searchSlots`).
- `total_count` is `LevelTotal(LevelEvidence, 0)`.
- `keyword_count_map` maps the base64 record key to the count. It holds at most
  100,000 objects (`maxSearchHits`). When more matched, the highest counts are
  kept and `truncated` is set.
- A keyword the caps refuse is `invalid_argument`. A search over its budget is
  `deadline_exceeded`.

See [SERVER_API.md](SERVER_API.md). `SearchSummaryWithContext` counts one
literal term the same way, as a library call, without the cap.

**TUI.** `tuiActions.search` (`cli/cmdtui.go`) calls `SearchReport` with
`DefaultRankAlpha` and `Force`. It reads every stored object and replaces
`report.json` in the current directory. See
[TUI_IMPLEMENTATION.md](TUI_IMPLEMENTATION.md) for how a job is cancelled.

**Cancellation.** The engine checks its context between evidence records,
objects and span batches, and while it waits for a read token. A cancelled
search returns the context's error and writes no report.

## Tests

| Test | What it checks |
| --- | --- |
| `TestMatcherCounts`, `TestMatcherStreamsAcrossCuts`, `FuzzStreamCount` (`match_test.go`) | Counts, folding, and the same count however the bytes are cut |
| `TestSearchCountsAcrossChunkBoundaries`, `TestSearchLongQueryAcrossChunks` (`engine_test.go`) | Matches across chunks, at an object's end, and files that share a chunk |
| `TestSearchSkipsUnstoredEvidenceAndQuarantinedIndexes` | Which objects are read |
| `TestSearchCountsIndexedContent` | Each object's count equals a count over the bytes its reader serves |
| `TestSearchFindsEveryIndexedFile` (`integrity_test.go`) | In every fixture, a search for 16 bytes of each indexed file finds that file |
| `query_bm25_test.go` | Parsing, caps, candidate sets, BM25 order and tie-breaks |
| `search_context_test.go` | Cancellation, schema version, report refusal, names that hold the separator, full-text coverage |
| `lib/fts/fts_query_test.go`, `lib/fts/index_test.go` | Text extraction, query building, literal terms, the shared handle, the bolt timeout, read-only use, `TextError` |
| `TestSearchCommand` (`cli/search_test.go`) | Usage errors and exit 2 |
| `TestSearchRPC` (`lib/server/rpc_test.go`) | The RPC's counts and argument errors |
| `TestLegacySearchAndNear` (`internal/testutil/legacyfixture`) | Search on every legacy fixture, through the binary |

The kill-point suite (`internal/e2e/interrupt_test.go`) stops a search at
`faultinject.SearchMid`.

## Changing search

- **A new operator.** Change `queryOp`, `parseQuery`, `buildCandidateSet`, the
  mode passed to `fts.SearchCandidates`, and the report's `operator`.
- **A new report field.** Add it to `structs.SearchReport`. Change
  `SearchReportSchemaVersion` if an existing consumer could misread the
  report.
- **Ranking.** Keep the sort deterministic: its last key is the record key.
- **Reading.** Read object bytes only through `dbio.OpenLogical`, so every
  content kind is searched as restore would write it.
