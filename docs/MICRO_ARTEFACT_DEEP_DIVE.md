# Micro-artefacts

A micro-artefact is a small fact found inside one indexed file: a URL, an IP
address, a PE import, a registry value, an event record. `dues micro extract`
finds them and stores them in the graph, under the file they came from:

```
evidence_file → partition → indexed_file → micro_artefact
```

This document is for developers who change extraction. It describes how the
code works, then where it falls short of the original spec. The graph store,
the batch and enrichment are in [ENRICHMENT_LLD.md](ENRICHMENT_LLD.md). The
proposed attack-path system that would build on micro-artefacts is in
[ATTACK_PATH_GRAPH_LLD.md](ATTACK_PATH_GRAPH_LLD.md).

## Packages

| Package | Role |
|---|---|
| `cli/cmdmicro.go` | `dues micro extract` and `dues micro list` |
| `lib/microartefact` | `Service`: parse, detect, relate, record one file |
| `lib/microartefact/fileparser`, `parsing` | Kind detection and format parsers |
| `lib/microartefact/detection`, `detectors` | The detection engine and the detectors |
| `lib/microartefact/relation` | Relation builders between artefacts of one file |
| `lib/microartefact/spec` | The rule sheet: which detectors and builders run |
| `lib/microartefact/repository/graphene` | Graph writes, the HTML view and the read models |
| `lib/microartefact/model` | `Artefact`, `Relation`, `FileRecord` |

## `dues micro extract`

`MicroArtefactCmd` runs these steps:

1. If the HTML path (`--out`) exists and `--overwrite` is not given, stop with
   a usage error. This happens before any work.
2. Open the database and the graph for writing.
3. For each completed evidence, skipping evidence whose index is quarantined,
   list the indexed files of every partition.
4. For each indexed file:
   1. Check that the graph has its indexed node (`HasIndexedFile`). If the
      evidence has none, enrich it first, once per evidence
      (`EnrichEvidenceIndex`). Missing entropy does not stop extraction.
   2. Without `--force`, skip a file that an earlier run examined
      (`ExtractionStatus` is not empty).
   3. Read the file's content (see [Reading content](#reading-content)).
   4. Extract and record its artefacts (`Service.ProcessFile`).
5. Print the summary, the top 20 files and the HTML path.
6. Exit with code 2 if any file failed.

The graph names each file by the base64 of its record's ID, as enrichment
writes it. Extraction used to derive an ID of its own
(`model.BuildIndexedFileID`, now deprecated), which matched no node, so it
found nothing and still exited 0 (GRA-1). The repository refuses a record
without an `IndexedFileID`.

### Flags

| Flag | Default | Effect |
|---|---|---|
| `--force`, `-V` | off | Extract again from files an earlier run examined. Their stored artefacts are replaced, not added to |
| `--top-k`, `-K` | `1` | How many files, by artefact count, the HTML view shows. `0` shows all |
| `--out PATH` | `graph.html` in the working directory | Where the HTML view is written |
| `--overwrite` | off | Replace an existing file at `--out` |

`--force` does not overwrite the HTML view. That is `--overwrite`.

### Counts and exit code

