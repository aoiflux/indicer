# Legacy hierarchical block index (BIDX)

Hierarchical mode is retired. In that mode, a container database kept its chunk locations in block index files instead of in `C|||`. This document is for developers who maintain the read path for such databases and the upgrade that moves them to plain container storage. Container storage itself is described in [CONTAINER_MODE_LLD.md](CONTAINER_MODE_LLD.md).

## Status

- New databases never use a block index. Chunk locations always go to `C|||`.
- Nothing writes or changes block files any more. The write path (`BlockManager`, its flush and its merge) is gone.
- `--hierarchical` (`-i`) still parses, so old scripts do not fail. It is hidden from help and has no effect, and it does not turn on container storage. It prints:

  ```
  dues: warning: --hierarchical: is retired and has no effect; use --container for container storage
  ```

- Existing hierarchical databases stay readable and writable. `dues repair --upgrade` turns one into a plain container database.

## How a hierarchical database is recognised

- The manifest records its storage as `container-bidx`.
- A database written before manifests existed is inferred as `container-bidx` when `blob/blocks/*.bidx` files exist (`inferStorage`, `lib/dbmeta/legacy.go`).
- `dbopen.ApplyToGlobals` sets `cnst.LegacyBlockIndex` for `container-bidx`. This turns the block index lookup on.
- Block files beside a database turn the lookup on whatever the manifest says (`Handle.keepBlockIndexLookup`). An upgrade may have kept them, or been cut short before the manifest changed.
- `--container` on a `container-bidx` database is not a layout conflict.

## File format

One block file holds the records whose chunk hash starts with one byte: `<data root>/blob/blocks/block_<xx>.bidx`, where `<xx>` is that byte in lower-case hex.

| Part | Layout |
|---|---|
| Header, 9 bytes | Magic `BIDX`, version byte `2`, record count (uint32, little endian) |
| Records, 86 bytes each, sorted by hash | Chunk hash zero-padded to 64 bytes; payload offset (int64); payload size (int64); path offset in the string table (uint32); path length (uint16) |
| String table | The container paths, as the old writer recorded them |

The offset and size locate the encoded chunk in its container, as in `C|||`. Paths are resolved with `dbio.ResolveBlobPath`, like every recorded path. An absolute path from the old writer resolves by its base name, in this database's blob directory.

## Read path (`lib/fio/blockindex.go`)

`dbio.LocateChunk` reads `C|||` first. Only when the key is missing, and the lookup is on, does it call `fio.LookupBlockIndex(dbpath, ckey)`:

1. Picks the block file from the first byte of the hash.
2. Checks the file before reading or allocating anything from it (`openBlockFile`):
   - the file is at least 9 bytes long;
   - the magic is `BIDX` and the version is `2`;
   - the records the header claims fit in the file.
3. Binary-searches the records with `ReadAt`, one 86-byte record at a time.
4. Decodes the match (`decode`). The offset must not be negative, the size must be positive, and the path must be non-empty and lie inside the string table. The path bounds are computed in uint64, so a large offset cannot wrap.

| Outcome | Error |
|---|---|
| No block file, hash not in it, or a hash of 0 or more than 64 bytes | Wraps `ErrChunkNotIndexed`. The locator turns it into `dbio.ErrChunkNotFound`. |
| A block file that fails a check | Wraps `ErrBlockIndexCorrupt` |
| A read error | Returned as it is |

The locator returns a block index record as a container location. Every caller of the locator therefore sees chunks in the block index: reads, restore, ingest validation and `dbio.ChunkStored`. Because `ChunkStored` sees them, a new ingest into a hierarchical database skips chunks the block index already holds, and writes only new chunks, to `C|||` (STO-21).

## Upgrade: `dues repair --upgrade [--yes]`

The upgrade moves every block index record into `C|||`, after checking the chunk it points at.

