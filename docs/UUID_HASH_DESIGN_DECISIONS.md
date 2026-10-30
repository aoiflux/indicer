# IDs, hashes and content identity

This document describes how DUES names its records, which hashes each record holds, and how an ingest finds content that is already stored. It is for developers who change ingest, indexing, hash lookups, or any code that reads record keys.

How restore checks these hashes is described in [`RESTORE_LLD.md`](RESTORE_LLD.md).

## Rules

1. A record's ID is fixed when the record is created, and never changes. Hashes are fields and lookup keys, never primary keys.
2. Evidence IDs are UUIDv7. Partition and indexed-file IDs are derived from what identifies the record, so indexing again writes the same keys.
3. One content root names at most one COMPLETED evidence. A later ingest of the same content becomes an alias of it.
4. Partition and indexed records carry their evidence ID, partition ID and path as fields. Code reads them through the helpers in `lib/dbio/recordref.go`, and never splits a name itself.
5. The hash algorithms belong to the database, not to the process.

Rule 1 follows from badger. A key cannot be renamed: changing one means writing the value under the new key, deleting the old one, and rewriting every key that refers to it (relations, members, lookups). A hash that is known only at the end of an ingest therefore cannot be the key.

## Record IDs

Every ID is 16 raw bytes.

| Record | ID | Made by |
| --- | --- | --- |
| Evidence | UUIDv7 | `store.BeginAttempt` |
| Partition | BLAKE3-128 of its identity | `index.PartitionID` |
| Indexed file | BLAKE3-128 of its identity | `index.IndexedID` |
| Materialised object | the indexed file's ID | the index pass (`materialize`) |

- The keys are `E|||:<id>`, `P|||:<id>`, `I|||:<id>` and `X|||:<id>`.
- The relations of an evidence or an object are keyed by its raw ID: `R|||:<id>|||<offset>`.
- `dbio.CanonicalEvidenceKey`, `CanonicalPartitionKey` and `CanonicalIndexedKey` add the prefix when it is missing. `dbio.EvidenceRawID` removes it.
- Commands show an ID as the standard base64 of its 16 bytes, such as the `id` field of `dues list`.

### Evidence IDs

- `store.BeginAttempt` makes the ID with `uuid.NewV7()`, in the transaction that writes the record.
- UUIDv7 IDs sort by creation time, so key order is storage order.
- An attempt that stopped before its commit is resumed under the same ID, and keeps the chunks it stored.
  - `EP|||:<sha256(source path)>` names a source path's unfinished attempt.
  - `BeginAttempt` resumes that attempt when it is PENDING or FAILED, has not reached FLUSHED, uses the v2 content model, and has the same size, fingerprint and chunk size.
  - Otherwise `BeginAttempt` makes a new ID.

### Partition and indexed-file IDs

`lib/index/ids.go` derives both. Each is the first 16 bytes (`IDLen`) of a BLAKE3 hash over a domain tag and fixed fields. Integers are 8-byte big-endian, and byte strings are preceded by their length.

- `PartitionID(eid, ordinal, offset)` hashes the tag `dues-pid1`, the evidence ID, the partition's ordinal in its table, and its offset. The ordinal is -1 for a filesystem that spans the whole device.
- `IndexedID(eid, ordinal, partOffset, inode, path, deleted, collision)` hashes the tag `dues-iid1`, the evidence ID, the partition's ordinal and offset, the inode, the path, the deleted flag (one byte) and a collision count. The path includes `:stream` for an alternate data stream.

The collision count keeps apart records that would otherwise share an ID, such as two deleted entries with the same name and inode. Per partition, the index pass keeps the first 8 bytes of every ID it issued. It increments the count until the ID is new.

The inputs are the same on every pass. So `dues reindex`, a resumed ingest and recovery overwrite the same keys instead of adding copies.

### Fields on partition and indexed records

- **Indexed files** carry `EvidenceID`, `PartitionID` and `Path`, and `Name` equals `Path`.
- **Partitions** carry `EvidenceID` and `Path`. `Path` is the label: `<evidence name>_p<n>`, or `<evidence name>_dev` for a whole-device filesystem.
  - `Name` keeps the older form, `base64(evidence ID)|||<label>`, because `list` groups partitions by that prefix.
- The evidence lists its partitions by base64 ID in `InternalObjects`. A partition's members are one key each, under `PX|||:` (ING-21).
- `index.Run` writes a partition's record, and its link from the evidence, before any member.

## Hash algorithms

- The manifest fixes the chunk hash when the database is created: BLAKE3 (32 bytes) or SHA3-512 (64 bytes).
  - `--hash-algo` (`-g`) on `store` or `server` chooses it.
  - Given for an existing database that uses another algorithm, `--hash-algo` is a usage error.
- The file hash follows the chunk hash: BLAKE3, or SHA3-256 in a SHA3 database (`dbmeta.FileHasher`). Evidence, partition, indexed-file and object hashes, content roots and fingerprints all use the file hash.
- New evidence records name both algorithms: `ChunkHashAlgo` (`blake3` or `sha3-512`) and `FileHashAlgo` (`blake3` or `sha3-256`).
- Hashes are stored, printed and looked up as standard base64 with padding.

