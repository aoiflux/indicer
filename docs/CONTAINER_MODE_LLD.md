# Container storage: low-level design

Container storage packs many chunks into a few large files instead of writing one blob file per chunk. This document describes how containers are written, made durable, located and read, and how containers written by older versions are still read. It is for developers changing `lib/fio`, `lib/dbio` or the chunk passes in `lib/store`.

Related documents:

- The chunk encoding is described in the package comment of `lib/blobcodec`.
- The ingest pipelines are described in [STORE_LLD.md](STORE_LLD.md).
- The retired block index is described in [HIERARCHICAL_INDEX.md](HIERARCHICAL_INDEX.md).

## Storage is part of the database layout

The manifest (`dues-manifest.json`) records the storage in `layout.storage`:

| Value | Meaning |
|---|---|
| `file` | One blob file per chunk. |
| `container` | Chunks in containers. Every chunk location is in the database. |
| `container-bidx` | Legacy. A container database written in the retired hierarchical mode. Some chunk locations are in block index files. |

- A new database gets `container` when it is created with `--container` (`-x`), and `file` otherwise (`storageOf`, `lib/dbopen/dbopen.go`). No new database gets `container-bidx`.
- An existing database keeps the storage it was created with. The storage code takes it from the manifest (`engine.For(db).Container()`), so every command reads and writes the way the database was written.
- An explicit `--container` that contradicts the manifest is a usage error, which names the database's storage (`checkRequested`). On a `container-bidx` database, `--container` is accepted.
- When `dues` creates a file-storage database with chunks smaller than 128 KiB, it recommends `--container`, in a notice from `dbopen.Open` (`lib/dbopen/dbopen.go`).

## Files on disk

All chunk material is in the blob directory, `<data root>/blob` (`util.BlobPath`).

| Path | Written now | Contents |
|---|---|---|
| `blob/ctr-<uuidv7>.blob` | Yes, in container storage | A v2 container. |
| `blob/<name>.blob` | Yes, in file storage | One encoded chunk. `<name>` is the unpadded URL-safe base64 of the hash of the chunk key. |
| `blob/<name>.blob` | No | A legacy container: encoded chunks back to back, with no header. |
| `blob/<name>.blob.zst` | No | A legacy container compressed with zstd as a whole. |
| `blob/blocks/block_<xx>.bidx` | No | A legacy block index file. |

A container is closed, and a new one started, when the next frame would take it past 1 GiB (`maxContainerSize`).

## Container format v2

Integers are little endian. CRC-32C uses the Castagnoli polynomial.

### Header (64 bytes)

| Bytes | Field |
|---|---|
| 0–7 | Magic `DUESCTR2` |
| 8–9 | Format version, `2` |
| 10–15 | Zero |
| 16–31 | Container ID: the UUIDv7 in the file name |
| 32–39 | Creation time, Unix nanoseconds |
| 40–59 | Zero |
| 60–63 | CRC-32C of bytes 0–59 |

### Frames

Each chunk is one frame. Frames follow the header and each other with no gap.

| Bytes | Field |
|---|---|
| 0–3 | Magic `DCR2` |
| 4 | Hash length, `h` |
| 5–7 | Zero |
| 8–15 | Payload length |
| 16–19 | CRC-32C of the payload |
| 20–23 | CRC-32C of bytes 0–19 followed by the hash |
| 24 onwards, `h` bytes | Chunk hash |
| After the hash | Payload |

- The hash is the chunk hash without the `C|||:` prefix: 32 bytes for BLAKE3, 64 for SHA3-512. A hash longer than 64 bytes (`maxFrameHash`) is refused.
- The payload is what the database's codec returned for the chunk (`blobcodec.For(db).Encode`).

## What `C|||` records

The key is `C|||:` followed by the chunk hash. The value is a msgpack `structs.ChonkMetadata`. It is compressed with zstd unless the database's value encoding is raw (legacy quick mode).

| Field | Container chunk | File-storage chunk |
|---|---|---|
| `path` | `blob/ctr-<uuidv7>.blob` | `blob/<name>.blob` |
| `offset` | Offset of the payload in the container | `0` |
| `original_size` | Chunk length before encoding | Chunk length before encoding |
| `stored_size` | Payload length | Blob length |
| `encoded_size` | Payload length | Blob length |
| `container` | `true` | `false` |