1. `dues repair` opens the database for writing and runs its usual inspection.
2. If there are no block files and the manifest does not say `container-bidx`, it prints `No legacy block index to upgrade.` and goes on.
3. `store.UpgradeBlockIndex(ctx, db)` reads every record of every block file (`fio.BlockIndexFiles`, `fio.ReadBlockFile`). For each record:
   - If the chunk already has `C|||` metadata, it is left alone and counted as already there. A second run therefore does nothing.
   - Otherwise the chunk is read from its container, decoded with the database's codec, and checked against its hash (`blobcodec.Verify`).
   - Only a chunk that passes gets `C|||` metadata. The path is written in the relative form, `blob/<name>`.
   - A chunk that fails is counted as damaged, and keeps only its block index entry. Up to 20 damaged records are listed.
   - The block files are not changed.
4. It prints `Block index: <files> files, <records> records: <n> moved into the database, <n> already there, <n> damaged.`
5. If any record is damaged, it lists them and fails. The block index is kept and still used.
6. If every record moved and the manifest says `container-bidx`, it records the database as `container` (`Handle.SetStorage`).
7. It deletes the block files, and then the `blocks` directory if it is empty:
   - with `--yes`, at once;
   - on a terminal, after a `y` (or `yes`) to `Delete the <n> block index files? … [y/N]`;
   - otherwise it keeps them and says that `--yes` deletes them.

What stops the upgrade with an error: a damaged block file, a database error, or a cancelled context.

The messages above, and the question, go to stderr. On stdout the `upgrade` section of the repair document holds the counts, `recordedAsContainer`, and what happened to the block files (`blockFiles`: `none`, `kept` or `deleted`); see [OPERATIONS.md](OPERATIONS.md).

`--upgrade` then also sets aside indexes that older versions built from the raw bytes of E01 and VHD/VHDX evidence (`quarantineContainerIndexes`). That step is not part of the block index upgrade.

### Crash safety of the storage change

- `Handle.SetStorage` (`lib/dbopen/storage.go`) writes the marker inside the database first, then the manifest file and its mirror.
- `dbmeta.SameLayout` treats `container` and `container-bidx` as the same layout. A crash between the two writes therefore leaves a database that opens.
- Block files kept beside an upgraded database keep the lookup on (`keepBlockIndexLookup`), so nothing is lost if `C|||` is incomplete.

## Code map

| What | Where |
|---|---|
| Read-only lookup | `lib/fio/blockindex.go`: `LookupBlockIndex`, `ReadBlockFile`, `BlockIndexFiles`, `ErrChunkNotIndexed`, `ErrBlockIndexCorrupt` |
| Locator | `lib/dbio/locator.go`: `LocateChunk` |
| Upgrade | `lib/store/upgrade.go`: `UpgradeBlockIndex`; `cli/cmdrepair.go`: `upgradeBlockIndex` |
| Storage change | `lib/dbopen/storage.go`: `SetStorage`, `keepBlockIndexLookup` |
| Flag | `cli_defs.go` (hidden flag), `internal/appconfig/appconfig.go` (warning) |

## Tests

| Test | What it checks |
|---|---|
| `TestBlockIndexLookup`, `TestBlockIndexRejectsDamage`, `FuzzLookupBlockIndex` | Lookup, and refusal of a count past the end, a path past the string table, an unknown magic and truncated records |
| `TestReingestFindsChunksInLegacyBlockIndex` | Re-ingest skips chunks the block index holds |
| `TestUpgradeBlockIndexMovesRecords`, `TestUpgradeBlockIndexKeepsDamagedRecords`, `TestUpgradeBlockIndexCancelled` | The upgrade, a damaged record, and cancellation |
| `TestSetStorage`, `TestStorageChangeInterrupted`, `TestBlockFilesKeepLegacyLookup` | The storage change and its crash safety |
| `TestRepairUpgradeMovesBlockIndex`, `TestRepairUpgradeWithoutBlockIndex` | The command, with `--yes`, confirmed, declined and without a terminal |

The `container-bidx` legacy fixture (`internal/testutil/legacyfixture`) covers reading a database that a release with hierarchical mode wrote.
