# Container Manager (Current Implementation)

This document explains the **current** container write/read pipeline implemented in `lib/fio/container.go`.
It is intended for engineers new to the project who need a practical mental model before changing code.

---

## 1) What this component does

`ContainerManager` is responsible for storing deduplicated chunks into large container files instead of one file per chunk.

- Active file format during writes: `.blob`
- Finalized file format after close/rotation: `.blob.zst`
- Maximum uncompressed container size: `1 GB` (`MaxContainerSize`)

Each successful write returns:

- `containerPath` (path to the container file used for this write)
- `offset` (byte offset inside the **uncompressed** container stream)
- `size` (written chunk size in bytes)

The metadata is stored by DB layer as `path|offset|size` and later used for reads.

---

## 2) Where it sits in the flow

### Write path

1. `store/store.go` creates one `ContainerManager` per store operation.
2. Worker goroutines call `dbio.SetBatchChonkNode(...)`.
3. In container mode, DB layer calls `ContainerManager.WriteChunkToContainer(...)`.
4. `ContainerManager` appends processed chunk bytes to active container and returns `(path, offset, size)`.
5. DB stores metadata string `path|offset|size`.

### Read path

1. `dbio.GetChonkNode(...)` parses metadata.
2. `fio.ReadChunkFromContainer(path, offset, size, key)` reads encoded bytes.
3. If needed, it decrypts and decompresses to reconstruct original chunk.

---

## 3) Core data structures

## `WriteRequest`

Represents one write operation:

- `data []byte` : raw chunk payload
- `key []byte` : encryption key
- `responseCh chan WriteResponse` : single-response channel back to caller

> Note: `WriteChunkToContainer` still accepts `ckey` for compatibility, but current container implementation does not use it.

## `ContainerManager`

Main mutable state:

- `currentContainer string` : current active container path
- `currentFile *os.File` : open file handle for active container
- `currentOffset int64` : next append offset in active container
- `containerIndex int` : sequence counter for naming new containers
- `writeQueue chan *WriteRequest` : buffered queue (`1000`)
- `writerDone chan struct{}` : closed when writer loop exits
- `acceptMu sync.RWMutex` + `closed bool` : gates enqueue during close
- `closeOnce sync.Once` : ensures `Close()` is idempotent

---

## 4) Concurrency model (current)

The implementation uses a **single writer goroutine** plus a buffered request channel:

- Many producer goroutines call `WriteChunkToContainer` concurrently.
- Producers enqueue requests into `writeQueue`.
- One writer goroutine (`writerLoop`) consumes requests sequentially and performs all file mutation.

Why this works well:

- No file write races (single mutator)
- Predictable ordering and offsets
- Easy shutdown semantics

Shutdown behavior:

- `Close()` marks manager closed under lock and closes `writeQueue`.
- `writerLoop` drains all queued requests via `for req := range writeQueue`.
- Caller waits for `writerDone` before final container sync/close/compress.

This ensures in-flight queued writes complete before finalization.

---

## 5) Write pipeline details

`WriteChunkToContainer(data, ckey, key)`:

1. Builds `WriteRequest` and response channel.
2. Takes read lock, checks `closed` flag.
3. Enqueues request to `writeQueue`.
4. Waits on `responseCh` for result.

`doWrite(data, key)`:

1. If not `QUICKOPT`:
   - compress chunk bytes with zstd encoder (`cnst.ENCODER.EncodeAll`)
   - encrypt compressed bytes via `util.SealAES(...)`
2. If container is missing/full, calls `createNewContainer()`.
3. Appends bytes to `currentFile`.
4. Returns `(currentContainer, previousOffset, bytesWritten)`.
5. Advances `currentOffset`.

Safety checks:

- Detects short write (`io.ErrShortWrite`).
- Propagates all I/O/compression/encryption errors back to caller.

---

## 6) Container lifecycle and naming

`createNewContainer()`:

