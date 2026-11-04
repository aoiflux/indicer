# DUES store: low-level design

This document describes how DUES stores evidence: how a database is opened,
what it records, and how one ingest turns a file into chunks, records and an
index. It is for maintainers and developers changing the storage code.

Related documents:

- [`CONTAINER_MODE_LLD.md`](CONTAINER_MODE_LLD.md): container files, their
  format and the legacy block index.
- [`RESTORE_LLD.md`](RESTORE_LLD.md): the read path and restore.

## Rules the code keeps

Code comments cite these rules by label.

| Label | Rule |
| --- | --- |
| I1 | No `C\|\|\|` chunk location becomes visible before the bytes it points at are synced. |
| I2 | FLUSHED means every relation and chunk location of the attempt is durable. COMPLETED means FLUSHED, plus the attempt won the content-identity commit, plus its `ER`, `EH` and `EQ` keys are written. |
| I3 | Layout comes from the manifest or the evidence record, never from command-line flags. |
| I4 | Content reads are checked against the chunk hash unless a caller turns that off (`dbio.ReadOptions.SkipVerify`). |
| I5 | Every evidence record change is one badger read-modify-write transaction, retried on conflict. |
| I6 | One ingest has exactly one outcome (stored, aliased or failed), and nothing it starts outlives it. |
| I7 | Ingest, the index pass, repair and reindex can be run again with the same result. |

Comments also cite design decisions: D1 (content identity and aliases), D5
(containers are stored decoded), D8 (no password means no encryption), D9
(the block index is retired) and D10 (materialised objects).

## The data root

A database is one directory, the data root. `--dbpath` names it; a path that
ends in `kvdb` names its parent. Without `--dbpath` the root is `~/.dues`
(`dbopen.DefaultRoot`).

| Path | Holds |
| --- | --- |
| `dues-manifest.json` | The manifest. |
| `.dues.lock` | The root lock. |
| `kvdb/` | The badger database, and `DUES-MANIFEST.mirror.json`, the manifest's mirror. |
| `blob/` | Chunk blobs (`<base64url chunk hash>.blob`) and containers (`ctr-<uuidv7>.blob`). |
| `graph/` | The graph that enrichment writes. |
| `fts/` | The full-text index. |
| `uploads/` | Part files of server uploads. |

