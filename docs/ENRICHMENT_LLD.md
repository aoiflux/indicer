# Enrichment and the graph store

This document is for developers who change how DUES writes or reads its graph.
It covers:

- the graph store's files, and how DUES opens, locks and recognises it
  (`lib/graph`);
- how DUES writes to it (`lib/graph/batch.go`);
- graph enrichment, which copies the evidence hierarchy from the database into
  the graph (`lib/enrichment`);
- how `dues list` uses what enrichment wrote.

Micro-artefact extraction adds nodes under the indexed files that enrichment
writes. It is described in
[MICRO_ARTEFACT_DEEP_DIVE.md](MICRO_ARTEFACT_DEEP_DIVE.md).

## The database is authoritative

The database (badger, in `kvdb/`) holds the evidence, partition and
indexed-file records. The graph holds a copy of that hierarchy, plus what DUES
derived from it: MIME types, tags, entropy, timestamps and micro-artefacts.

- No command reads the graph to decide what evidence exists. `dues list` lists
  from the database.
- The graph can be rebuilt from the database. Remove the data root's `graph`
  directory, then run `dues enrich` and `dues micro extract`.

## Files

The store is the directory `<data root>/graph` (`util.GraphPath`,
`cnst.GRAPHDIR`).

| File | Owner | Holds |
|---|---|---|
| `graphene.wal` | graphene | The store: every record ever written |
| `dues.lock` | DUES | The DUES lock (see [Lock](#lock-libgraphlockgo)) |
| `dues.lock.<pid>-<random>.info` | DUES | One record per lock holder, for error messages |
| `dues.schema.json` | DUES | The marker: how this store encodes its labels |

graphene keeps its own lock file and ledger files in the same directory.

**The store is never compacted.** No DUES code calls `Compact`, so there is no
`graphene.csr`. An older release, which reads only the log, can therefore
still open the store. The cost is that every open replays the whole log (see
[Replay warning](#replay-warning)). `TestStoreIsNeverCompacted` checks this.

## Opening the store (`lib/graph/handle.go`)

All access goes through a `*graph.Handle`. Code does not open one directly. It
uses a repository:

| Repository | Writable | Read-only |
|---|---|---|
| Enrichment | `enrichment.OpenGrapheneRepository(root)` | `enrichment.OpenGrapheneRepositoryReadOnly(root)` |
| Micro-artefacts | `microartefact.OpenGrapheneRepository(root)` | `microartefact.OpenGrapheneRepositoryReadOnly(root)` |

Both call `graph.OpenWith(root, graph.OpenOptions{ReadOnly: …})`. A
repository's `Close` releases its reference to the handle.

### Two modes

| | Writable | Read-only |
|---|---|---|
| graphene open | `graphenedb.Open` | `graphenedb.OpenReadOnly` (graphene's shared lock); `graphenedb.OpenLive` on read-only media |
| No store yet | Created | `graph.ErrNoGraph`; nothing is created |
| `dues.lock` | Exclusive; creates the directory | Shared |
| Marker | Written when absent or out of date | Never written |
| Ordered property keys | Declared (`propindex.Declare`) | Not declared |
| Writes | Through a `Writer` | Refused |

- **Read-only callers:** `dues list`, `dues micro list` and the TUI micro
  view. Any number of read-only processes can share a store.
- **Writable callers:** `dues enrich`, `dues micro extract`, an ingest or
  reindex with `--enrich`, any reindex of evidence in a database that has a
  graph (to delete the old index's nodes), and `dues repair --upgrade`.

**Read-only media.** graphene opens its lock file for writing even to share
it, so `OpenReadOnly` fails where nothing can be written. The handle then
opens the store with `OpenLive`, which takes no process lock. This is safe
because no writer can exist on read-only media. `isReadOnlyMedia` decides
this: a permission error, `EROFS` on Unix, or `ERROR_WRITE_PROTECT` on
Windows.

### Open sequence (`openHandle`)

1. Read-only only: stat the directory. If it is missing, return
   `ErrNoGraph`. If it is not a directory, return an error.
2. Take `dues.lock` (`acquireLock`): exclusive for writable, shared for
   read-only. This never blocks.
3. Compute the replay warning. If there is one, print it to stderr and log it.
4. Open graphene (`openStore`). graphene's `disk.ErrStoreLocked` becomes
   `*ErrGraphLocked`.
5. Detect the label vocabulary and choose a schema registry
   (`resolveSchema`). A writable open stamps the marker when needed.
6. Writable only: declare the ordered property keys.

If a step fails, the steps before it are undone.

### One handle per store per process

- Handles are kept in a process-wide registry, keyed by `registryKey(dir)`.
  The key is the absolute, cleaned path with symlinks resolved, lower-cased
  on Windows. Symlinks are resolved on the deepest part that exists, so a
  directory not yet created keys the same way. Two spellings of one directory
  get one handle.
- `Open` of a store that is already open returns the same handle and adds a
  reference. `Release` drops one. The last `Release` closes the store and
  releases the lock. A `Release` past zero is an error.
- A read-only request can share a writable handle. A writable request while
  the store is open read-only in the process is refused: the shared lock
  already taken does not exclude other writers.
- The store is opened outside `registryMu`, because replaying the log can take
  minutes. A second open of the same directory waits for the first. Opens of
  other directories do not wait.
- The handle caches node identities (index key and value to node ID) for its
  life. graphene never reuses node IDs, so the cache needs no invalidation. A
  node this process deletes is removed from it.

### Errors

| Error | When | What callers do |
|---|---|---|
| `graph.ErrNoGraph` | A read-only open finds no `graph` directory | `list` lists from the database. `micro list` prints an empty listing and says to run `dues micro extract`. The TUI micro view shows nothing |
| `*graph.ErrGraphLocked` | Another process holds `dues.lock`, or graphene refuses the open | Report it. The message names the holder from the lock records, for example `graph store at … is in use by dues enrich (pid 1234, started 10:15:02); wait for it to finish, or close it, and try again`. It unwraps to graphene's error when graphene refused |

### Replay warning

Every open replays the whole log. Past these sizes the open is slow and needs
memory in proportion, so DUES warns:

| Measure | Threshold | How it is read |
|---|---|---|
| Log size | 1 GiB | A stat of `graphene.wal` |
| Log records | more than 5,000,000 | `graphenedisk.PreflightOpen`, only when the log is at least 45 MB (the smallest log that can hold that many records) |

The warning is printed as `warning: …`, logged, and kept on the handle
(`Handle.ReplayWarning`).

### Versions (`lib/graph/version.go`)

The DUES and graphene versions come from the binary's build information
(`debug.ReadBuildInfo`). A `replace` directive's version wins. Without build
information they read `(devel)` and `(unknown)`.

- The marker records `written_by` as `dues <version> / graphene <version>`.
- Every derived node and edge records `producer_version` (see
  [Rules every write follows](#rules-every-write-follows)).

## Lock (`lib/graph/lock.go`)

`dues.lock` is an advisory whole-file lock (`flock` on Unix, `LockFileEx` on
Windows). graphene has taken its own lock since v0.9.0. DUES keeps
`dues.lock` beside it so that a DUES binary that knows only `dues.lock` is
still excluded.

- **Taking it.** A writable open takes it exclusively and creates the
  directory if needed. A read-only open takes it shared.
- **Never blocking.** A held lock fails at once with `*ErrGraphLocked`.
- **Read-only media.** A shared lock opens an existing lock file read-only. If
  there is no lock file, it takes no lock.
- **Holder records.** Each holder writes its own
  `dues.lock.<pid>-<random>.info`, a JSON object with `pid`, `command`,
  `started` and `shared`. It is written only when the lock file could be
  opened for writing. A failure to write it is ignored: the record only
  improves a later error message.
- **Releasing.** A holder removes its own record, then unlocks. A shared
  holder never removes another holder's record.
- **Stale records.** An exclusive holder is alone, so it removes every other
  record, and the single `dues.lock.info` that older releases wrote.
- **Naming a holder.** `readLockInfo` prefers an exclusive holder, then the
  shared holder that started last.

## Marker and detection

### Marker (`lib/graph/schema/marker.go`)

`dues.schema.json` is the only DUES file in the graph directory that
describes the data. It records:

- `schema_version` and `written_by`;
- `custom_node_base` and `custom_edge_base`, the graphene type bands the
  store was written with;
- `mixed_vocabulary`: the store holds labels in both encodings;
- `node_kinds`, `edge_kinds`, `dynamic_node_kinds`, `ordered_node_keys`,
  `ordered_edge_keys` and `migrated_from`.

Rules:

- `WriteMarker` uses `fsx.WriteFileAtomic`: a unique temporary file, synced
  and renamed into place, then the directory synced. A crash leaves the old
  marker or the whole new one.
- An empty marker counts as absent. `ReadMarker` logs a warning, the store is
  probed, and the next writable open writes the marker again.
- A marker that is not valid JSON refuses the open. Opening without it could
  misread the labels.
- A writable open writes the marker when it is absent, or when the schema
  version or `mixed_vocabulary` has changed. It keeps the ordered keys and
  dynamic kinds already recorded. Read-only opens never write it.
- A writable database open removes stale temporary files from the graph
  directory (`dbopen`'s `sweepTemps`).

### Kinds and the schema registry (`lib/graph/schema`)

A `schema.NodeKind` or `schema.EdgeKind` is an offset within graphene's custom
type band, not a raw graphene type. graphene v0.4.0 moved the band from 128 to
32768, so a raw value captured from the library silently matched nothing in
older stores. Code resolves kinds through the store's registry
(`Handle.Schema()`), never through graphene's `CustomNodeType`.

The container kinds are `KindEvidenceFile` (0), `KindPartition` (1),
`KindIndexedFile` (2) and `KindFile` (3). Containment uses graphene's built-in
`EdgeTypeContains`, and micro-artefacts use the built-in
`NodeTypeMicroArtefact`.

### Detection (`lib/graph/migrate/detect.go`)

`migrate.Detect` classifies the store at every open:

1. Read the marker. If there is one, check its bases against the linked
   graphene (`CheckLibraryBases`) and refuse a newer schema version.
2. Count the nodes. No nodes: `VocabEmpty`.
3. Count nodes and edges by type (`CountNodesByType`, `CountEdgesByType`).
   It does not list node IDs.
4. Classify the container kinds and relation edges found under the legacy
   encoding (graphene v0.1.3, base 128) and the current one:

| Found | Vocabulary | Registry |
|---|---|---|
| Both | `VocabMixed` | Legacy (reads match both encodings) |
| Legacy only | `VocabLegacyV1` | Legacy |
| Current only | `VocabCurrentV2` | Current |
| Neither, with a marker or only graphene's built-in types | `VocabCurrentV2` | Current |
| Neither, no marker, and a custom type this build does not know | `VocabUnknown` | None: the open is refused |

- A store without a marker that holds only built-in types (micro-artefacts) is
  accepted. Built-in types sit below both custom bands, so nothing can be
  misread.
- A marker that says `mixed_vocabulary` keeps a store mixed, even when the
  probe finds only current labels.
- When the bases recorded in the marker do not match the linked graphene, the
  error says how to rebuild the graph.

## Writing (`lib/graph/writer.go`, `lib/graph/batch.go`)

### Writer

`Handle.Writer(actor)` returns the handle's one write session. It blocks until
the current writer closes, and fails on a read-only handle. An upsert is a
lookup followed by an insert, two graphene calls; one writer at a time is what
stops two goroutines from both missing and both inserting.

- `Writer.As(producer)` names the pipeline that derives the writes.
- `Writer.Batch()` begins a transaction that carries the writer's actor. The
  zero `ActorRef` writes unattributed.
- `Handle.ExclusiveWrite(fn)` runs any other read-then-write sequence under
  the same lock.

### Batch operations

| Method | Does |
|---|---|
| `Upsert(key, labels, props, index)` | Writes a full record. Creates the node if absent. Otherwise merges `props` over the stored properties and replaces the named index entries |
| `UpsertFill(key, labels, props, index)` | Writes a stub. Creates the node if absent. Otherwise leaves it as it is |
| `Add(labels, props, index)` | Creates a node unconditionally, for nodes with no natural key |
| `Update(id, props, index)` | Merges properties into a node and replaces the named index entries |
| `DeleteNode(id)` | Deletes a node and every edge incident to it |
| `EnsureEdge(src, dst, labels, props, index, discriminators…)` | Creates an edge unless an equivalent one exists |
| `Commit()` | Writes everything as one atomic unit |
| `Rollback()` | Discards the batch. Nothing is written or cached |

`Upsert` and `UpsertFill` find a node by its key in this order: nodes this
batch already wrote, the handle's identity cache, then the store
(`NodesByProperty`). The key is always indexed. A stub and then a full record
in one batch become one node holding the record.

### Rules every write follows

- **Index entries commit with the nodes.** `Commit` passes each node's index
  entries to the transaction (`IndexNodeProperties`, `UpdateNodeIndexed`,
  `UpdateNodePartialIndex`, `IndexEdgeProperties`). They used to be written
  after the commit, so a crash in between left nodes that could not be found
  by their key, and the next run duplicated them (GRA-3).
- **A full record replaces a stub.** A stored node gets the new properties
  merged over its own, and `UpdateNodePartialIndex` replaces the index
  entries the write names. A value the node no longer has stops matching.
  Before, `Upsert` never changed a stored node, so evidence and partition
  nodes kept the properties of the stubs that created them (GRA-2).
- **A write that changes nothing writes nothing.** `Commit` compares the
  merged properties and labels with the stored ones, as they read back, and
  skips a node that has not changed. The log is never compacted, so a re-run
  over unchanged data must not grow it.
- **Labels are required.** `Upsert`, `UpsertFill` and `Add` refuse a node
  with no labels, naming the node. `EnsureEdge` refuses an edge with none.
  `Upsert` also needs a non-empty key and value.
- **Every write is attributed to a producer.** Every node a batch writes and
  every edge it creates gets `producer_version`, unless the caller set it.
  The value is `<pipeline>@<version>/graphene@<version>`. Enrichment writes
  `enrichment@1/…`; a writer that names no producer writes `dues@0/…`.
- **Edges are deduplicated on their identity.** `EnsureEdge` hashes the
  endpoints, labels and discriminators into `edge_key`, stores it on the edge
  and indexes it. A later run finds the edge through that index. On a store
  with legacy labels it also walks the source's edges, because edges written
  before `edge_key` existed are not in the index.
- **Identities are cached only once durable.** The batch adds its keys to the
  handle's cache after `Commit` succeeds, and removes the nodes it deleted.

### Properties (`lib/graph/props`)

- `props.Pack` encodes msgpack with map keys sorted, so the same properties
  always encode to the same bytes.
- `props.Unpack` returns an error for a blob that does not decode, so a
  damaged node is not mistaken for one with no properties. The returned `Map`
  is never nil.
- `Map` accessors (`Str`, `Int64`, `Float64`, `Bool`, `Strings`) accept every
  integer width msgpack may choose, and return the zero value for a missing
  key.
- Reads that cannot decode a node name it in the error (`graph node <id>: …`).

## Enrichment (`lib/enrichment`)

Enrichment writes an evidence's hierarchy to the graph: the evidence, its
partitions, their indexed files, and one FILE node for each.

### Entry points

| Trigger | Call | When |
|---|---|---|
| `dues enrich` (`cli/cmdenrich.go`) | `Service.EnrichAll(ctx)` | Every completed evidence, except duplicates |
| `dues store --enrich`, `dues reindex --enrich` | `Service.EnrichEvidenceIndex(ctx, eid)` (`lib/ingest/features.go`) | After the evidence is committed, when its index is complete or partial |
| `dues reindex` of evidence stored with `--enrich` | Same | Same |
| `dues micro extract` | Same | Once per evidence whose files are not in the graph |

Store and reindex open the graph once per run, through their ingest session
(STORE_LLD, Sessions), and keep it open until the run ends. Each evidence
then reuses the handle and its identity cache; before, each evidence opened
the graph and replayed its whole log.

`EnrichEvidenceIndex` refuses evidence that is not completed
(`cnst.ErrIncompleteFile`). `EnrichAll` reads each evidence in transactions of
its own, not under one held for the whole run.

### What one evidence becomes (`enrichEvidence`)

1. Read the partition records named in the evidence's `InternalObjects`.
   Skip this step when the index is quarantined.
2. Compute the entropy of the evidence and of each readable partition, in one
   pass (see [Entropy](#entropy)).
3. Write the evidence node (`UpsertEvidence`), then its FILE node.
4. If the index is quarantined, stop.
5. For each partition:
   - A partition record that cannot be read gets a partition node named by
     its ID (`upsertUnreadablePartition`), so every partition the evidence
     names has a node.
   - Otherwise write the partition node (`UpsertPartition`). Then write the
     partition's FILE node and one record per indexed file, in commits of at
     most 2,000 records (`fileBatchSize`).

Each record write (`upsertFile`) ensures the whole chain in one batch: an
evidence stub and a partition stub (`UpsertFill`), the indexed node
(`Upsert`), the FILE node (`Upsert`), and the `contains` edges between them.
A stub never overwrites the full record that `UpsertEvidence` or
`UpsertPartition` wrote.

Names come from the record helpers. A file's path is `dbio.RecordPath`, its
name the path's last element, and its deleted flag the record's own. The
evidence node's `path` is the path of the evidence's first source member.

### Nodes

Every node also carries `producer_version`.

| Kind | Upsert key | Properties | Indexed besides the key |
|---|---|---|---|
| `KindEvidenceFile` | `evidence_file_id` | `evidence_file_id`, `name`, `path`, `size`, `evidence_hash`, `node_type` (`evidence`), `evidence_type`, `ingest_state`, `completed`, `failed`, `partition_count` | `evidence_hash`, `name`, `path`, `evidence_type`, `ingest_state`, `size` |
| `KindPartition` | `partition_id` | `partition_id`, `name`, `evidence_file_id`, `start`, `size`, `filesystem_type`, `hash`, `indexed_file_count` | `name`, `filesystem_type`, `size` |
| `KindIndexedFile` | `indexed_file_id` | `indexed_file_id`, `hash`, `size`, `partition_id`, `evidence_file_id`, `file_type`, `start`, `is_deleted`, `is_fragmented`, `has_entropy`, `entropy`, `has_simhash`, `simhash`, times | `size`, `file_type`, `entropy`, `simhash`, times |
| `KindFile` | `file_node_id` | `file_node_id`, `name`, `path`, `hash`, `size`, `level`, `file_type`, `mime_type`, `tags`, `is_deleted`, `is_fragmented`, `has_entropy`, `entropy`, `has_simhash`, `simhash`, `start`, `evidence_file_id`, `partition_id`, `indexed_file_id`, times | `name`, `path`, `level`, `is_deleted`, `is_fragmented`, `mime_type`, `tags`, `size`, `entropy`, `simhash`, times |

Notes:

- `level` is `evidence_file`, `partition` or `indexed_file`.
- `evidence_hash` holds the same value as `evidence_file_id`.
- A FILE node's `hash` is the evidence ID, the partition ID, or the indexed
  file's content hash, by level.
- `entropy` and `simhash` are written only when known; `has_entropy` and
  `has_simhash` say which.
- Times are `mtime`, `atime`, `ctime` and `btime`, in Unix nanoseconds. Only
  the times the filesystem records are written.
- `simhash` is the file fingerprint ingest cached in the `F|||` namespace
  (`dbio.GetFileSimhash`).
- `micro extract` adds `artefact_count` and `micro_status` to indexed nodes.
- Index encodings: `name`, `level`, `is_deleted`, `is_fragmented`, `mime_type`
  and `tags` are plain strings (booleans as `true`/`false`, tags joined with
  `|`). Sizes, entropy, simhash, times and the FILE `path` use the
  order-preserving encoders in `lib/graph/propindex`.

The indexed node does not carry the file's name or path; the FILE node it
contains does. A reader that needs the name reads the FILE node.

### Edges

All hierarchy edges are graphene's `EdgeTypeContains`, written with
`EnsureEdge`:

- evidence → partition;
- partition → indexed file;
- evidence, partition or indexed file → its FILE node.

### Identity

The graph names each object by the base64 (standard encoding) of its record's
ID in the database:

| Node | Key | Value |
|---|---|---|
| Evidence | `evidence_file_id` | base64 of the evidence's raw ID (a UUIDv7, minted when it is first stored) |
| Partition | `partition_id` | base64 of the partition record's ID, as the evidence's `InternalObjects` lists it |
| Indexed file | `indexed_file_id` | base64 of the indexed record's ID |
| FILE | `file_node_id` | `ids.HashText("file-node", <parent ID>, <path, or name when there is no path>)`, where the parent ID is the evidence, partition or indexed-file ID by level |

- Partition and indexed-record IDs are derived from the evidence ID and the
  record's place in it (`lib/index/ids.go`). Indexing the same evidence again
  writes the same IDs, so a re-run finds the same nodes.
- Micro-artefact extraction looks files up by the same `indexed_file_id`.
  It used to derive an ID of its own (`BuildIndexedFileID`, now deprecated),
  which matched no node enrichment wrote, so extraction found nothing
  (GRA-1).
- These values are persisted. Changing a derivation orphans every node already
  keyed by it. `lib/graph/ids` tests pin them.

### Entropy

Entropy is Shannon entropy in bits per byte, from 0 to 8
(`lib/entropy.Counter`).

- **Indexed files.** An index pass run with `index.Options.Entropy` computes
  each file's entropy from the bytes it already reads (for hashing, inline
  content and materialised content), and records it on the record
  (`IndexedFile.Entropy`). `dues store --enrich` sets it, and so does a
  reindex of evidence stored with enrichment. Enrichment then copies the
  value.
- **Records without a recorded value** (indexed without enrichment, or by an
  older release) are read once through `dbio.OpenLogical`. The value is
  cached by content hash for the rest of the evidence.
- **Evidence and partitions.** Both are computed in one pass over the
  evidence's stored chunks (`dbio.OpenLogical`, `Spans`). A partition is a
  range of the evidence, so it counts the bytes inside its range. Empty
  evidence has entropy 0.
- **No content to read.** A file whose content is unavailable (a symlink, or
  content the index could only describe) has no entropy. That is not a
  problem.
- **Read failure.** The node is written with `has_entropy` false, and the
  failure is counted in the report.

### Report and `IncompleteError`

`EnrichAll` and `EnrichEvidenceIndex` return `(Report, error)`:

| `Report` field | Counts |
|---|---|
| `Evidence`, `Partitions`, `Files` | Nodes written |
| `Quarantined` | Evidence with a quarantined index; only the evidence was written |
| `Unreadable` | Objects whose entropy could not be computed because their content could not be read |
| `Problems` | The first 10 of those failures |

When `Unreadable` is above zero the error is `*enrichment.IncompleteError`:
the graph was written, but some entropy is missing.

- `dues enrich` prints the counts and each problem (`unreadable: …`), then
  exits with code 2 (`lifecycle.WarningError`).
- An ingest or reindex records the index as `partial` and returns
  `ingest.IndexIncompleteError` (exit code 2).
- `micro extract` ignores it when it enriches an evidence first.

### Quarantined indexes

`dues repair --upgrade` quarantines an index built over the raw bytes of an
EWF or VHD container: it marks it `IndexInvalid`. Such an index is hidden:

- Enrichment writes only the evidence node and its FILE node, and counts it in
  `Report.Quarantined`.
- `repair --upgrade` removes the index's nodes from the graph
  (`ingest.DeleteGraphIndex`, keeping the evidence node).
- `dues list` leaves out its partitions.
- `micro extract` skips the evidence.

### Deleting an index from the graph

`GrapheneRepository.DeleteEvidenceIndex(evidenceID, keepEvidence)`:

1. Finds the evidence nodes by `evidence_file_id`.
2. Marks every node reachable from them over `contains` edges: partitions,
   indexed files, FILE nodes and micro-artefacts.
3. With `keepEvidence`, keeps the evidence node and its own FILE node (level
   `evidence_file`).
4. Deletes the marked nodes, with their edges (relation edges included), in
   commits of at most 10,000 nodes.

It returns the number of nodes deleted. A database without a graph is left
without one (`graph.Exists`).

A reindex removes the old index from both stores before it rebuilds:
`store.DeleteEvidenceIndex` deletes the database records and their full-text
documents, then the graph delete runs with `keepEvidence`. If the evidence was
stored with enrichment, the reindex enriches it again. `repair --upgrade` runs
the graph delete for each index it quarantines.

### MIME types and tags (`lib/enrichment/tags.go`)

`classifyFile(path, indexedType)` runs for every FILE node:

- **MIME type:** from the extension (`mime.TypeByExtension`, parameters
  dropped), else from the record's indexed type, else
  `application/octet-stream`.
- **Tags:** any of `image`, `document`, `credential`, `archive`, `script` and
  `browser artefact`, from the MIME type, extension and path. They are sorted.

## Reading

### `dues list`

`cli/cmdlist.go` opens the database read-only, then the graph read-only.

1. `store.ListWithEnrichment(db, repo, filter)` builds the listing from the
   database (`listOutput`).
2. `GrapheneRepository.IndexedFileAnnotations()` reads the FILE nodes at
   level `indexed_file`, keyed by `indexed_file_id`.
3. Each indexed file in the listing whose ID has an annotation gets
   `mimeType`, `tags` and, when known, `entropy`.

The listing used to come from the graph's evidence nodes, so evidence stored
without enrichment was missing from it (GRA-24). The graph only adds to
entries the database already lists.

- No graph (`ErrNoGraph`): the plain database listing, with no warning.
- A graph that cannot be opened: a warning, then the plain listing.
- Annotation fails: the plain listing; the failure is logged.

### `ReadHierarchy`

`GrapheneRepository.ReadHierarchy()` walks evidence → partition → indexed
file → FILE node (level `indexed_file`) and returns the tree, sorted by name.
An indexed node with no FILE node is returned from its own properties. No
command uses it now; tests do.

## Rules for maintainers

- Bump `producerVersion` in `lib/enrichment/graphene_repository.go` when
  enrichment's output changes. This matters most when only the index entries
  change: the "changes nothing" check compares properties, so without a new
  `producer_version` the change is never written.
- Never call `Compact` on the store.
- Resolve node and edge types through `Handle.Schema()`. Never derive them
  from graphene's constants.
- Never change a persisted derivation (`ids.HashText` inputs, the base64 IDs,
  property and index key names) without a migration.
- Write through a `Writer` and a `Batch`, never with loose graphene calls. A
  loose write has no commit frame, so it cannot be attributed or signed.
- Treat the database as authoritative. The graph may lag it, and can be
  rebuilt from it.

## Tests

| Behaviour | Tests |
|---|---|
| Read-only opens | `TestReadOnlyOpenOfMissingStoreCreatesNothing`, `TestReadOnlyOpensShareTheStore`, `TestReadOnlyOpenOnReadOnlyMedia`, `TestReadCommandsCreateNoGraph` (cli) |
| Lock and registry | `TestOpenOfLockedStoreIsErrGraphLocked`, `TestSharedHolderKeepsOtherHoldersInfo`, `TestRegistryKeyFoldsSpellings`, `TestSlowOpenDoesNotBlockOtherStores` |
| Marker and detection | `TestEmptyMarkerCountsAsAbsent`, `TestMarkerlessStoreOfBuiltinKindsOpens`, `TestNoAdviceToRunMissingCommands` |
| Log and versions | `TestStoreIsNeverCompacted`, `TestLargeLogIsReported`, `TestStampsNameLinkedGraphene` |
| Batch | `TestUpsertReplacesStoredNode`, `TestUpsertFillLeavesExistingNode`, `TestUpsertStubThenRecordInOneBatch`, `TestIndexEntriesCommitWithTheBatch`, `TestRepeatedUpsertWritesNothing`, `TestNodesNeedLabels` |
| Enrichment | `TestEnrichmentGraph` (`lib/ingest`), `TestEnrichAllCreatesFileNodesAtAllLevels`, `TestDeleteEvidenceIndex`, `TestClosedRepositoryRefusesWrites`, `TestEntropy` |
| Listing | `TestListShowsEvidenceTheGraphLacks` (cli) |

## Code map

| Area | Code |
|---|---|
| Handle, registry, replay warning | `lib/graph/handle.go` |
| Lock | `lib/graph/lock.go`, `lock_unix.go`, `lock_windows.go` |
| Versions and producer stamp | `lib/graph/version.go`, `lib/graph/producer.go` |
| Writer and batch | `lib/graph/writer.go`, `lib/graph/batch.go` |
| Property blobs | `lib/graph/props` |
| Order-preserving index values | `lib/graph/propindex` |
| Persisted identifiers | `lib/graph/ids` |
| Kinds, registry, marker | `lib/graph/schema` |
| Vocabulary detection | `lib/graph/migrate/detect.go` |
| Enrichment service and report | `lib/enrichment/service.go` |
| Graph writes, delete, annotations, hierarchy read | `lib/enrichment/graphene_repository.go` |
| Records and FILE identity | `lib/enrichment/types.go` |
| MIME types and tags | `lib/enrichment/tags.go` |
| Entropy | `lib/entropy`; the index pass in `lib/index` |
| Ingest and reindex callers | `lib/ingest/features.go`, `lib/ingest/reindex.go` |
| Commands | `cli/cmdenrich.go`, `cli/cmdlist.go`, `cli/cmdrepair.go` |
| Listing | `lib/store/list.go`, `lib/store/list_enrichment.go` |