1. If an active container exists:
   - `Sync()` + `Close()` current `.blob`
   - compress it to `.blob.zst` via `compressContainer(...)`
2. Increments `containerIndex`.
3. Generates deterministic container filename from hash of `container_<index>`.
4. Ensures `BLOBS` directory exists.
5. Opens new `.blob` with append/write flags.
6. Resets offset to `0`.

`Close()`:

1. Stops accepting new writes.
2. Closes queue and waits for writer completion.
3. Syncs/closes final open `.blob`.
4. Compresses final container to `.blob.zst`.

---

## 7) Compression of finalized containers

`compressContainer(path)` currently does **streaming compression**:

- Opens source `.blob`
- Creates destination `.blob.zst`
- Streams bytes with `io.Copy(zstdWriter, src)`
- `Sync()` output and remove original `.blob`

This avoids loading full container content into memory.

---

## 8) Read pipeline details

`ReadChunkFromContainer(path, offset, size, key)` supports both formats:

### A) Uncompressed `.blob`

- Uses `ReadAt` to fetch only requested byte range.

### B) Compressed `.blob.zst`

**With cache enabled (restore sessions)**:
- First access to container:
   - If fits in memory cache: decompresses and caches in RAM (respecting LRU size limit)
   - If too large for RAM: decompresses to temporary disk file (streaming, no memory spike)
- Subsequent reads:
   - From memory cache: direct slice operation (fastest)
   - From disk cache: `ReadAt` on temp file (fast random access, no re-decompression)
- LRU eviction: automatically removes least-recently-used containers when memory cache exceeds size limit (default: 25% of available RAM, max 4GB)
- Disk cache cleanup: all temporary files removed when `DisableContainerReadCache()` is called

**Without cache (default)**:
- Opens zstd streaming decoder per chunk read
- Skips `offset` bytes via `io.CopyN(io.Discard, decoder, offset)`
- Reads exact `size` bytes via `io.ReadFull`
- Suitable for single-chunk reads or very low memory environments

Then decode payload:

- If `!QUICKOPT`: decrypt (`UnsealAES`) then decompress (`cnst.DECODER.DecodeAll`)
- If `QUICKOPT`: return raw bytes

Compatibility behavior:

- If metadata points to `.blob` but file is already compressed, function automatically tries `.blob.zst`.

---

## 9) Invariants and assumptions

- Offsets always refer to byte positions in the **container stream used at write time**.
- Container writes are append-only during ingestion.
- Metadata `path|offset|size` must be preserved exactly for retrieval.
- One `ContainerManager` instance is expected per store operation.

---

## 10) Performance characteristics (practical)

Good at:

- Fast restore via two-tier cache:
   - Hot containers in memory: zero I/O overhead
   - Large containers on disk: no repeated decompression, fast `ReadAt`
   - Bounded memory usage with LRU eviction

Current trade-offs:

- Reading from compressed containers without cache: requires streaming skip per chunk (`O(offset)` cost)
- Disk cache uses temp directory space (cleaned on restore completion)
- Memory cache limited to 25% of available RAM (max 4GB); larger containers spill to disk


## 11) File map for new contributors

- Container writer/read logic: `lib/fio/container.go`
- Write-mode routing + metadata format: `lib/dbio/dbio.go` (`SetBatchChonkNode`, `GetChonkNode`)
- Store orchestration and manager lifecycle: `lib/store/store.go`
- Constants/extensions/flags: `lib/cnst/const.go`

---

## 12) Safe extension points

If you optimize this subsystem, preserve:

1. Metadata contract: `path|offset|size`
2. Backward-compat behavior for `.blob` and `.blob.zst`
3. Close semantics (drain queued writes before final compression)
4. QUICK mode behavior (no encrypt/decrypt + minimal processing)

Recommended benchmark scenarios before/after changes:

- many small chunks (high enqueue pressure)
- large unique chunks (container rollover frequency)
- restore workload with random offsets from compressed containers