Older databases can also hold legacy containers and `blob/blocks/*.bidx`
files (see [Older databases](#older-databases)).

## The manifest (`lib/dbmeta`)

The manifest says how the database is laid out and keyed. Every reader and
writer must agree with it, and no field of it changes once data is written.

It is stored three times:

1. `<root>/dues-manifest.json`, plain JSON with no secrets.
2. `kvdb/DUES-MANIFEST.mirror.json`, a copy. `dbmeta.Load` reads it when the
   root file is missing or unreadable.
3. The marker `M|||:format` inside badger, encrypted with the database when
   the database is encrypted. The files are checked against it.

`dbmeta.Save` writes the root file atomically (temporary file, sync, rename),
then the mirror. Format name `dues-db`, version 1 (`FormatVersion`). A
manifest with a higher version is `ErrNewerFormat`.

### Layout fields

| Field | Values | Default for a new database |
| --- | --- | --- |
| `chunk_kib` | 4 to 65536, a multiple of 4 | 256 (`--chonksize`) |
| `chunk_hash` | `blake3`, `sha3` | `blake3` (`store --hash-algo`) |
| `storage` | `file`, `container`, `container-bidx` | `file`; `container` with `--container` |
| `blob_encoding` | `v2`, `legacy-sealed`, `legacy-raw` | `v2` |
| `value_encoding` | `zstd`, `raw` | `zstd` |
| `badger_compression` | `zstd`, `snappy`, `none` | `zstd`; `none` with `--quick` |

The chunk hash names two hashes (`dbmeta.ChunkHasher`, `dbmeta.FileHasher`):

| `chunk_hash` | Chunk hash | File hash (evidence, partitions, indexed files, roots) |
| --- | --- | --- |
| `blake3` | BLAKE3, 32 bytes | BLAKE3, 32 bytes |
| `sha3` | SHA3-512, 64 bytes | SHA3-256, 32 bytes |

`Manifest.Validate` also checks that the encodings fit the encryption scheme.
`raw` values go only with `legacy-raw` blobs. `dbmeta.SameLayout` compares
layout and encryption, and treats `container` and `container-bidx` as the
same.

### Encryption

| Scheme | Used by | Keys |
| --- | --- | --- |
| `none` | A database created without a password (D8), and a legacy quick-mode database | None. New blobs are `v2`, unsealed. |
| `argon2id` | A new database created with a password | Argon2id over the password and a 16-byte random salt (`DefaultKDF`: 3 passes, 64 MiB, 4 threads). HKDF-SHA256 derives three 32-byte keys: badger's, the blob key, and a check key. |
| `sha3-legacy` | Databases written before manifests existed | An unsalted SHA3-256 of the password, used as both badger and blob key. |

For `argon2id`, the manifest holds the KDF parameters and `check`, an HMAC
of a fixed text under the check key. `dbmeta.DeriveKeys` compares it, so a
wrong password is reported before badger is opened. The manifest holds no
secret.

Passwords come from `--password-file` or `--password-stdin`; DUES reads no
environment variable, and the plain `--password` flag was removed. An empty
password is no password. A password given for an
unencrypted database prints a notice and is not used.

### Per-database registries

Code given only a `*badger.DB` reads the layout through registries that
`dbopen` fills at open:

- `dbmeta.RegisterManifest`: `dbmeta.ChunkSize`, `HashAlgo`, `ChunkHasher`,
  `FileHasher` and `UniformChunkSize` read it. A database opened without
  `dbopen` (some tests) falls back to the process-wide settings.
- `blobcodec.Register`: `blobcodec.For` returns the database's codec.
- `dbio.RegisterKVDir`: `dbio.KVDir` returns the real `kvdb` directory when
  badger was opened elsewhere (read-only media). Every path derived from the
  badger directory goes through `KVDir`, not `db.Opts().Dir`.

`dbopen.ApplyToGlobals` also sets the process-wide globals the storage code
still reads: `cnst.ChonkSize`, `cnst.HASHALGO`, `cnst.QUICKOPT` (raw
values), `cnst.CONTAINERMODE` and `cnst.LegacyBlockIndex`. The guard test
`TestRewrittenFilesAvoidLayoutGlobals` fails when a listed storage file reads
`cnst.ChonkSize`, `cnst.QUICKOPT` or `cnst.HASHALGO`.

## Opening a database (`lib/dbopen`)

`dbopen.Open(ctx, Request)` is the only way to open a database. A request
names the path, the mode (`ReadOnly` or `ReadWrite`), the credentials and the
layout the command line asked for (`dbopen.Requested`).

### Open sequence

1. Resolve the data root (`ResolveRoot`).
2. Check that a database exists (`dbmeta.HasDatabase`: a manifest, or
   `kvdb/MANIFEST` and `kvdb/KEYREGISTRY`). A read-only open of a missing
   database is `ErrNoDatabase`. So is a read-only open of a root with a
   manifest but no `kvdb/MANIFEST`: creating or resetting it did not finish.
   A writable open of a missing database creates the root, `kvdb/` and
   `blob/`.
3. Take the root lock.
4. On a writable open, check the free-space reserve, before anything is
   written.
5. Load the manifest:
   - A manifest exists: check the requested layout against it, then derive
     the keys (and check an `argon2id` password).
   - No manifest, but a legacy database: infer the manifest
     ([Older databases](#older-databases)).
   - No database: create the manifest and save it before badger exists, so
     a database keyed by a salt never exists without it.
6. Open badger with the options the manifest fixes.
7. Check the marker `M|||:format`. A marker that does not match the manifest
   is `ErrMarkerMismatch`: the message says to restore the manifest from the
   mirror. A missing marker is written, on a writable open only.
8. Register the codec and layout, apply the globals, and turn the legacy
   block-index lookup on when block-index files exist
   (`keepBlockIndexLookup`).
9. On a writable open, remove the temporary files a killed DUES process left
   in the root, `kvdb/`, `blob/` and `graph/`, and the part files in
   `uploads/`.

`Handle.Close` closes badger, removes any shadow directory, then releases
the lock.

### The root lock

`.dues.lock` covers the whole data root: badger, blobs, graph and full-text
index.

- A writable open takes it exclusively; a read-only open takes it shared.
  Several readers can share a database. No reader shares it with a writer.
- A lock already held is `ErrInUse` at once; opens do not wait.
- On read-only media the lock file may not exist and cannot be created. A
  read-only open then goes on without a lock, with a notice.

### Read-only and writable opens

| | Read-only | Writable |
| --- | --- | --- |
| Commands | `list`, `stats`, `search`, `near`, `restore`, `micro list`, `repair` without `--fix` or `--upgrade` | `store`, `reindex`, `enrich`, `micro extract`, `tui`, `server`, `repair --fix` or `--upgrade` |
| Creates a database | Never | When missing |
| Writes the manifest or marker | Never | When missing or inferred |
| Recovery of unfinished ingests | Never | Yes, unless the caller turns it off: `repair`, and the compression product's new archive (see [Recovery](#recovery-recoverevidence)) |
| Free-space check | No | Yes |
| badger options | `ReadOnly`; `BypassLockGuard` on Windows, which has no read-only badger lock; key rotation off | Normal |

A read-only open never rotates badger's data key. Badger rewrites
`KEYREGISTRY` to rotate it, and a read-only open that tries ends the process
(CFG-14).

`cli.Common` wraps `dbopen.Open` for every command. It prints the open's
notices as `dues: note: ...`. On a writable open with `recover` set it then
runs `store.RecoverIncompleteIngests`.

### Read-only media

Badger opens its `DISCARD` file for writing even on a read-only open. When it
cannot, it ends the process instead of returning an error (CFG-15). So a
read-only open first tries to open `DISCARD` for writing, which changes
nothing (`discardWritable`). When that fails:

1. `newShadow` makes a temporary directory (`dues-readonly-*`) holding links
   to every regular badger file except `DISCARD` and `LOCK`. Where links are
   refused, the files are copied.
2. Badger opens the shadow directory and makes its own `DISCARD` there.
3. `dbio.RegisterKVDir` records the real `kvdb` directory, so blobs, graph
   and full-text index are found in the data root.
4. A notice gives how much was copied, when anything was.
5. `Close` removes the shadow directory.

### A database that was not closed cleanly

A process that died leaves badger's memtable logs (`*.mem`). Badger must
truncate them before it can open read-only. A read-only open that finds them,
or that badger refuses with `ErrTruncateNeeded`, swaps its shared lock for
an exclusive one and opens writable, so badger recovers. A notice says so.
When another process holds the lock, the open fails with `ErrNeedsRecovery`
and asks for `dues repair --fix` with write access.

### Layout flags on an existing database

For an existing database, `--chonksize`, `--hash-algo` and `--container`
must match the manifest if the user set them explicitly. A mismatch is a
usage error (exit 2) that names the database's value. A flag the user did
not set is ignored. `--container` on a `container-bidx` database is not a
mismatch. `--quick` is not compared.

### The free-space reserve (`internal/diskspace`)

Badger writes its memtables and tables through memory-mapped files. On Linux,
a write to a mapping the disk can no longer back kills the process with
SIGBUS, which no recover can stop (CFG-16). DUES therefore keeps
`diskspace.Reserve`, 128 MiB (two of badger's 64 MiB memtables), free on the
database's volume while it writes:

- A writable open refuses, before anything is written, when less than the
  reserve is free (`diskspace.Check(root, 0)`).
- An ingest counts every new chunk it writes against a guard shared by the
  database (`diskspace.GuardFor(kvdir)`). The guard asks the filesystem every
  16 MiB or 250 ms. Once less than the reserve would stay free, the ingest
  stops with a `*diskspace.LowSpaceError` naming the volume, what is free,
  and why DUES keeps the reserve.
- A guard that reported low space is replaced at the next `GuardFor`, so a
  later ingest checks again.
- Server uploads keep the reserve as their headroom.
- Read-only opens are not checked.

`diskspace.IsNoSpace` recognises `ErrLow` as well as the platforms' disk-full
errors. The reserve covers a memtable and the table it is flushed to. A large
compaction writes more, and another program can still fill the disk.

## Records (`lib/structs`)

Records are MessagePack, with structs encoded as maps. New fields are
`omitempty`, so older records decode with them empty. Values are
zstd-compressed, except in a legacy database whose `value_encoding` is
`raw`. Every production decode goes through
[`internal/msgpackx`](#bounded-decoding-internalmsgpackx).

### What one stored evidence holds

- Its record, `E|||:<id>`.
- A relation per chunk offset, `R|||:<id>|||<offset>`, naming the chunk hash.
- Per distinct chunk, shared with every other object that holds it: the
  chunk metadata `C|||:<chunk hash>` and the bytes, in a blob or a container.
- Its identity keys: `ER`, `EH` and `EQ`, and an `EA` alias for each source
  path that later held the same content.
- Its index: partition records (`P`, `PX`, `PR`, `PH`), indexed records
  (`I`, `IH`, `XE`), and materialised objects (`X`, with relations of their
  own).
- With `--revrel`, a reverse relation per chunk occurrence (`RA`). With
  `--simhash`, a signature per distinct chunk (`S`).

[Key namespaces](#key-namespaces) gives every key's exact form.

### Evidence record (`structs.EvidenceFile`, `E|||:<id>`)

The key holds the evidence's raw ID, a 16-byte UUIDv7.

| Field | Meaning |
| --- | --- |
| `Name` | Display name: the base name of the path, unless the caller named it. |
| `Size` | Bytes stored. |
| `EvidenceType` | Type detected from the name and first 4096 bytes. |
| `Completed`, `Failed`, `IngestState` | Storage state (below). |
| `ContentModel` | `v2` for records written by the current ingest. |
| `ChunkSize`, `ChunkHashAlgo`, `FileHashAlgo` | The layout it was stored with: `blake3`/`blake3`, or `sha3-512`/`sha3-256`. |
| `HashStrategy` | `sync`, `async` or `hocho`. |
| `PlainHash` | File hash of the stored bytes, base64. |
| `ContentRoot` | The content root, base64: the content identity. |
| `FileHash` | `ContentRoot` for the `hocho` strategy, otherwise `PlainHash`. |
| `Fingerprint` | Cheap identity of the bytes (see [The evidence source](#the-evidence-source-libevidence)). |
| `Source` | Format (`raw`, `ewf`, `vhd`, `opaque`, `client`), container, member files and sizes, and for EWF the stored and computed digests. |
| `Features` | Optional records the ingest wrote: `revrel`, `simhash`, `fts`, `enrich`. |
| `Attempt`, `LastError` | Attempt count, and why the last attempt failed. |
| `Index` | The index state (`*structs.IndexInfo`). |
| `DuplicateOf` | For a duplicate attempt, the raw ID of the evidence holding its content. |
| `InternalObjects` | The evidence's partitions: base64 partition ID to its range `[Start, End]` (`End` inclusive). |

Readers take the layout from the manifest, which cannot change once data is
written. Of the layout fields, only `BeginAttempt` reads `ChunkSize`.

The content root is the file hash over every chunk hash in offset order,
each prefixed with its length as a 4-byte big-endian integer
(`hochoAccumulator`).

### Storage state

| State | `IngestState` | Flags | Meaning |
| --- | --- | --- | --- |
| Pending | `pending` | | The attempt is storing, or stopped before FLUSHED. |
| Failed | `pending` (or `flushed`) | `Failed` | The attempt failed; `LastError` says why. |
| Flushed | `flushed` | | Every chunk is durable; not yet committed. |
| Completed | `completed` | `Completed` | The evidence holds its content. Only completed evidence restores. |
| Duplicate | `duplicate` | | Another evidence committed the same content first. Its records are being removed. |

### Index state (`structs.IndexInfo`)

The index state is separate from the storage state: a completed evidence can
have an index that failed. `IndexInfo` holds `State`, `Reason`, `Started` and
`Finished` (Unix nanoseconds), and `Coverage` (counts of partitions, files
seen and indexed, directories, fragmented, inline, sparse, deleted,
materialised and unavailable files, symlinks, special files, and skips by
reason).

| State | Set when |
| --- | --- |
| `running` | The ingest or reindex starts its index pass. |
| `complete` | The pass, the partition hashes and the requested features all finished. |
| `partial` | A partition could not be walked whole or opened, a file's decoded content could not be stored, or a feature failed. |
| `failed` | The pass or the partition hashes failed, or storage failed while the pass ran (`storage failed`). |
| `interrupted` | Recovery found a pass still marked running. |
| `skipped` | Nothing to index. `Reason` is `not_requested` (`--no-index`), `unsupported_container`, `container_not_decoded` or `empty`. |
| `invalid` | `repair --upgrade` quarantined a legacy index (see [Older databases](#older-databases)). |

`structs.IndexPending` exists, but no code sets it.

### Partition record (`structs.PartitionFile`, `P|||:<pid>`)

- `Name` keeps the legacy form `base64(evidence ID)|||<evidence name>_p<n>`,
  or `..._dev` for a filesystem spanning the whole device. `list` groups
  partitions by it. `Path` holds the label `<evidence name>_p<n>`.
- `EvidenceID`, `Start` (offset), `Size`, `IndexedType` (filesystem type or
  `unknown`), `FileHash` (plain hash of the range).
- `MemberCount`: the indexed files, which are kept as `PX|||` keys.
- `Partition` (`PartitionMeta`): ordinal (-1 for a whole-device filesystem),
  type name, label, filesystem type, status and detail.
- The partition's report (`structs.PartitionReport`) is a record of its own,
  `PR|||:<pid>`.

### Indexed file record (`structs.IndexedFile`, `I|||:<iid>`)

- `EvidenceID`, `PartitionID`, `Path` (escaped full path, `:stream` included
  for an alternate data stream), `Stream`, `NodeKind` (`file`, `symlink`,
  `other`), `Inode`, `Seq`, `Target`, `Links`, `Prov`, `Confidence`,
  `Times` (MACB, nil where the filesystem has none), `IsDeleted`,
  `IsFragmented`.
- `Name` equals `Path`. `Start` is the evidence offset of the first data
  extent.
- `FileHash`: the plain file hash of the content, base64.
- `Entropy`: set when the pass ran for enrichment.
- `Content` (`structs.ContentRef`) says where the bytes are:

| `Content.Kind` | Content |
| --- | --- |
| `extents` | Ranges of the evidence (`Extents`); holes read as zeros. More than 65536 extents (`MaxRecordExtents`) go to `XE\|\|\|` pages. |
| `inline` | Up to 16 KiB (`MaxInlineBytes`) kept in the record. |
| `materialized` | Decoded content, or larger inline data, stored through the chunk pass as an object (`ObjectID`, `X\|\|\|`). |
| `unavailable` | Metadata only: content DUES could not read, or a symlink. |
| `none` | An empty file. |

`Located` counts the bytes described. A file found only in part has
`Located < Size`, and nothing past `Located` is filled in.

### Chunk metadata (`structs.ChonkMetadata`, `C|||:<chunk hash>`)

| Field | File mode | Container mode |
| --- | --- | --- |
| `Path` | `blob/<name>.blob` | `blob/ctr-<uuidv7>.blob` |
| `Offset` | 0 | Offset of the encoded payload |
| `OriginalSize` | Plain chunk length | Plain chunk length |
| `StoredSize`, `EncodedSize` | Blob length | Payload length |
| `Container` | false | true |

`Path` is relative to the data root, with forward slashes
(`dbio.StoredBlobPath`), so a data root can be moved or copied.

### Object and alias records

- `structs.ObjectRecord` (`X|||:<oid>`): a materialised object. Its ID is the
  indexed file's ID. It records size, chunk size, plain hash, evidence ID,
  indexed ID and what decoded it.
- `structs.AliasRecord` (`EA|||`): a later ingest of content already stored.
  It records name, path, size, source, the attempt ID (if any) and the time.

## Ingest (`lib/ingest.Run`)

`ingest.Run(ctx, db, Request)` stores one evidence. Its callers are the
server's uploads (`finalize` and `StreamFile`) and the compression product.
`dues store`, the TUI's store and `dues reindex` call the same steps through a
session (below).

### Sessions (`lib/ingest.Session`)

A session is what a run of ingests shares: the database, and the full-text
index and graph that `FTS` and `Enrich` write.

- `NewSession(db)` opens nothing. The first evidence that has files to index
  opens the full-text index (`fts.Acquire`). The first that enriches opens
  the graph. Both stay open until `Close`.
- One session lasts one `dues store` (file or folder), one TUI store job, one
  `dues reindex`, or the graph deletions of one `repair --fix`. Before
  sessions, each evidence opened and closed both stores, and each graph open
  replayed the graph's whole log.
- `Close` releases both, and does nothing the second time. The command joins
  its error to its own, as it does the database's close error (CFG-8). The
  database belongs to the caller and stays open.
- A session is safe for concurrent use. A failed open is not kept: the next
  evidence tries again. A closed session refuses `Run` and `Reindex` before
  writing anything.
- Transactions stay one per evidence. A session shares resources, not
  commits.
- `ingest.Run` and `ingest.Reindex` run in a session of their own.
- `Session.Opens()` counts the opens. `StoreData COMPLETE` and `StoreFolder
  COMPLETE` log them as `fts_opens` and `graph_opens`, at most 1 each.

| `Request` field | Effect |
| --- | --- |
| `Path`, `Name` | The file, and its display name. |
| `SourcePath` | Recorded as the source path instead of `Path` (server uploads record the client's path). |
| `Decode` | Store a recognised container decoded. `dues store` sets it; the compression product does not. |
| `NoIndex` | Store without indexing (`--no-index`). |
| `FTS`, `Enrich` | Run full-text indexing and graph enrichment after the commit (`--fts`, `--enrich`). |
| `RequireVerified` | Fail an EWF set whose stored digests do not match (`--require-verified`). |
| `Quiet` | Report no progress (`progress.Without`). |

### Steps

1. Open the source (`evidence.Open`) and compute its fingerprint with the
   database's file hash.
2. Build the evidence record (`newRecord`).
3. Look for content already stored (`findStored`, by hash strategy). When it
   is found, write an alias of that evidence (`store.WriteAlias`) and return
   `aliased`. Nothing is stored and no attempt record exists.
4. Create the attempt record, or resume an earlier one (`store.BeginAttempt`).
5. Set the index state: `running`, or `skipped` with the source's reason.
6. When indexing, start `index.Run` on its own goroutine, under its own
   cancellation. Get the partition-table ranges (`index.PartitionRanges`) for
   the chunk pass to hash.
7. Run the chunk pass (`store.IngestChunks`) over the source.
8. If storage failed: stop the index pass with the cause `storage failed`,
   wait for it, mark the attempt failed with `LastError`, and return the
   error. The attempt can be resumed.
9. Check the EWF digests the chunk pass computed. A mismatch with
   `RequireVerified` fails the attempt here, before FLUSHED.
10. Mark the attempt FLUSHED (`flush`): record `PlainHash`, `ContentRoot`,
    `FileHash` and the EWF check.
11. Wait for the index pass.
12. Commit (`store.CommitEvidence`). If another evidence committed the same
    content first, remove this attempt's records (`store.CleanupDuplicate`)
    and return `aliased`.
13. Record the index result. Set partition hashes: from the chunk pass's
    range digests, the evidence's plain hash for a whole-device filesystem,
    or by hashing the range (`store.HashContent`). Then run the requested
    features when the index is complete or partial.
14. Write the final `IndexInfo`.

An index failure never stops storage. A storage failure stops the index
pass. A failure after FLUSHED (for example in the commit) leaves the attempt
FLUSHED, as a crash would, and recovery commits it.

### Outcomes and errors

| Result | Error | `dues store` exit |
| --- | --- | --- |
| Stored, index complete or skipped | nil | 0 |
| Aliased | nil | 0 |
| Stored, index not complete | `*ingest.IndexIncompleteError` | 2 |
| Stored, EWF digests do not match | `*ingest.DigestMismatchError` with `Stored` | 2 |
| EWF digests do not match, with `--require-verified` | `*ingest.DigestMismatchError` | 1 |
| Failed | the cause (a `*store.SourceReadError` for a source read error) | 1; 130 or 143 when a signal stopped it |

A folder store (`storeFolder`) stores each item of `evidence.Plan` in turn.
A file that fails does not stop the rest. It returns an error naming the
first failure, or a warning (exit 2) when any item was stored with warnings.

### Parallel folder stores (`--store-jobs`)

`--store-jobs N` (config `store.store-jobs`) stores up to N items of a folder
at once, through `internal/workpool`. The default is 1; 0 picks
`min(4, CPUs/4)`; low-resource mode always stores one at a time. One item at a
time runs the plain loop, so a panic still reaches the crash handler.

- **Same content stays sequential.** `folderItems` opens every item, jobs at a
  time, for its decoded size. Items of one size get their fingerprint, and
  items with equal size and fingerprint form a chain: they run one after
  another, in folder order. The first is stored and the others become its
  aliases, exactly as one at a time. Without chains, two copies would both
  miss `findStored`, both store every chunk, and race at the commit (whose
  single winner, `CommitEvidence`, stays as the backstop).
- **Large items run alone.** An item of 256 MiB or more (decoded) starts when
  nothing else runs, with the full pipeline, and nothing starts beside it.
  Items after it wait, so it is never starved.
- **Machine share.** Beside others, an item's chunk pass runs
  `max(2, workers/jobs)` workers (`ChunkPass.MaxWorkers`), unless
  `--store-workers` was given, and so does the pass of each object its index
  pass stores (`index.Options.MaxWorkers`). Every pass allocates its feed
  ring when it starts, and no more slots than the source has chunks (at
  least 2), so a small file no longer takes the full 256 MiB ring.
- **Memory.** Before the first item starts, the jobs are checked against the
  budget one store's workers and queue have: a quarter of the free memory,
  at least 256 MiB. An item beside others holds two chunk passes of its size
  (its own, and one for an object its index stores; one with `--no-index`),
  each its ring plus a chunk per busy worker, and no more workers are busy
  than the ring has slots (`store.PassMemory`, at the database's chunk
  size). The worst case is the N items holding the most running
  together; large items run alone and are not counted. A number given with
  `--store-jobs` that does not fit is refused (exit 2) before anything is
  stored; `0` comes down to the most that fit, and the log says so (`Store
  jobs reduced to fit memory`).
- **Reports in folder order.** Each item writes to its own writer
  (`orderedOutput`): the first item not yet reported writes through, the
  others are held until those before them are reported. Results, failures
  and the first error are counted in folder order, so stdout and the error
  are those of a store of one at a time.
- **Errors and stops.** A failed item does not stop the others. A signal stops
  new items; running ones stop, the finished ones are reported in order, and
  the store returns the signal. A panic in an item stops the run the same
  way and exits 70 (`lifecycle.InternalError`).
- **Shared stores.** All items use the command's session (one database, one
  full-text index, one graph). Graph writes take turns (`Handle.Writer`);
  bleve batches and badger commits run side by side, and badger commits
  concurrent transactions together.
- `StoreFolder COMPLETE` logs `jobs`.

`dues reindex --store-jobs N` runs the same pool over the evidence it
selected (flag only; the config key is the store command's). The reports
come out in selection order, the counts are kept in that order, and a large
evidence runs alone, as above. Nothing is chained: what a reindex deletes and
writes (partitions, indexed records, materialised objects, full-text
documents, graph nodes) is keyed by IDs derived from its own evidence ID
(`index.PartitionID`, `index.IndexedID`, the graph's file node IDs), so two
evidence never touch the same key. The chunks they read are shared, and a
reindex never deletes chunks. Beside others, the objects an evidence's index
stores get the same share of workers, and the memory check counts one
chunk pass per evidence. `repair --reindex-failed` reindexes one at a time.

### The evidence source (`lib/evidence`)

`evidence.Open(ctx, path, Options{Decode})` returns a `*evidence.Source`, safe
for concurrent reads.

- Only a regular file is opened: opening a FIFO blocks, and a device has no
  fixed content.
- With `Decode`, `image.OpenSource` decodes an EWF set, a VHD/VHDX chain or a
  split raw set, and the source is the decoded device (D5). A container that
  fails to decode is an error, never a fallback to its raw bytes.
- A container DUES cannot decode (`image.UnsupportedContainerError`) is
  stored as its own bytes, with format `opaque` and index skip reason
  `unsupported_container`.
- Without `Decode`, every file is stored as its own bytes. A recognised
  container then gets skip reason `container_not_decoded`.
- An empty source gets skip reason `empty`.
- `Parallel()` is true for EWF and VHD sources, whose decoding costs CPU per
  chunk. Raw files are read in order by one reader.

`Fingerprint` hashes a header (`dues-fp1` and the size) and then either the
whole content, up to `FingerprintFullLimit` (4 MiB), or three 1 MiB samples:
head, middle and tail. Up to 4 MiB a fingerprint identifies content exactly.
Beyond that, equal fingerprints only nominate a candidate.

`evidence.Plan(ctx, root, decode)` groups a folder for `dues store <folder>`:
regular files only (symlinks, FIFOs and devices are skipped and reported);
the segments of an EWF set (`.e01`, `.ex01`, `.s01`) or a split raw set
(`.001`, ...) are one item, claimed by the first segment; an unreadable
directory is recorded and the walk goes on.

### Hash strategies

`store --hash-strategy` picks how an ingest finds content already stored
before it stores anything, and which hash becomes `FileHash`.

| Strategy | Lookup before storing | `FileHash` |
| --- | --- | --- |
| `sync` (default) | A hash pre-pass (`store.HashContent`) reads the whole source and computes the plain hash and content root. `store.StoredByIdentity` then looks them up. | Plain hash |
| `async` | `store.QuickCandidates` looks up evidence of the same size and fingerprint (`EQ`). Up to 4 MiB the first candidate is the match. Beyond that, the content root is computed once and compared with each candidate's. | Plain hash |
| `hocho` | As `async`. | Content root |

The commit checks identity again in every case, so two ingests racing past
the lookup still end with one winner.

## The chunk pass (`lib/store`)

`store.IngestChunks(ctx, ChunkPass) (ChunkResult, error)` stores one object:
an evidence, or a materialised object. It stores each chunk unless the
database already holds it intact, and writes the object's relations.

| `ChunkPass` field | Meaning |
| --- | --- |
| `DB` | The database. |
| `ID` | The raw ID the relations are keyed by. |
| `Source`, `Size` | An `io.ReaderAt` and its length. |
| `Parallel` | Read with several readers. |
| `Ranges` | Ranges whose plain hash to compute (partition-table entries). |
| `EWF` | The set's stored metadata, whose digests to check. |
| `Quiet` | No progress bar. |

`ChunkResult` holds the chunk count, the plain hash, the content root, the
range digests and the EWF digest result. All are computed over exactly the
bytes stored. It also holds `NewChunks` and `NewBytes`: the chunks, and their
raw bytes, that this pass stored for the first time. The run's closing line
reports them as "new", and the rest of what was read as "deduplicated".

The pass reports a `store` phase in bytes, with a `new` meter, and adds to the
command's `read` and `new` tallies (`internal/progress`). Workers add outside
the batch mutex; the counters are atomic.

A source read error is a `*store.SourceReadError` with the offset; it
matches `store.ErrSourceRead`. The pass then fails.

### Shape

Both pipelines take their shape from the database (`shapeFor`):

- Chunk size and hashes from the registered layout.
- Workers and queue depth from `cnst.GetStorePipelineTuning`: twice the CPU
  count (1 with `--low`), queue twice the workers. Container mode halves the
  workers (at least 2) and makes the queue four times the workers.
  `--store-workers` and `--store-queue` override them. On the command line,
  `appconfig.Resolve` first fits both to a memory budget: it lowers the
  defaults with a warning for large chunks, and rejects explicit values over
  the budget.
- Readers: 1, or the worker count for a parallel source.
- Buffer slots (`feedSlots`): four per worker, at most about 256 MiB of
  buffers, never fewer than 2.

Storage mode comes from `cnst.CONTAINERMODE`, which `ApplyToGlobals` sets
from the manifest. In container mode the pass opens one
`fio.ContainerManager` and closes it at the end.

### The chunk feed (`chunkfeed.go`)

The feed reads the source chunk by chunk into a fixed ring of buffer slots,
so an ingest holds a bounded amount of memory whatever the source's size.
Nothing is memory-mapped: a failing device gives a read error, not a SIGBUS.

- Chunks reach the pipeline in any order. Each consumer releases a chunk
  once, and the last release returns its buffer to the ring.
- The digest tap sees the chunks in offset order. It computes the plain hash
  of the whole, the digest of each requested range, and the EWF MD5 and
  SHA-1, so none needs a second read of the source.
- A short read is an error: the source's size was known before the read.

### Batch-owner pipeline (default)

`--pipeline batch-owner` (`ingestChunksBatchOwner`):

1. Workers take chunks from the feed. Each hashes its chunk with the
   database's chunk hash.
2. A sharded set (`shardedChunkSet`) marks the first occurrence of each
   chunk hash in this pass. For a first occurrence, the worker calls
   `materializeChunkMeta`, which skips a chunk the database already holds
   intact and otherwise encodes and writes it.
3. The worker sends a `chunkTask` (offset, hash, encoded metadata or none,
   size) to the writer.
4. One writer goroutine owns the `*badger.WriteBatch`, the chunk-metadata
   barrier and the content root. For each task it adds the hash to the
   content root, hands the metadata to the barrier, and writes the relation
   and, with `--revrel`, the reverse relation.
5. At the end the writer fires the barrier, finishes the content root and
   flushes the batch.

### Staged pipeline

`--pipeline staged` (`ingestChunksStaged`): feed, hash workers, one dedup
goroutine, then one writer. The writer collects up to
`max(1, min(128, slots/4))` chunks and stores them itself, one after another.
The batch stays well below the ring, or the feed would wait for slots the
writer waits to fill. A repeated chunk still reaches the writer: its bytes
are stored once, but every occurrence gets its relation.

### What both pipelines write

- The chunk's bytes, once per pass and only when `dbio.ChunkStored` is false.
  That is when the chunk has no location, its metadata does not decode, its
  blob is not the size the metadata records, or its container is too short
  to hold it. A re-ingest or resumed attempt therefore rewrites torn blobs
  and skips intact ones.
- `C|||:<chunk hash>`, through the barrier, for each chunk it wrote.
- `R|||:<ID>|||<offset>` for every chunk, its value the chunk hash. The
  offset is the chunk's byte offset in decimal.
- With `--revrel` (`cnst.ENABLEREVREL`), one reverse-relation key per
  occurrence: `RA|||:<chunk hash>|||<0x01><offset, 8 bytes big-endian><shard
  byte><ID>`, its value the ID. The shard is the ID's last byte modulo 16.
  Nothing is buffered or merged, so concurrent ingests never overwrite each
  other's reverse relations.
- With `--simhash` (`cnst.ENABLESIMHASH`), a SimHash signature per chunk,
  `S|||:<chunk hash>` (8 bytes). It is computed beside the pipeline by
  `simhashAsyncWriter`, which holds at most `cnst.GetMaxThreadCount()` chunk
  copies at once and skips a chunk that already has one. Its errors fail the
  pass when the pass ends.

Every new chunk is counted against the free-space guard before it is
written.

## The blob codec (`lib/blobcodec`)

A database's blob encoding is fixed at creation and recorded in the
manifest. `blobcodec.For(db)` returns its codec; a database not opened by
`dbopen` gets the legacy codec the globals imply.

| Encoding | Stored form | Checked against the chunk hash |
| --- | --- | --- |
| `v2` | 2-byte header (version 2, flags), then zstd. For an encrypted database, a 12-byte random nonce and AES-256-GCM whose additional data is the header and the chunk key. | Yes, on every decode |
| `legacy-sealed` | zstd, then AES-GCM with a nonce prepended. | No |
| `legacy-raw` | The chunk bytes as they are. | No |

`v2` binds each payload to its chunk key. A payload read at the wrong offset,
a damaged blob, or a blob sealed under another key is `blobcodec.ErrCorrupt`,
never wrong content. A sealed database refuses an unsealed chunk, and the
reverse.

`blobcodec.Verify(ckey, plain)` checks content against the hash in a chunk
key: 32 bytes is BLAKE3 or SHA3-256, 64 bytes is SHA3-512.

Encoding uses `cnst.ENCODER`, whose level `--compress-level` sets (`fast`,
`default`, `best`; default `best`). Decoding uses `cnst.DECODER`, which
returns at most `cnst.MaxDecodedBlock` (256 MiB) from one call.

## Durable blobs and the barrier

### File-mode blobs

`fio.WriteChonk` writes a chunk's encoded bytes to
`blob/<base64url(chunk hash)>.blob`:

- A new blob is created in place (`fsx.WriteNewFile`: exclusive create,
  write, sync, close; the file is removed on any failure).
- An existing blob is torn or the wrong size, since ingest writes only when
  `ChunkStored` is false. It is replaced through a temporary file and an
  atomic rename (`fsx.WriteFileAtomic`), so metadata that may still point at
  it never sees a short file.
- Writers of one blob in one process take turns. A second writer waits and
  uses the first writer's blob.
- `WriteChonk` returns only after the sync.

### Containers

In container mode each chunk is a frame in a container file. A brief
summary; [`CONTAINER_MODE_LLD.md`](CONTAINER_MODE_LLD.md) has the full
design:

- Every container is a new file, `blob/ctr-<uuidv7>.blob`, created
  exclusively. No session appends to a file another session wrote.
- A 64-byte header (`DUESCTR2`) starts the file. Each frame (`DCR2`) carries
  the chunk hash, the payload length and CRC-32C checksums, then the encoded
  chunk.
- `C|||` records the payload's offset and length.
- A failed write truncates the container back to its last complete frame and
  starts a new container.
- A container is synced when it rotates (at 1 GiB) or closes, and on
  `ContainerManager.Sync`.

### The chunk-metadata barrier (`durability.go`)

Ingest skips chunks that `C|||` says are stored. If `C|||` reached the
database before its bytes did, a power loss could make every later ingest of
the chunk trust bytes that were never written. `chunkMetaBarrier` therefore
holds `C|||` metadata back until the bytes are durable (I1).

- The writer passes every chunk to `record`. A chunk with metadata is added;
  any other only checks the delay.
- The barrier fires at 4096 pending chunks (`barrierMaxChunks`), at 64 MiB
  of their content (`barrierMaxBytes`), 250 ms after the oldest pending entry
  (`barrierMaxDelay`), and at the end of the pass.
- Firing syncs the open container (`ContainerManager.Sync`) and the blob
  directory (`fsx.SyncDir`, which does nothing on Windows), then releases
  every pending entry into the write batch.
- A failed sync releases nothing and fails the ingest.

Relation keys go to the batch at once. They need no barrier: an attempt is
trusted only once it is FLUSHED, which the ingest records after the final
fire and the batch flush. Badger runs with `SyncWrites`.

The order of one pass is therefore: chunk bytes synced, then their `C|||`
in the batch, then the batch flushed, then FLUSHED, then the commit.

## Commit and idempotence (`lib/store/commit.go`)

An evidence's content identity is its content root. Content already stored
is never stored again: a later ingest of it becomes an alias (D1).

### Identity keys

| Key | Value | Written |
| --- | --- | --- |
| `ER\|\|\|:<content root>` | Raw ID of the one COMPLETED evidence with that content | At commit |
| `EH\|\|\|:<FileHash>` | Hash lookup: members are `HM\|\|\|` keys naming `E\|\|\|:<id>` | At commit |
| `EQ\|\|\|:<size>\|\|\|<fingerprint>` | Hash lookup, as `EH` | At commit |
| `EA\|\|\|:<winner ID><sha256(path)>` | Alias record | At commit (duplicate), or by `WriteAlias` |
| `EP\|\|\|:<sha256(path)>` | Raw ID of an unfinished attempt for that source path | By `BeginAttempt`; deleted at commit |

There is one alias per evidence and source path, so storing the same file
again rewrites the same alias.

### Attempts and resume (`BeginAttempt`)

`BeginAttempt(db, path, rec)` runs in one retried transaction:

- When `EP|||` names an earlier attempt that can resume, that record carries
  on under its ID. Its attempt count goes up, `Failed` and `LastError` are
  cleared, and its state returns to pending. Chunks it already stored are
  skipped by `ChunkStored`; relations are rewritten in place.
- An attempt can resume when it is not completed and not FLUSHED, is pending
  or failed, has content model `v2`, and has the same size, fingerprint and
  chunk size.
- Otherwise a new record is written under a new UUIDv7, with attempt 1, and
  `EP|||` points at it.

### `CommitEvidence`

`CommitEvidence(db, eid)` decides, in one transaction retried on conflict
(`dbio.UpdateTxn`), what the FLUSHED attempt becomes (I2):

1. An attempt already completed or duplicate returns its first outcome.
   Any state other than FLUSHED is an error.
2. Remove the attempt's `EP|||` key, if it names this attempt.
3. Find a winner (`storedWinnerTxn`): the evidence `ER|||` names, or, for
   evidence stored without `ER`, a member of `EH|||` under the plain hash or
   the content root. The winner must be another COMPLETED evidence of the
   same size.
4. With a winner: the attempt becomes `duplicate`, `DuplicateOf` names the
   winner, and an alias is written under the winner. Outcome `aliased`.
5. Without one: the attempt becomes COMPLETED, and `ER`, `EH` (when
   `FileHash` is set) and `EQ` (when the fingerprint is set) are written.
   Outcome `stored`.

Two ingests of the same content that commit at once conflict on the `ER`
key. The retry makes the second one the duplicate, so exactly one wins.

### Duplicate cleanup (`CleanupDuplicate`)

`CleanupDuplicate(ctx, db, eid)` removes everything a duplicate attempt
wrote, in this order:

1. Its index (`DeleteEvidenceIndex`): for each partition, the indexed
   records found through the `PX|||` keys themselves (not the record's
   count), their `IH` members, extent pages and materialised objects; then
   the `PX|||` keys, the `PH` member, the `PR|||` report and the partition
   record; then the evidence's partition list.
2. Its relations and the reverse relation each implies.
3. Its file SimHash cache (`F|||:E|||:<id>`) and its record.

Chunks stay: other objects may share them, and a chunk with no relation
costs space, never correctness. The alias keeps the attempt's audit trail.
Every step deletes keys whose absence is the goal, so a cleanup that stops
part way is finished by running it again. Recovery does that for any record
still in state `duplicate`.

## The index pass (`lib/index`)

`index.Run(ctx, Options) (Result, error)` walks the decoded image with
`scan.Scan` and writes the index. It runs beside the chunk pass and reads the
same source.

| `Options` field | Meaning |
| --- | --- |
| `DB`, `EvidenceID`, `EvidenceName` | Where to write, and what to name partitions after. The evidence record must exist. |
| `Image` | The decoded evidence. |
| `Workers` | Hash workers; 0 picks one per CPU, between 2 and 16. |
| `Limits` | Scan limits (`core.Limits`). |
| `Entropy` | Also compute each file's entropy, for enrichment. |

`Result` holds the state (`complete`, or `partial` with a summary of the
problems), the coverage, the partitions written and the scan report. A
source read error, a database error or a cancelled context stops the pass
with an error. The records written stay. `Run` returns only once its workers
have stopped.

### Deterministic IDs (`ids.go`)

IDs are 16 bytes of BLAKE3, so indexing the same evidence again writes the
same keys (I7):

- `PartitionID`: the evidence ID, the partition's ordinal (-1 for a
  whole-device filesystem) and its offset.
- `IndexedID`: the evidence ID, the partition's ordinal and offset, the
  node's inode, path and deleted flag, and a collision counter. The counter
  separates two entries that hash alike (two deleted entries with one name
  and inode); each partition keeps the first 8 bytes of every ID given out.

### Partitions

- At `BeginPartition` the pass writes the partition record, with no members,
  and links it from the evidence's `InternalObjects` before any member
  exists. A pass that stops anywhere after that leaves nothing a delete
  cannot find.
- Each partition has its own write batch for its indexed records.
- At `EndPartition` the pass waits for the partition's hash jobs, flushes the
  batch, writes the final record with `MemberCount` and `PartitionMeta`, and
  writes the `PR|||` report. A partition whose status is partial or open
  error makes the index partial.
- `PartitionRanges(img)` lists the allocated partition-table entries for the
  chunk pass to hash. `SetPartitionHash` records a partition's plain hash and
  its `PH` member after the commit.

### Files

The visitor's `File` callback runs on the scan's goroutine:

- Directories and special files are counted, not indexed.
- Symlinks are indexed as `unavailable`, with their target.
- An empty file is `none`.
- An extents file is queued for a hash worker. The worker reads the content
  through `dbio.NewExtentReader` (holes as zeros), hashes it, detects the
  file type from the first 4096 bytes, and writes the record. A read error
  stops the pass.
- Inline content up to 16 KiB is kept in the record.
- Larger inline content, and content the filesystem decodes, is
  materialised (below).
- Anything else is `unavailable`.

Each record is written in this order: the `PX|||` member key, extra entries
(the object record), extent pages, the `I|||` record, then its `IH` member.
A member key therefore exists for anything a stopped pass leaves.

### Materialised content (`materialize.go`)

Content that is not a range of the evidence is stored through the chunk pass
as an object keyed by the indexed file's own ID (D10), so it is deduplicated
like any other content:

1. The member key is written in its own transaction first. A pass that stops
   while the object is stored leaves a member whose missing record tells the
   delete to remove the object's relations.
2. The decoded stream is opened. It can only be read in order, so the chunk
   pass reads it with one reader; an out-of-order read is an error.
3. `store.IngestChunks` stores it under the ID, quietly.
4. On success the pass writes `X|||:<iid>` and the record, with kind
   `materialized` and the object's plain hash.
5. When the stream cannot be opened, or a read of it fails, the record is
   metadata only and the index is partial. After a failed read the
   half-stored object is deleted first (`store.DeleteObject`). Any other
   storage error stops the pass.

## Recovery (`RecoverEvidence`)

A writable open through `cli.Common` runs `store.RecoverIncompleteIngests`,
unless its caller turns recovery off (`repair`, and the compression product
when it creates an archive). It scans the evidence records (`forEachEvidenceRecord` reads `E|||:` keys
only) and passes each one `NeedsRecovery` selects to
`store.RecoverEvidence`. `repair --fix` uses the same function.

`NeedsRecovery` selects a duplicate, an index still marked running, and an
attempt that is neither completed nor failed and is pending, FLUSHED or has
no state.

| Record | Recovery | Action |
| --- | --- | --- |
| Duplicate | Finish `CleanupDuplicate`. | `duplicate_cleaned` |
| FLUSHED, material present | Validate at the material level, then `CommitEvidence`. If it becomes a duplicate, clean it up. | `auto_completed` or `aliased` |
| FLUSHED, material missing | Mark failed with a `LastError`. | `flushed_invalid_marked_failed` |
| FLUSHED, read error | Leave it FLUSHED for the next writable open, and return the error. | none |
| Pending or no state | Mark failed (`interrupted: ...`). Storing the same file again resumes it. | `marked_failed` |
| Index marked running | Mark it `interrupted`. | `index_interrupted` |

Every change is a read-modify-write of the current record, so a record
another writer finished meanwhile is left alone. A read error on one record
does not stop the others; the errors are returned together.

### Validation levels (`ingest_validation.go`)

`store.ValidateEvidence(ctx, db, id, rec, level)` steps through an evidence's
relations, 1024 per transaction, at the database's chunk size
(`dbmeta.ChunkSize`). Each chunk is found through `dbio.LocateChunk`. It
remembers up to 4096 checked chunks, so a repeated chunk is not checked at
every occurrence.

| Level | Checks |
| --- | --- |
| `ValidateLocations` | Every offset has a relation, and every chunk a location. |
| `ValidateMaterial` | Also, by size, the blob or container holds the recorded bytes. |
| `ValidateContent` | Also, each chunk is read, decoded and checked against its hash. |

Missing or short material is `store.ErrMaterialMissing`. A chunk that does
not decode to its hash is `store.ErrMaterialCorrupt`. Any other error is a
read failure, which says nothing about the evidence. Recovery validates at
the material level (`ValidateFlushedEvidenceMaterialized`).

## `dues repair`

| Flags | Open | Does |
| --- | --- | --- |
| none | Read-only | Lists the evidence an unfinished ingest left, as JSON (`store.InspectEvidenceRepairs`). |
| `--fix` | Writable, no automatic recovery | Lists them, then recovers each with `RecoverEvidence`. |
| `--fix --reindex-failed` | Writable | Also rebuilds every index whose pass failed or was interrupted. |
| `--fix --revrel` | Writable | Also adds reverse relations to every completed evidence stored without them (`store.AddMissingReverseRelations`). |
| `--upgrade [--yes]` | Writable | Moves a legacy block index into `C\|\|\|`, then quarantines legacy container indexes (see [Older databases](#older-databases)). |
| `--verify` | Read-only unless `--fix` or `--upgrade` | Validates every completed or FLUSHED evidence at the content level. Prints `OK` or `DAMAGED` with the first problem for each. Exits 1 when any is damaged. |

`--reindex-failed` and `--revrel` without `--fix` are usage errors. The steps
run in this order: inspect or fix, upgrade and quarantine, reverse
relations, reindex, verify. After `--fix` or `--upgrade`, repair compacts
badger's value log, as `store` does (`dbio.CompactValueLog`, 30 s budget; a
failure is only logged).

Each JSON row has an action: `pending`, `flushed`, `already_failed`,
`duplicate` or `index_running` before a fix, and the recovery actions after
one.

## `dues reindex`

```
dues reindex <id|hash>... [--failed] [--all] [--source PATH] [--fts] [--enrich]
```

`dues reindex` rebuilds indexes from what is stored (`ingest.Reindex`). It
opens the database writable, with recovery.

- Targets: named evidence (a base64 ID, or a hash a lookup gives),
  `--failed` (index failed or interrupted), or `--all` (every completed,
  non-empty evidence). `--failed` and `--all` leave out invalid indexes and
  `opaque` sources.
- It refuses evidence that is not completed, has an invalid index, is an
  `opaque` source, or is empty (`ingest.ErrNotReindexable`).
- It reads the stored chunks, each checked against its hash. With
  `--source` (one evidence only) it reads that file instead; its size and
  fingerprint must match (`ingest.ErrSourceMismatch`).
- It deletes the previous index first: records, full-text documents and
  graph nodes.
- It runs the index pass, hashes the partitions, runs the features the
  evidence was stored with plus those asked for, and records the index state.
- IDs are deterministic, so the result is the index a first ingest builds.
- One target that fails does not stop the rest. An incomplete index exits 2.

## Key namespaces

Keys are written by `lib/dbio`, `lib/store` and `lib/index`; the namespaces
are in `lib/cnst/const.go`. IDs and chunk hashes are raw bytes, unless the
table says base64. File hashes, content roots and fingerprints are base64
strings.

| Key | Value |
| --- | --- |
| `M\|\|\|:format` | The manifest, as JSON (the marker). |
| `E\|\|\|:<id>` | Evidence record. |
| `EH\|\|\|:<hash>` | Evidence hash lookup (members in `HM`; a legacy msgpack list may also be the value). |
| `ER\|\|\|:<root>` | Raw ID of the evidence holding the content. |
| `EQ\|\|\|:<size>\|\|\|<fingerprint>` | Quick lookup (members in `HM`). |
| `EA\|\|\|:<winner id><sha256(path)>` | Alias record. |
| `EP\|\|\|:<sha256(path)>` | Raw ID of an unfinished attempt. |
| `P\|\|\|:<pid>` | Partition record. |
| `PH\|\|\|:<hash>` | Partition hash lookup (members in `HM`). |
| `PX\|\|\|:P\|\|\|:<pid>\|\|\|<base64 iid>` | Partition member: msgpack `InternalOffset`, the evidence range of the file's data, or `{0, -1}` for content that is not a range. |
| `PR\|\|\|:<pid>` | Partition report. |
| `I\|\|\|:<iid>` | Indexed file record. |
| `IH\|\|\|:<hash>` | Indexed file hash lookup (members in `HM`). |
| `HM\|\|\|:<lookup key>\|\|\|<member key>` | Empty: one key per hash-lookup member. |
| `X\|\|\|:<oid>` | Materialised object record. |
| `XE\|\|\|:<iid>\|\|\|<page>` | One page of an indexed file's extents. |
| `R\|\|\|:<id>\|\|\|<offset>` | Relation: the chunk hash at that offset of an evidence or object. |
| `RA\|\|\|:<chunk hash>\|\|\|...` | Reverse relation: the owner ID. |
| `C\|\|\|:<chunk hash>` | Chunk metadata. |
| `S\|\|\|:<chunk hash>` | Chunk SimHash, 8 bytes. |
| `F\|\|\|:<record key>` | Cached file SimHash, written by `near`. |
| `Я\|\|\|:`, `RAS\|\|\|:` | Legacy reverse relations; read, never written. |

One key per member means an append writes one key and reads nothing. Its
cost does not grow with the lookup, concurrent appends never conflict, and a
partition with millions of files stays under badger's value size limit.
`dbio.HashLookupMembersTxn` and `dbio.ForEachPartitionMember` read the legacy
list or map first, then the member keys, and report each member once.

Evidence record writes go through `dbio.UpdateEvidence` or `dbio.UpdateTxn`
(I5): read, change, write in one transaction. A transaction that conflicts
is retried after a short random pause, up to 32 attempts in all. A callback
that returns `dbio.ErrNoChange` writes nothing. The guard test
`TestEvidenceWritesUseUpdateEvidence` fails on a `SetFile` call outside
`SetPartitionWithMembers`.

## Bounded decoding (`internal/msgpackx`)

The MessagePack library (`vmihailenco/msgpack` v5.4.1) allocates a typed
slice at the length its header declares, so five bytes can ask for billions
of elements. The process is then killed for memory, which no recover can stop
(STO-25). A damaged database, or a crafted archive, could kill any command
that read the record.

`msgpackx.Unmarshal` first runs `msgpackx.Check`, which walks the first value
without allocating. It refuses a value (`msgpackx.ErrInvalid`) that:

- is truncated;
- uses the reserved code `0xc1`;
- has a container declaring more elements than the bytes left can hold (each
  element takes at least one byte);
- nests deeper than `MaxDepth` (64).

Only then does it decode with the library, so what the decoder allocates is
bounded by the input's size. Every production decode goes through it. The
guard test `TestDecodesGoThroughMsgpackx` fails on any other `msgpack`
decode outside tests and tools.

## Older databases

Databases written before the manifest existed are still opened and read.

### Manifest inference

A database without a manifest but with `kvdb/MANIFEST` and
`kvdb/KEYREGISTRY` is a legacy database. `inferLegacy` reconstructs its
manifest:

- **Encryption**, from badger's key registry (`dbmeta.LegacyEncryption`):
  - Unencrypted: legacy quick mode. Values `raw`, blobs `legacy-raw`, badger
    compression `snappy`. A password given is not used.
  - Encrypted: `sha3-legacy`. Values `zstd`, blobs `legacy-sealed`, badger
    compression `zstd`. No password meant the SHA3-256 of the empty
    password. `dbmeta.CheckLegacyKey` checks the key against the registry
    before badger opens.
- **Layout**, by bounded sampling (`dbmeta.InferLayout`):
  - Chunk size: the GCD of relation offsets, for the 3 most recent objects,
    at most 50,000 keys each. When objects differ, the most recent applies,
    with a notice.
  - Chunk hash: the length of up to 64 sampled relation values (32 bytes
    BLAKE3, 64 bytes SHA3).
  - Storage: `container-bidx` when `blob/blocks/*.bidx` files exist;
    `container` when one of up to 1000 sampled `C|||` records points into a
    container; otherwise `file`.
- A field it cannot infer is named in `inferred.unverified` and taken from
  the flags or the defaults. A flag cannot override what the data shows.
- A writable open saves the inferred manifest; a read-only open does not.

### Read differences

- **Chunk sizes.** Evidence in a legacy database may use different chunk
  sizes. Readers find each evidence's own (`dbio.EvidenceChunkSize`, from its
  relation keys). Validation steps at the manifest's chunk size.
- **Blob paths.** Legacy chunk metadata records absolute paths written on
  whichever machine stored them. `dbio.ResolveBlobPath` resolves such a path
  to the file of the same base name in this database's `blob/` directory,
  never to the recorded location.
- **Containers.** Legacy containers (counter names, and `.blob.zst` files
  compressed in quick mode) are read, never written. Where a later session
  appended to an earlier session's container, its recorded offsets can be
  wrong, and those chunks fail to decode.
- **Block index.** In a `container-bidx` database, chunks without `C|||`
  are found in `blob/blocks/block_*.bidx` files (`fio.LookupBlockIndex`).
  `dbio.LocateChunk` falls back to them when `cnst.LegacyBlockIndex` is set:
  by the manifest, or because block files exist. Nothing writes block-index
  files any more.
- **Lookups and members.** Hash lookups kept as one msgpack list, partition
  members kept in the record's `InternalObjects` map, and reverse relations
  kept as `Я|||` maps, `RAS|||` segments or an older text form of `RA|||`,
  are all read.
- **Record names.** Partition and indexed records without `EvidenceID`,
  `PartitionID` or `Path` carry them in `Name`. `dbio.RecordEvidenceID`,
  `RecordPartitionID` and `RecordPath` read either form.
- **Identity.** Legacy evidence has no `ER` key. The commit and
  `StoredByIdentity` also try `EH` under the plain hash and the content root.
  Legacy attempts (no content model) are never resumed.
- **Features.** Legacy evidence records no features.
  `store.HasReverseRelations` looks for the reverse relation of its first
  chunk instead.

### `repair --upgrade`

1. `store.UpgradeBlockIndex` reads every block-index record, reads the chunk
   from its container, decodes it and checks it against its hash. Only then
   does it write the chunk's `C|||` metadata, with a relative path. Chunks
   that already have `C|||` are left alone, so a second run does nothing.
   Records that fail are counted (up to 20 listed) and keep their block-index
   entry.
2. When every record moved, `Handle.SetStorage` records the database as
   `container`: the marker first, then the manifest and its mirror. A crash
   between the two still opens, because `SameLayout` treats the two storages
   as one. When any record failed, repair ends with an error, and the block
   index is kept and still used.
3. The block files are deleted with `--yes` or a `y` on a terminal, and kept
   otherwise. While they exist, the block-index lookup stays on.
4. `store.QuarantineRawContainerIndexes` marks invalid the index of every
   legacy evidence stored as the raw bytes of an EWF set or a dynamic or
   differencing VHD/VHDX: its partitions and files were parsed out of the
   container's bytes. The records stay; listings hide them, and the graph
   drops them. A fixed VHD (a raw disk and a footer) keeps its index.
   Storing the evidence again stores and indexes its decoded disk.

## Source map

| Area | Files |
| --- | --- |
| Manifest, keys, inference, registry | `lib/dbmeta/manifest.go`, `keys.go`, `legacy.go`, `registry.go` |
| Open, lock, shadow, storage change | `lib/dbopen/dbopen.go`, `storage.go` |
| Free-space reserve | `internal/diskspace/reserve.go`, `diskspace.go` |
| Records | `lib/structs/filestruct.go`, `records.go`, `chonkmetadata.go` |
| Ingest | `lib/ingest/ingest.go`, `features.go`, `reindex.go` |
| Evidence source | `lib/evidence/evidence.go`, `plan.go` |
| Chunk pass | `lib/store/chunkpass.go`, `chunkfeed.go`, `batch_owner.go`, `staged_pipeline.go`, `store.go`, `simhash.go`, `hashpass.go` |
| Blob codec | `lib/blobcodec/blobcodec.go` |
| Durability | `lib/store/durability.go`, `lib/fio/fio.go`, `lib/fio/container.go` |
| Commit and cleanup | `lib/store/commit.go`, `index_delete.go`, `lib/dbio/identity.go` |
| Index pass | `lib/index/index.go`, `ids.go`, `partition.go`, `record.go`, `hash.go`, `materialize.go` |
| Recovery, validation, repair | `lib/store/recovery.go`, `ingest_validation.go`, `repair.go`, `verify.go`, `upgrade.go`, `quarantine.go`, `revrel_add.go` |
| Locations, members, paths | `lib/dbio/locator.go`, `chunk_stored.go`, `members.go`, `blobpath.go`, `evidence_update.go`, `recordref.go`, `kvdir.go` |
| CLI | `cli/common.go`, `cli/cmdstore.go`, `cli/cmdrepair.go`, `cli/cmdreindex.go` |
| Bounded decoding | `internal/msgpackx/msgpackx.go` |