## Hash strategies

`store --hash-strategy` (`-S`) chooses one of three strategies. The default is `sync`. The spellings `synchash`, `asynchash` and `hochohash` are also accepted. The evidence records its strategy in `HashStrategy`.

Every strategy computes the same two hashes over exactly the bytes it stores, in the chunk pass (`store.IngestChunks`): the plain hash and the content root. The strategies differ in two things only:

| Strategy | Before storing, the ingest looks for stored content by | `FileHash` |
| --- | --- | --- |
| `sync` | reading the source once for both hashes (`store.HashContent`), then `store.StoredByIdentity` | plain hash |
| `async` | `store.QuickCandidates(size, fingerprint)` | plain hash |
| `hocho` | `store.QuickCandidates(size, fingerprint)` | content root |

For `async` and `hocho`:

- A source of at most 4 MiB (`evidence.FingerprintFullLimit`) matches a candidate outright, because its fingerprint covers the whole content.
- A larger source is confirmed by computing its content root and comparing it with each candidate's.
- A miss here is not final: the commit checks again (see [Ingest flow](#ingest-flow)).

## What each record's hash is

| Record | Field | Value |
| --- | --- | --- |
| Evidence | `PlainHash` | the file hash of the stored bytes |
| Evidence | `ContentRoot` | the content root; the evidence's content identity |
| Evidence | `FileHash` | `PlainHash`, or `ContentRoot` under `hocho` |
| Evidence | `Fingerprint` | the cheap identity the quick lookup uses |
| Partition | `FileHash` | the plain hash of its range |
| Indexed file | `FileHash` | the plain hash of its located content, holes read as zeros |
| Materialised object | `FileHash` | the plain hash of the decoded content |

- **Partitions.** The chunk pass hashes every partition-table range as it stores the evidence. After both passes, `index.SetPartitionHash` records each hash and its `PH` lookup. A filesystem that spans the whole device takes the evidence's plain hash. Any other partition is read and hashed (`store.HashContent`).
- **Indexed files.** An empty file (`none`) gets the hash of no bytes. A metadata-only record (`unavailable`, including symlinks) has no hash. A file whose content was only partly located is hashed over the located bytes.

### Content root

The content root (hocho) is the file hash over every chunk hash in evidence order. Each chunk hash is preceded by its length, as a 4-byte big-endian integer:

```
root = H( len(c0) ‖ c0 ‖ len(c1) ‖ c1 ‖ … )
```

Here `H` is the file hash and `cᵢ` is the hash of chunk i.

- The root depends on the chunk size and the chunk hash. The same bytes give different roots in databases with different layouts.
- For one layout, the root identifies content exactly, so it is the content identity. The commit also compares sizes.
- It costs nothing extra: the chunk pass already has every chunk hash.

The **edge form** exists only on older partition and indexed records. It is the same computation over the chunks a range touches, except that a chunk the range covers only partly, or a short last chunk, contributes the file hash of its covered bytes instead of its chunk hash. Nothing writes this form now. Restore still verifies it.

### Fingerprint

`evidence.Fingerprint` is the file hash of the tag `dues-fp1`, the size (8-byte big-endian), and then either:

- the whole content, for a source of at most 4 MiB; or
- three 1 MiB samples, at the head, the middle and the tail.

Equal content always has equal fingerprints. For a source larger than 4 MiB, an equal fingerprint only nominates a candidate.

## Content identity and aliases

The lookup keys:

- `EH|||:<FileHash>`: evidence by its recorded hash. Members are `E|||:<id>` keys.
- `ER|||:<ContentRoot>`: the one COMPLETED evidence holding that content. The value is its raw ID.
- `EQ|||:<size>|||<Fingerprint>`: evidence that may hold content of that size and fingerprint. Members are `E|||:<id>` keys. The size is decimal.
- `EA|||:<winner ID><sha256(source path)>`: an `AliasRecord`. There is one per winner and source path.
- `EP|||:<sha256(source path)>`: the raw ID of that path's unfinished attempt.
- `PH|||:<FileHash>` and `IH|||:<FileHash>`: partitions and indexed files by hash. Members are `P|||:<id>` and `I|||:<id>` keys.

The key builders are in `lib/dbio/identity.go` (`RootLookupKey`, `QuickLookupKey`, `AliasKey`, `AttemptKey`, `PathDigest`). The namespaces are in `lib/cnst/const.go`.

A hash lookup keeps one key per member: `HM|||:<lookup key>|||<record key>`, with an empty value (`dbio.AppendHashLookupUUIDByKeyTxn`). An append writes one key, so a popular hash costs the same as a rare one, and concurrent appends do not conflict (STO-22). `dbio.HashLookupMembersTxn` reads the members.

### Ingest flow

`ingest.Run` (`lib/ingest/ingest.go`) runs these steps:

1. **Look before storing.** It looks for a COMPLETED evidence with the same content, as the strategy says. If one exists, it writes an alias (`store.WriteAlias`), stores nothing, and returns the outcome `aliased` with the winner's ID.
2. **Begin the attempt.** `store.BeginAttempt` creates the record (PENDING) under a new UUIDv7, or resumes the path's unfinished attempt.
3. **Store and index.** The chunk pass stores the content, and the index pass runs beside it. Then the evidence becomes FLUSHED, with `PlainHash`, `ContentRoot` and `FileHash` set.
4. **Commit.** `store.CommitEvidence` decides what the FLUSHED evidence becomes, in one transaction that is retried on conflict:
   - It removes the path's `EP` key if that key names this attempt.
   - It looks for a winner: the evidence under `ER|||:<root>`, else the `EH` members of the plain hash, then those of the root. Older evidence has no `ER` key, and its `FileHash` was its plain hash or its root, so both are tried.
   - A winner must be COMPLETED, have the same size, and not be this attempt.
   - With a winner, the attempt becomes `duplicate` with `DuplicateOf` set, and an alias is written under the winner. The ingest then runs `store.CleanupDuplicate`.
   - Without a winner, the attempt becomes COMPLETED and is entered under `ER`, `EH` and `EQ`.
   - A second commit of the same attempt returns the first outcome.

Two ingests of one content that commit at the same time both read the `ER` key. One write succeeds. The other conflicts, retries and finds the winner. So exactly one evidence holds the content.

Recovery at a writable open commits a FLUSHED record through the same `CommitEvidence` (`store.RecoverEvidence`).

### Duplicates and aliases

- `store.CleanupDuplicate` deletes what a duplicate attempt wrote: its index records, relations, reverse relations, file SimHash, and finally its record. Each step deletes keys whose absence is the goal, so a cleanup that stopped finishes when run again. Chunks are shared, so they stay.
- An `AliasRecord` keeps the later ingest's `Name`, `Path`, `Size`, `Source` and `At`. `AttemptID` names the deleted duplicate, when there was one.
- Storing the same path again rewrites the same alias.
- `dbio.Aliases(db, winner)` lists an evidence's aliases.

## Looking up a hash or an ID

`dbio.GuessFileTypes(encoded, db)` turns a base64 string from a user into record keys. It decodes the string to bytes `D`, then probes in this order:

1. `I|||:<D>`: an indexed record with that ID
2. `P|||:<D>`: a partition with that ID
3. the members of `PH|||:<encoded>`
4. the members of `IH|||:<encoded>`
5. `E|||:<D>`: an evidence with that ID
6. the members of `EH|||:<encoded>`

It returns every match once, in that order. `GuessFileType` returns the last match. So a hash that several records share resolves to the evidence first, then an indexed file, then a partition.

- `ER` is not probed. A sync or async evidence is found by its ID or its plain hash, not by its root.
- The callers are `dues restore`, `near in` (every match counts as an exact match), `near out`, and `dues reindex`.
- `near out` also calls `store.StoredByIdentity`, so it finds the evidence that holds the file, whatever hash its strategy recorded.

## Record reference helpers

`lib/dbio/recordref.go` reads a partition or indexed record's references, from the record's fields or from its older name:

| Helper | Returns | From a new record | From an older record |
| --- | --- | --- | --- |
| `RecordEvidenceID(rec)` | the raw evidence ID | `EvidenceID` | the name's first field (`util.GetEvidenceFileHash`) |
| `RecordPartitionID(rec)` | an indexed file's raw partition ID | `PartitionID` | the name's second field, base64-decoded |
| `RecordPath(rec)` | an indexed file's path, or a partition's display name | `Path` | the name's last field; a partition's second field |

Older names have these forms:

- partition: `base64(evidence ID)|||<evidence name>_p<n>`
- indexed file: `<raw 16-byte evidence ID>|||base64(partition ID)|||<path>`

A path may itself contain `|||`, so the helpers split only at the first two separators.

The legacy parse is tolerant. A raw evidence ID is recognised by the separator at byte 16, and is checked before base64. A raw ID made only of base64-alphabet bytes would otherwise decode as base64 to the wrong ID (ING-19). A prefix that decodes to nothing is an error.

## Older databases

- **Random IDs.** Older releases gave partitions and indexed files random UUIDv7 IDs. They wrote no `EvidenceID`, `PartitionID` or `Path`, so the record reference helpers read the name. `dues reindex` deletes an evidence's index and writes it again, with deterministic IDs and explicit fields.
- **No content root.** Older evidence has no `PlainHash`, `ContentRoot`, `Fingerprint` or `ER` key. Its `FileHash` is its plain hash, or its root under `hocho`.
  - `CommitEvidence` and `StoredByIdentity` find it through `EH`, trying both hashes.
  - `EQ` does not list it, so the `async` and `hocho` lookups before storing miss it. The commit still finds it.
- **Edge-form hashes.** Some older partition and indexed records hold the edge form of the content root. Restore verifies it.
- **Lookup lists.** Older hash lookups are one msgpack list per hash, at the lookup key itself. `HashLookupMembersTxn` reads the list first, then the member keys, and reports each member once. New members are always written as separate keys.
- **Chunk size.** Older evidence records have no `ChunkSize`. Readers infer it from the relation keys (see [`RESTORE_LLD.md`](RESTORE_LLD.md#older-databases)).