- The offset and size locate the payload, not the frame. A reader reads the encoded chunk directly, so the same code reads v2 and legacy containers.
- `parseChonkMetadata` rejects an empty path, a container record whose `encoded_size` is not positive, and negative sizes. The error wraps `ErrChunkMetaInvalid`.
- During ingest, `C|||` reaches the write batch only through the durability barrier (see [Sync and the durability barrier](#sync-and-the-durability-barrier)).

## Blob paths (`lib/dbio/blobpath.go`)

- New metadata records paths relative to the data root, with forward slashes: `blob/<name>` (`StoredBlobPath`). This covers file-storage blobs, containers, and records moved by `dues repair --upgrade`. A path outside the blob directory is kept as it is.
- `ResolveBlobPath(dbDir, stored)` turns a recorded path into a file:
  - A path that starts with `blob/` resolves under this database's data root.
  - Any other path was recorded by an older version, as an absolute path on the machine that wrote it. It resolves to the file with the same base name in this database's blob directory. The base name is split off at `/` or `\`, so a Windows path read on Linux also works.
- A recorded path is never opened where it points: it could name another case's blob directory (STO-10).
- Paths are resolved when they are used: `readChonk`, the logical reader, `ChunkStored`, ingest validation and `UpgradeBlockIndex`. Records keep the form they were written in.
- A data root can therefore be moved, copied, or opened from another machine.

## Writing

### One manager per chunk pass

- In container storage, each chunk pass creates one `ContainerManager` with `fio.NewContainerManager(dbio.KVDir(db))`, and closes it when the pass ends. Both pipelines do this: `ingestChunksBatchOwner` (`lib/store/batch_owner.go`) and `ingestChunksStaged` (`lib/store/staged_pipeline.go`). A close error fails the pass if nothing failed before it.
- A manager creates no file until its first write.
- Ingests that run at the same time have their own managers, so each one writes its own containers.

### The writer goroutine

- `writerLoop` is the only goroutine that touches the open container. Callers reach it through `writeQueue`, a channel with room for 1000 requests. Each request carries its own reply channel.
- `WriteChunkToContainer(ckey, encode)`:
  1. If this manager has already written `ckey`, it returns that location and does not call `encode` (the `seen` map).
  2. It calls `encode` in the caller's goroutine. Encoding runs in parallel; the writer only writes.
  3. It queues the frame and waits for the location: container path, payload offset and payload length.
  4. It remembers a successful location in `seen`. A failed write is not remembered.
- After `Close`, `WriteChunkToContainer` and `Sync` fail with `container manager is closed`.

### Writing one frame (`doWrite`)

1. Build the frame header from the hash and the payload.
2. If a container is open and the frame would take it past `maxContainerSize`, sync and close that container.
3. If no container is open, create one (`createNewContainer`):
   - a new UUIDv7 gives the name `ctr-<uuid>.blob`;
   - the file is opened with `O_CREATE|O_EXCL|O_WRONLY`;
   - the header is written at offset 0. If that fails, the file is closed and removed.
4. Write the frame header at the writer's offset, then the payload straight after it. Both are positioned writes (`writeBackend.WriteAt`), never appends.
5. Advance the writer's offset only when both writes are complete.

A container is always a new file. A session never writes to a file that another session wrote, so every offset it records is an offset in a file it created (STO-1).

### A failed or short write

- A write that returns fewer bytes than asked for is `io.ErrShortWrite`.
- If either write in step 4 fails, `abandonFrame` truncates the container to the start of the failed frame, which is the end of the last complete frame. It then syncs and closes the container. The caller gets the original error, with any clean-up error joined to it.
- The writer's offset never moves past a frame that was not fully written. A short write or a full disk therefore cannot shift a later chunk's offset (STO-3).
- The next request opens a new container. In practice the error fails the chunk pass, and the ingest with it.
- Before a chunk is encoded, ingest also charges its size to the volume's disk-space guard (`diskspace.GuardFor`). The guard fails the ingest once the volume runs low.

### Sync and the durability barrier

`ContainerManager.Sync` makes every chunk the manager has acknowledged durable:

- It is a request on the writer's queue, so it runs after every write queued before it.
- It syncs the open container.
- A container the manager rotated away from was synced when it was closed.

Ingest skips chunks that `C|||` says are stored. If metadata reached the database before its bytes were on disk, a power loss could leave metadata that every later ingest trusts, pointing at bytes that were never written (STO-5). The chunk-metadata barrier (`chunkMetaBarrier`, `lib/store/durability.go`) prevents this:

- The pipeline's single writer hands each chunk's `C|||` metadata to the barrier, not to the write batch.
- The barrier fires when any of these is reached:

  | Trigger | Value |
  |---|---|
  | Pending chunks | 4096 (`barrierMaxChunks`) |
  | Content of the pending chunks | 64 MiB (`barrierMaxBytes`) |
  | Age of the oldest pending entry | 250 ms (`barrierMaxDelay`), checked at every chunk, duplicates included |
  | End of the pass | Before the final `batch.Flush` |

- Firing runs `syncChunkMaterial`: `ContainerManager.Sync`, then `fsx.SyncDir` on the blob directory, so the names of new containers and blobs survive a power loss. `fsx.SyncDir` does nothing on Windows.
- Only after the sync does the barrier pass the pending entries to the batch. If the sync fails, nothing is released and the ingest fails.
- Relation keys go to the batch at once. An ingest is trusted only once it is FLUSHED, and that is recorded after the final fire.

In file storage, `fio.WriteChonk` syncs each blob as it writes it, and the barrier syncs only the directory.

### Close

`Close` runs once (`closeOnce`). It:

1. marks the manager closed under `acceptMu`, so no new request is queued;
2. closes the queue, and waits while the writer finishes every request already queued;
3. syncs and closes the open container, and returns that error.

## Skipping chunks already stored (`dbio.ChunkStored`)

Ingest materialises a chunk through `materializeChunkMeta` (`lib/store/staged_pipeline.go`). It skips the chunk, and records no metadata for it, when `dbio.ChunkStored` reports it stored.

`ChunkStored(ckey, db)`:

1. Finds the chunk with `LocateChunk`, so a chunk that only a legacy block index holds counts as stored (STO-21).
2. Reports `false` for missing or invalid metadata. The chunk is then written again and its metadata replaced.
3. Resolves the path with `ResolveBlobPath`.
4. For a container chunk, returns `fio.ContainerHolds(path, offset, encoded_size)`. That is true when the container is a regular file, the offset is not negative, the size is positive, and `offset + size` fits inside the file. A compressed legacy container cannot be measured without decoding it, so its presence is enough.
5. For a file-storage chunk, returns true when the blob is a regular file of exactly `stored_size` bytes.

- Only read errors are returned.
- A chunk left short by a crash or a full disk reports `false`, so the next ingest writes it again.
- The check is a stat, not a read. `dues repair --verify` reads every chunk and checks its hash.
- Within one pass, repeated chunks are caught before this check. The pipeline tracks the chunks it has seen, and the manager's `seen` map returns a location it has already written.

## Reading

### Locating a chunk (`lib/dbio/locator.go`)

`LocateChunk(ckey, db)` returns a chunk's `structs.ChonkMetadata` in one read transaction (`locateChunkTxn` inside an existing one):

1. It reads `C|||`. A value that decodes as zstd is decompressed first. The msgpack record is then parsed and checked (`parseChonkMetadata`).
2. If there is no `C|||` key and the database has a legacy block index (`engine.For(db).BlockIndex()`), it calls `fio.LookupBlockIndex`. A block index record becomes a container location, with `stored_size` and `encoded_size` both set to the record's size.

| Outcome | Error |
|---|---|
| No location anywhere | Wraps `ErrChunkNotFound`, which also matches `badger.ErrKeyNotFound` |
| Metadata that does not decode or describe a location | Wraps `ErrChunkMetaInvalid` |
| A badger read error, or a block file that is damaged or cannot be read | Returned as it is |

A badger read error is never taken for a missing key, so it never sends the lookup on to the block index.

Callers: `GetChonkNode`, `ChunkStored`, the logical reader (`lib/dbio/logical.go`, which restore uses) and ingest validation (`validateChunk`).

### Reading a chunk

`GetChonkNode(key, db)` and the logical reader both:

1. locate the chunk;
2. resolve its path;
3. read the stored bytes: `fio.ReadChunkFromContainer(path, offset, encoded_size)` for a container chunk, `fio.ReadChonk(path)` for a blob;
4. decode them with the database's codec (`blobcodec.For(db).Decode`).

The v2 blob encoding checks the decoded chunk against its hash. A payload read from the wrong place is therefore an error, not wrong content. The legacy encodings do not check it. For them, the logical reader checks each decoded chunk with `blobcodec.Verify`, unless its caller sets `ReadOptions.SkipVerify`.

Restore reads each batch of chunks grouped by container (`containerLocalityOrder`), for cache and read locality. The order in which it writes them does not change.

### Bounds checks in `ReadChunkFromContainer`

- A negative offset or size is an error.
- If the recorded file is missing and a file of the same name with `.blob.zst` in place of `.blob` exists, that compressed container is read (`resolveContainerPath`).
- For an uncompressed container, the offset and size are checked against the file's size before the buffer is allocated. Both come from metadata, which can be damaged (STO-16).
- The read does not check the container header or the frame CRCs. It reads only the payload bytes, and the codec checks them.
- Reads from compressed containers have their own bounds (see [The legacy `.blob.zst` read cache](#the-legacy-blobzst-read-cache)).

### Scanning a container (`fio.ScanContainer`)

`ScanContainer(path, fn)` walks a v2 container and calls `fn` with a `ContainerFrame{Hash, Offset, Size}` for each frame. The offset and size locate the payload, as in `C|||`. The chunk index of a container can therefore be rebuilt from the container alone.

It checks, in order:

1. The header has the magic `DUESCTR2` and a valid CRC. The version field is not checked.
2. For each frame:
   - at least 24 bytes remain;
   - the magic is `DCR2`;
   - the hash length is at most 64, and the hash and payload fit in the rest of the file (compared before the payload buffer is allocated);
   - the header CRC matches;
   - the payload CRC matches.

- The first failed check stops the scan with an error that wraps `ErrContainerCorrupt` and names the frame's offset. Frames before it have already been passed to `fn`.
- A read error, or an error from `fn`, is returned as it is.
- A legacy container has no header, so it is rejected.
- No command calls `ScanContainer` yet. `TestScanContainer` and `FuzzScanContainer` exercise it.

## Legacy containers

- Containers written by older versions have no header and no frames, and are not named `ctr-*`. They are read through the same `C|||` offsets, and never written.
- In older container databases, chunks stored by a second session can sit at offsets that do not match their metadata (STO-1). A read that checks the chunk's hash reports an error. A read that does not, such as `GetChonkNode` with a legacy encoding, can return another chunk's content. The data of those chunks cannot be recovered.
- Older versions in quick mode compressed each finished container into `<name>.blob.zst`, while `C|||` kept the `.blob` name. Container-level compression is retired for new databases: no container is compressed, renamed or replaced after it is written.

### The legacy `.blob.zst` read cache

A `.blob.zst` container is one zstd stream. It cannot be read at an offset without decoding everything before it. `lib/fio/container_cache.go` handles this.

Without the cache, `streamReadCompressedChunk` decodes from the start, discards `offset` bytes and reads `size` bytes. It first checks that `offset + size` is within the 1 GiB limit.

During a restore the cache is on:

- `restoreLogical` (`lib/store/restore.go`) calls `EnableContainerReadCache`, and `DisableContainerReadCache` when it ends. Calls nest, so concurrent restores share the cache. The last `DisableContainerReadCache` releases every entry.
- `configureRestoreCache` sets the memory budget (`SetContainerReadCacheSize`) to the database tuning's `CacheLimit()`, at most 4 GiB. `CacheLimit` is a quarter of available memory, between 64 MiB and 8 GiB. It is 64 MiB in low-resource mode, and 256 MiB when memory cannot be probed. Until a restore sets it, the budget is 2 GiB (`defaultMaxCacheSize`).
- Each container is decoded once, by the first read that needs it. Other reads of that container wait on its entry, not on a global lock. Only one decode runs at a time.
- A decoded container stays in memory if it fits in what is left of the budget. Otherwise it is spilled to a temporary file:
  - The file comes from `fsx.CreateTempDeleteOnClose`, with the pattern `dues-container-*.tmp`, in the system temporary directory.
  - On Unix the file is unlinked as soon as it is created. On Windows it is created with `FILE_FLAG_DELETE_ON_CLOSE`.
  - Decompressed evidence therefore never outlives the process, even when the process is killed.
  - With the 64 MiB budget of low-resource mode, larger containers are cached on disk.
- A failed decode is not cached. The next read tries again.
- An evicted entry is released once no read holds it. A spill file is deleted when it is closed.

| Bound | Value |
|---|---|
| Decompressed size of one container | 1 GiB (`maxDecompressedContainer`). Larger is an error: quick mode closed containers at 1 GiB, so a larger one is damage or a decompression bomb. |
| zstd decoder memory | 256 MiB (`zstd.WithDecoderMaxMemory`) |
| Containers spilled to disk at once | 4 (`maxSpilledContainers`). The least recently used is evicted first. |
| Containers held in memory | Within the budget |
| One read | `offset + size` within the decoded size |

## Write backends

Container writes go through a `writeBackend` (`lib/fio/write_backend.go`). It is chosen once per process from `dues store --io-engine` (`-I`):

| Value | Backend |
|---|---|
| `auto` (default) | Standard library positioned writes |
| `stdlib` | Standard library positioned writes |
| `io-uring` | io-uring on Linux. If the probe fails, a warning and the standard library. |

- File-storage blobs do not use the backend. They are written with `fsx.WriteNewFile`.
- Every write is positioned, so writing the same bytes at the same offset again changes nothing.
- The standard library backend loops until all bytes are written. A zero-byte write is `io.ErrShortWrite`.
- `fio.GetWriteBackendStats` returns the engines and the io-uring counters of the current process. `dues stats` prints them.

The io-uring backend (`write_backend_iouring_linux.go`):

- The queue depth comes from `--io-uring-queue-depth` (`-j`). `0` means `runtime.NumCPU() × 16`, between 256 and 4096.
- Each submission carries its offset. A short write continues at the next offset.
- `io_uring_enter` is repeated on `EINTR`, and on `EAGAIN` or `EBUSY` up to 100 times, 1 ms apart.
- An error before the kernel accepted the request wraps `errNotSubmitted`. Only then does the backend write the rest of the data with a plain positioned write.
- After any other error a write may still be in flight. The ring is closed, so the kernel cancels the request, and the error is returned. The bytes are never written twice (STO-15). Later writes use the standard library.

## Code map

| Area | Files | Main identifiers |
|---|---|---|
| Container writer and reader | `lib/fio/container.go` | `ContainerManager`, `WriteChunkToContainer`, `Sync`, `Close`, `ScanContainer`, `ContainerHolds`, `ReadChunkFromContainer` |
| Legacy compressed containers | `lib/fio/container_cache.go`, `lib/fsx/tempfile*.go` | `EnableContainerReadCache`, `SetContainerReadCacheSize`, `CreateTempDeleteOnClose` |
| Write backends | `lib/fio/write_backend*.go` | `writeBackend`, `ioUringWriteBackend`, `errNotSubmitted` |
| File-storage blobs | `lib/fio/fio.go` | `WriteChonk`, `ReadChonk` |
| Metadata and paths | `lib/dbio/dbio.go`, `lib/dbio/blobpath.go` | `MaterializeChonkNode`, `GetChonkNode`, `StoredBlobPath`, `ResolveBlobPath` |
| Locator and stored check | `lib/dbio/locator.go`, `lib/dbio/chunk_stored.go` | `LocateChunk`, `ChunkStored` |
| Durability barrier | `lib/store/durability.go` | `chunkMetaBarrier`, `syncChunkMaterial` |
| Legacy block index | `lib/fio/blockindex.go` | `LookupBlockIndex` |

## Tests

| Test | What it checks |
|---|---|
| `TestContainerSessionsNeverShareFile` | Two sessions write two different containers. |
| `TestContainerFailedWriteKeepsOffsets` | A write that stops half way, then reports disk full, leaves every later offset right. |
| `TestContainerRotation`, `TestContainerFileHeader` | Rotation at the size limit, and the header layout. |
| `TestScanContainer`, `FuzzScanContainer` | Frame checks, including a flipped payload byte. |
| `TestCompressedContainerDecodedOnce`, `TestCompressedContainerCacheBounded`, `TestCompressedContainerSizeCapped`, `TestCompressedContainerWithoutCache` | The legacy read cache. |
| `TestCreateTempDeleteOnClose`, `TestCreateTempDeleteOnCloseAfterCrash` | Spill files are gone after close, and after the process is killed. |
| `TestIoUringWriteNeverDuplicates`, `TestIoUringWriteAtRealRing` | io-uring error handling, and positioned writes on a real ring (Linux only). |
| `TestStoredBlobPathRoundTrip`, `TestResolveLegacyBlobPaths` | Relative paths, and legacy Windows, UNC, Unix and relative paths. |

Builds with `-tags faultinject` can fail a write at `container.mid_record`, between a frame's header and its payload. They can also fail at `store.chunk.before_barrier` and `store.chunk.after_barrier`, on either side of the barrier's sync and release.