The summary line counts the files examined by status (see
[Outcome](#outcome)), and also:

- files skipped because an earlier run examined them;
- files without content to read (`dbio.ErrContentUnavailable`, such as a
  symlink);
- evidence enriched first because the graph lacked their files.

A file fails when its record cannot be read, it has no node in the graph even
after enrichment, its content cannot be read, or its artefacts cannot be
stored. Failures are counted, the first 10 are printed
(`failed: …`), and the command exits with code 2 (`lifecycle.WarningError`).
A database or graph error that is not about one file stops the run.

### Output after extraction

- **Top 20 files.** `micro extract` does not print the whole graph. It reads
  the 20 files with the most artefacts (`Repository.TopFiles(ctx, 20, 20)`),
  and prints them as a tree under their partition and evidence: the total
  per file, each kind with its count, and up to 3 values per kind. If more
  files hold artefacts, it says how many, and that `dues micro list` lists
  them all.
- **HTML view.** `Repository.ExportHTML(out, topK, overwrite)` writes an
  interactive view of the top `topK` files, their artefacts, the partition
  and evidence above them, and the edges among those nodes. It reads only the
  edges of the nodes it shows. The file is written beside `out` and renamed
  into place (`fsx.ReplaceFile`).
- **No artefacts anywhere.** The command prints `[micro] No indexed file has
  micro-artefacts; no HTML view written.` and writes no file
  (`ErrNothingToExport`).

The top 20 files are printed only when the HTML view is written.

### `dues micro list` and the TUI

- `dues micro list` opens the graph read-only and prints the whole hierarchy
  as JSON (`Repository.ReadHierarchy`): totals of artefacts and relations, and
  each evidence, partition and file with its artefacts. A database without a
  graph prints an empty listing, and a message on stderr saying to run
  `dues micro extract`.
- The TUI micro view opens the graph read-only and reads
  `TopFiles(ctx, 500, 20)`: the 500 files with the most artefacts, at most 20
  artefacts each. `ArtefactCount` keeps a file's total when its artefacts are
  cut.
- File names and paths come from the FILE node the indexed node contains
  (`Repository.indexedFile`). Enrichment keeps them there, not on the indexed
  node. The read models used to read them from the indexed node, so they
  named no file (GRA-25).

## Reading content

`readIndexedFileContent` reads through the database's logical reader
(`dbio.OpenLogical`, `Stream`). The reader verifies each chunk and serves
every content kind: extents of the evidence, inline bytes and materialised
objects.

- It reads at most `microartefact.RawLimit` (64 MiB) bytes, and returns the
  file's full size.
- Structured parsers (PE, ELF, PDF, EVTX, registry) get all the bytes read.
- Text detectors scan the first 4 MiB of the parsed text (`scanWindow`).
- A file larger than `RawLimit` is not parsed for structure. The first 4 MiB
  is scanned as text (or as its printable strings, when it does not look like
  text), and the file is recorded `too_large`.

Content used to be cut to 4 MiB before parsing. A larger binary or PDF then
failed to parse, and looked like a file with no artefacts (GRA-16).

## Parsing (`lib/microartefact/fileparser`)

### Kind detection

`detectSupportedKind` decides how a file is parsed, in this order:

1. **Signature** of a format the detectors parse:

   | Signature | Kind |
   |---|---|
   | `%PDF-` | `pdf` |
   | `regf` | `registry` |
   | `ElfFile\x00` | `evtx` |
   | `\x7fELF` | `elf` |
   | `MZ` with a `PE\0\0` header | `pe` |

2. **The record's type** (`IndexedType`), when it is set and not `unknown`.
   A type that names a format no detector reads is not parsed.
3. **The extension**, for a record without a type.
4. **Text:** valid UTF-8 with no NUL, and at least 85% printable in the first
   4 KiB.

Signatures come first, so a registry hive is recognised whatever its name or
type. Hives such as `SYSTEM` have no extension, and `NTUSER.DAT` is typed
`dat`.

### What a parser returns

`fileparser.Parse` returns a `ParsedFile`:

- `Kind`;
- `Content`, what text detectors scan;
- `Raw`, the file's own bytes, for detectors that parse the format
  themselves;
- `ELFMeta`, for ELF files;
- `Err`, when the format's parser failed.

| Kind | `Content` |
|---|---|
| `text` | The text |
| `pdf` | The text of the pages |
| `pe` | Printable strings, then the PE metadata as `pe.*` lines |
| `elf` | Printable strings, then the ELF metadata |
| `evtx`, `registry` | The bytes |

When a format parser fails, `Content` becomes the file's printable strings
(runs of at least 6), so text detectors still run, and the failure is recorded
as `parse_failed`. Parsing runs under `internal/safe`: a panic in a parser
fails that file, not the process.

### PE

`parsePEContent` parses the PE from memory:

- `saferpe.NewBytes(content, opts)`, with the library's logger discarded
  (it logs to stdout otherwise), and certificate and signature validation
  off. No temporary file is written.
- The metadata text is capped: 64 sections, 256 imported functions and 32
  anomalies, each with a count of what was cut.
- **A file from `NewBytes` is never `Close`d.** `Close` unmaps the file's
  data, which matters only for memory the caller mapped itself: `Close` would
  unmap it under the caller. For a heap buffer like this one the unmap fails
  (`x/sys/unix` returns `EINVAL` for memory it did not map; on Windows the
  flush before the unmap fails), `Close` drops the error, and nothing
  happens. The file holds nothing else, so not closing it leaks nothing.

### ELF

`parseELFContent` parses from memory with `saferelf.NewBytes`, and closes the
parser with `CloseFile`. No temporary file is written. Sections are capped at
64.

### PDF

`parsePDFContent` bounds what a PDF can inflate to before pdfcpu sees it.
pdfcpu inflates every stream it reads in full, so a small file could expand
to gigabytes.

1. `checkPDFInflation` decodes every stream whose dictionary names an
   expanding filter (Flate, LZW, RunLength, ASCII85, ASCIIHex) through
   pdfcpu's filters. Each stage stops just past the per-stream limit.
2. The file is refused with `ErrPDFTooLarge` when:

   | Limit | Value |
   |---|---|
   | Decoded bytes of one stream | 16 MiB |
   | Decoded bytes of all streams | 64 MiB |
   | Decoded over encoded bytes, once more than 1 MiB is decoded | 200 |

3. pdfcpu's configuration directory is disabled (`api.DisableConfigDir`,
   once per process). Otherwise pdfcpu creates one under the user's
   configuration directory on first use.
4. The file is read with `api.ReadAndValidate`, without optimisation, which
   would decode every stream to deduplicate them.
5. Page text is extracted up to 16 MiB in all. A PDF with no page text is a
   parse failure.

Image filters are not decoded by text extraction, so their streams are not
measured.

## Detection (`lib/microartefact/detection`, `detectors`)

The rule sheet (`spec.Default()`) says which detector runs on which kind.
The engine runs each enabled detector once per file, in name order, under
`internal/safe`.

| Kind | Detectors |
|---|---|
| `text` | `url-pattern`, `ioc`, `key-value`, `log-line`, `windows-event-record`, `scheduled-task`, `service-artefact`, `browser-artefact` |
| `pdf` | `url-pattern`, `ioc`, `key-value`, `log-line` |
| `pe` | `pe-metadata`, `url-pattern`, `ioc`, `key-value`, `scheduled-task`, `service-artefact` |
| `elf` | `url-pattern`, `ioc`, `key-value`, `log-line`, `service-artefact` |
| `evtx` | `evtx-native`, `windows-event-record` |
| `registry` | `registry-hive` |

### `WantsRaw` detectors

A detector that parses a format itself implements `detectors.RawDetector`
and returns true from `WantsRaw()`. The engine gives it the parse result's
`Raw`, the file's bytes, instead of `Content`. Three do: `pe-metadata`, `evtx-native` and
`registry-hive`. They used to get the text, so the PE detector never fired
(GRA-13).

`pe-metadata` uses the standard library's `debug/pe`. It reports the compile
timestamp, each section with its entropy, and the imported symbols.

### Registry hives

- A hive is recognised by its `regf` signature, and the rule `registry.hive`
  runs `registry-hive` on it.
- The key walk is iterative, with a set of visited keys (by offset) and a
  depth limit of 512 (`maxRegistryDepth`). A key that names an ancestor as its
  subkey cannot make the walk loop or overflow the stack.
- Each value becomes a `registry_value` artefact with `key_path`,
  `value_name`, `value_type` and `last_write_time`. At most 256 values are
  reported by default.
- String values are reported without the terminating NUL Windows stores. A
  DWORD or QWORD of 0 is reported as `0`, not empty.
- Fixture: `lib/microartefact/detectors/testdata/hive.dat`, written with
  Windows' offline registry library (`offreg.dll`). It holds a `Run` key and a
  chain of keys 508 deep, the library's nesting limit.
  `TestRegistryDetectorWalksAHive` finds every value, 500 levels down.
  `TestRegistryDetectorSurvivesACycle` patches a subkey list to name an
  ancestor, and each value is still reported once. The fixture cannot reach
  the 512-level limit; `FuzzExtract` covers forged hives.

### Detector rules

- PE sections are clamped to the file. A section past the end would otherwise
  panic.
- The log-line detector allocates for the results it can keep, not one per
  line.
- The URL and key-value patterns match the bytes, without copying the window
  to a string.
- A FILETIME before 1970 gives no timestamp, rather than wrapping to a date
  far in the future.
- `truncate` cuts at a rune boundary, so artefact values stay valid UTF-8.
- Each detector has a result cap (for example 64 URLs, 128 IOCs, 256 log
  lines).
- `Sheet.RemoveDetectionRule` and `RemoveRelationRule` build a new slice, so a
  copied sheet keeps its rules.

## Relations (`lib/microartefact/relation`)

Two builders relate artefacts of one file. Both are deterministic, with
confidence 1.0:

| Rule | Builder | `relation_type` | Groups artefacts by |
|---|---|---|---|
| `hash_match` | `same-identifier` | `same_identifier` | Kind and normalised value, for identifier kinds (`ioc_hash`, `ioc_ip`, `ioc_email`, `ioc_domain`, `url`, `txid`, `serial`, `message_id`) |
| `transaction_id_propagation` | `transaction-propagation` | `references_transaction` | A transaction ID: a `txid` artefact's value, a key-value artefact keyed like `txid`, or a `TX…` token in the value |

A relation edge's `method` is the rule ID.

Relating every pair in a group would make millions of relations for a value
seen thousands of times in one file. Two caps apply:

- A builder relates at most the first 32 members of a group, by offset
  (`maxGroupMembers`).
- In the graph, one relation becomes at most 64 edges (`maxRelationEdges`).
  Artefacts of the same value are joined as a star from the first one.

## `Service` (`lib/microartefact/service.go`)

### `ProcessFile`

`ProcessFile(file, content, size, replace)` extracts one file:

1. If `size` or `len(content)` is over `RawLimit`, scan the start as text and
   set status `too_large`.
2. Otherwise parse, then detect.
3. Deduplicate the artefacts (by kind, detector, span and value), and sort
   them by offset.
4. Build relations.
5. Record the result (`Repository.Record`).

### Outcome

`ProcessFile` returns an `Outcome`: `Status`, `Kind`, `Artefacts`,
`Relations`, and `ParseErr` for a parse failure.

| Status | Meaning |
|---|---|
| `extracted` | Artefacts found |
| `none` | Examined; nothing found |
| `unsupported` | No detector applies to the file's kind |
| `parse_failed` | The format's parser failed; text detectors ran on its strings |
| `too_large` | Past `RawLimit`; text detectors ran on its start |

A parse failure is therefore never mistaken for a file with no artefacts.

## Graph storage (`lib/microartefact/repository/graphene`)

### `Repository.Record`

`Record(file, artefacts, relations, Result{Status, Replace})` writes in one
transaction:

1. Check the hierarchy: `IndexedFileID`, `EvidenceFileID` and `PartitionID`
   are required. Find the indexed node by `indexed_file_id`. If it is
   missing, fail and say to run `dues enrich`.
2. If the file already has artefacts: without `Replace`, leave it as it is;
   with `Replace`, delete them, with their edges.
3. Add one node per artefact, and a `contains` edge from the indexed node.
4. Add the relation edges.
5. Set `artefact_count` and `micro_status` on the indexed node. Both are
   indexed. The HTML view and `TopFiles` rank files by `artefact_count`.

`ExtractionStatus(indexedFileID)` returns `micro_status`. A file that has
artefacts stored before statuses were recorded counts as `extracted`. When
ranking files, a file with no `artefact_count` and no `micro_status` is
counted by listing its artefacts.

### Nodes and edges

| Item | Labels | Properties | Indexed |
|---|---|---|---|
| Artefact node | graphene's `NodeTypeMicroArtefact` | `kind`, `detector`, `value`, `summary`, `confidence`, `start`, `end`, `attributes`, `evidence_hash` | `artefact_kind`, `artefact_detector`, `artefact_value_hash` (sha256 of the value), `evidence_hash`, `artefact_value` (values up to 256 bytes) |
| Containment edge | `EdgeTypeContains` | `detector`, `start`, `end` | `contains_kind`, `start`, `end` |
| Relation edge | `schema.EdgeKindRelation`, through the store's registry | `relation_type`, `method`, `deterministic_flag`, `confidence`, `evidence_fields` | `relation_type`, `relation_method`, `deterministic_flag` |

- Every node and edge also carries `producer_version`, and every edge an
  indexed `edge_key`.
- An artefact's `evidence_hash` is the `Hash` of the file record.
  `micro extract` sets it to the indexed file's ID.
- Artefact nodes are created with `Batch.Add`, never deduplicated: two files
  holding the same value have two artefacts.
- Each artefact has exactly one file parent. `storeArtefactNodes` checks this
  against the batch; `VerifyOneFileParent` checks the stored graph.
- Relation edges are deduplicated on their endpoints, `relation_type` and
  `method` (`EnsureEdge` discriminators), so two artefacts can carry more than
  one relation.

## Tests

| Behaviour | Tests |
|---|---|
| The command | `TestMicroExtract`, `TestMicroExtractPrintsTopFiles` (cli) |
| Outcomes and raw bytes | `TestServiceOutcomes`, `TestServicePEDetectorGetsRawBytes`, `TestPEMetadataDetectorParsesRawPE` |
| Parsers | `TestParseRecognisesRegistryHives`, `TestParseFailureKeepsStrings`, `TestPDFInflationIsBounded` |
| Detectors | `TestPESectionPastEndDoesNotPanic`, `TestRegistryDetectorWalksAHive`, `TestRegistryDetectorSurvivesACycle`, `TestLogLineDetectorKeepsAtMostMax`, `TestTruncateKeepsRunes`, `TestFiletimeBeforeEpoch` |
| Relations and rules | `TestRelationEdgesAreBounded`, `TestSpecFiltersCopyOnWrite` |
| Read models | `TestTopFiles` |
| Fuzzing | `FuzzExtract` (parsers, detectors and relations over six file types; output bounded), `FuzzTextDetectors` |

## Gaps against the spec

The original micro-artefact spec, `instructions.json`, is not in the
repository. Its requirements are restated here with what the code does.

| Spec asks for | The code has | Missing |
|---|---|---|
| Canonical nodes: `id`, `type`, `canonical_key`, `timestamp`, `origin`, `payload_ref`, `features`, `confidence`, `parser`, `raw_evidence_snippet` | `Artefact` (kind, detector, value, summary, confidence, span, attributes). `model.CanonicalNode` has the spec's fields, but nothing writes it | Canonical keys, timestamps, origin, payload references and snippets as node fields |
| Relation edges: `relation_type`, `evidence_fields`, `deterministic_flag`, `created_by`, `created_at`, `confidence` | Relation edges with `relation_type`, `evidence_fields`, `deterministic_flag` and `confidence`. `producer_version` names the code that wrote the edge (`dues@0/graphene@…`, since extraction names no producer) | `created_at`; a producer name for extraction |
| Deterministic links first, then probabilistic | Two deterministic builders, within one file | Links across files and evidence; any probabilistic pass |
| A triage score with a threshold | A per-artefact detector confidence | The score; every artefact found is stored |
| Modalities: pdf, txt, log, email, evtx, registry, pe, elf, pcap | text (covers txt and log), pdf, pe, elf, evtx, registry | email, pcap |

### Proposed defaults

These are proposals, not implemented. [ATTACK_PATH_GRAPH_LLD.md](ATTACK_PATH_GRAPH_LLD.md)
develops them into observations, entities and a rule engine.

| Item | Proposed value |
|---|---|
| Triage score | `0.35*time + 0.35*attribution + 0.20*uniqueness + 0.10*extractability` |
| Promotion threshold (`NODE_THRESHOLD`) | 0.60; lower scores kept only behind a debug flag |
| Time windows | 5 minutes for event streams and log bursts; 24 hours for human workflows |
| Deterministic edge confidence | 1.0 when an exact anchor matches |
| Probabilistic edge confidence | 0.55 to 0.85 by matcher; never high without corroboration from another modality |

First deterministic relations proposed: hash match (`same_file`),
transaction ID (`references_transaction`), message ID (`attached_to`, after an
email parser), and MFT/LNK and shellbag links (after those parsers).
