# Container Mode - Low-Level Design Document

## Table of Contents
1. [Overview](#overview)
2. [Architecture](#architecture)
3. [Components](#components)
4. [Data Structures](#data-structures)
5. [Storage Flow](#storage-flow)
6. [Retrieval Flow](#retrieval-flow)
7. [Concurrency Model](#concurrency-model)
8. [File Format Specifications](#file-format-specifications)
9. [Configuration](#configuration)
10. [Backward Compatibility](#backward-compatibility)
11. [Performance Considerations](#performance-considerations)
12. [Trade-offs](#trade-offs)

---

## Overview

### Purpose
Container Mode is an optimization for the indicer deduplication system that reduces filesystem overhead by packing multiple data chunks into large container files (max 1GB each) instead of creating one file per chunk.

### Key Benefits
- **Reduced filesystem overhead**: From ~4000 files to 1 file per GB of unique data
- **Better compression ratios**: Compressing entire containers (1GB) vs individual chunks (256KB)
- **Sequential I/O patterns**: Appending chunks to containers instead of random file creation
- **Efficient storage**: Automatic zstd compression when containers are finalized

### Use Cases
- **Optimal**: Small chunk sizes (< 128KB) where file count becomes problematic
- **Beneficial**: Large datasets with high deduplication where unique data is limited
- **Standard**: Default 256KB chunks when filesystem can handle the file count

---

## Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Store Command                            │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│              storeEvidenceData()                             │
│  • Creates ContainerManager (if CONTAINERMODE enabled)      │
│  • Spawns multiple storeWorker() goroutines                 │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│                   storeWorker()                              │
│  • Reads chunk from memory-mapped file                      │
│  • Computes hash                                             │
│  • Calls processChonk()                                      │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│                  processChonk()                              │
│  • Checks if chunk exists (deduplication)                   │
│  • If new: calls SetBatchChonkNode()                        │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│              SetBatchChonkNode()                             │
│  Container Mode:                 Original Mode:              │
│  • WriteChunkToContainer()      • WriteChonk()              │
│  • Store "path|offset|size"     • Store filepath            │
└─────────────────┬───────────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────────┐
│              ContainerManager                                │
│  • Mutex-protected sequential writes                        │
│  • Auto-creates new containers when full                    │
│  • Compresses containers on finalization                    │
└─────────────────────────────────────────────────────────────┘
```

### Component Interaction

```
┌─────────────┐        ┌──────────────┐       ┌─────────────┐
│  store.go   │───────>│  dbio.go     │──────>│   fio.go    │
│             │ chunks │              │ write │             │
│ ThreadIO    │        │ SetBatch     │       │ Container   │
│ Management  │        │ ChonkNode    │       │ Manager     │
└─────────────┘        └──────────────┘       └─────────────┘
       │                      │                      │
       │                      │                      │
       ▼                      ▼                      ▼
┌─────────────┐        ┌──────────────┐       ┌─────────────┐
│  Badger DB  │        │  Metadata    │       │  Container  │
│  (K/V pairs)│        │  Storage     │       │   Files     │
│             │        │              │       │  .blob.zst  │
└─────────────┘        └──────────────┘       └─────────────┘
```

---

## Components

### 1. ContainerManager (`lib/fio/container.go`)

**Responsibility**: Manages writing chunks to container files with thread-safe sequential access.

**Key Fields**:
```go
type ContainerManager struct {
    mutex            sync.Mutex  // Ensures thread-safe writes
    currentContainer string      // Path to active container
    currentFile      *os.File    // Open file handle
    currentOffset    int64       // Current write position
    dbpath           string      // Database directory path
    containerIndex   int         // Container sequence number
}
```

**Key Methods**:

#### `NewContainerManager(dbpath string) *ContainerManager`
- Initializes a new container manager
- Sets initial container index to 0
- No files created until first write

#### `WriteChunkToContainer(data, ckey, key []byte) (string, int64, int64, error)`
- **Thread-safe**: Protected by mutex
- **Pre-processing**: Compresses (zstd) and encrypts (AES) chunk data
- **Space check**: Creates new container if current is full or chunk won't fit
- **Write**: Appends processed data to current container
- **Returns**: `(containerPath, offset, sizeWritten, error)`

#### `createNewContainer() error`
- Closes and compresses previous container
- Generates unique container filename using hash
- Creates new `.blob` file in `BLOBS/` directory
- Resets offset to 0

#### `compressContainer(containerPath string) error`
- Reads entire uncompressed container file
- Compresses using zstd encoder (`cnst.ENCODER`)
- Writes to `.blob.zst` file
- Deletes original `.blob` file
- Atomic operation to prevent corruption

#### `Close() error`
- Syncs and closes current file handle
- Compresses the final container
- Called via `defer` in storage flow

### 2. Database Layer (`lib/dbio/dbio.go`)

#### `SetBatchChonkNode(key, data []byte, db *badger.DB, batch *badger.WriteBatch, containerMgr *fio.ContainerManager) error`

**Dual-mode implementation**:

```go
if containerMgr != nil {
    // CONTAINER MODE
    containerPath, offset, size := containerMgr.WriteChunkToContainer(...)
    metadata := fmt.Sprintf("%s|%d|%d", containerPath, offset, size)
    return SetBatchNode(key, []byte(metadata), batch)
} else {
    // ORIGINAL MODE
    cfpath := fio.WriteChonk(...)
    return SetBatchNode(key, cfpath, batch)
}
```

**Database Key-Value Mapping**:
- **Key**: `C|||:<chunk_hash>` (chunk namespace + SHA3-512 hash)
- **Value (Container)**: `"/path/to/container.blob.zst|offset|size"`
- **Value (Original)**: `"/path/to/chunk.blob"`

#### `GetChonkNode(key []byte, db *badger.DB) ([]byte, error)`

**Intelligent format detection**:

```go
metadata := GetNode(key, db)
parts := strings.Split(string(metadata), "|")

if len(parts) == 3 {
    // New format: container mode
    return ReadChunkFromContainer(parts[0], offset, size, key)
} else {
    // Old format: direct file path
    return ReadChonk(metadata, key)
}
```

### 3. Storage Coordinator (`lib/store/store.go`)

#### `storeEvidenceData(infile structs.InputFile) error`

**Container Manager Lifecycle**:

```go
var containerMgr *fio.ContainerManager
if cnst.CONTAINERMODE {
    containerMgr = fio.NewContainerManager(db.Opts().Dir)
    defer containerMgr.Close()  // Ensures cleanup and compression
} else {
    containerMgr = nil
}

tio.ContainerMgr = containerMgr  // Passed to worker goroutines
```

**Worker Pool Pattern**:
- Spawns concurrent `storeWorker()` goroutines
- Limited by `cnst.GetMaxThreadCount()` (typically CPU count)
- Each worker processes chunks independently
- Shares single ContainerManager (mutex-protected)

#### `processChonk(cdata, chash []byte, db *badger.DB, batch *badger.WriteBatch, containerMgr *fio.ContainerManager) error`

**Deduplication Check**:
1. Check if chunk hash exists in database
2. If exists: skip (already stored)
3. If new: write via `SetBatchChonkNode()`

---

## Data Structures

### ThreadIO Structure

```go
type ThreadIO struct {
    Index         int64                    // Current chunk position in file
    ChonkEnd      int64                    // End position of chunk
    MappedFile    mmap.MMap                // Memory-mapped source file
    FHash         []byte                   // File hash
    DB            *badger.DB               // Database handle
    Batch         *badger.WriteBatch       // Batch write handle
    Err           chan error               // Error channel for goroutines
    ContainerMgr  *fio.ContainerManager    // Shared container manager (or nil)
}
```

### Metadata Format

#### Container Mode
```
Format: "containerPath|offset|size"
Example: "/data/BLOBS/Ax7bQ2.blob.zst|268435456|262144"
         └── path ──┘ └─ offset ─┘ └─size─┘
```

- **containerPath**: Absolute path to container file
- **offset**: Byte offset where chunk starts (int64)
- **size**: Size of chunk in bytes after compression/encryption (int64)

#### Original Mode
```
Format: "filePath"
Example: "/data/BLOBS/Bx9cD4f5g6h7i8j9k0l1m2n3o4.blob"
```

---

## Storage Flow

### Detailed Sequence Diagram

```
User          CLI          Store         Worker        ContainerMgr      Disk
 │             │             │             │                │             │
 │─store cmd──>│             │             │                │             │
 │             │─StoreData──>│             │                │             │
 │             │             │             │                │             │
 │             │             │─create mgr──────────────────>│             │
 │             │             │             │                │             │
 │             │             │─spawn──────>│                │             │
 │             │             │─spawn──────>│                │             │
 │             │             │─spawn──────>│                │             │
 │             │             │             │                │             │
 │             │             │             │─read chunk────>│             │
 │             │             │             │─compute hash──>│             │
 │             │             │             │                │             │
 │             │             │             │─write chunk───>│             │
 │             │             │             │                │─[mutex]     │
 │             │             │             │                │─compress    │
 │             │             │             │                │─encrypt     │
 │             │             │             │                │─check space │
 │             │             │             │                │             │
 │             │             │             │                │─append─────>│
 │             │             │             │                │<────ok──────│
 │             │             │             │<─path|off|sz───│             │
 │             │             │             │                │             │
 │             │             │             │─store metadata>│             │
 │             │             │             │  (to badgerDB) │             │
 │             │             │             │                │             │
 │             │             │<─done───────│                │             │
 │             │             │<─done───────│                │             │
 │             │             │<─done───────│                │             │
 │             │             │             │                │             │
 │             │             │─close mgr──────────────────>│             │
 │             │             │             │                │─sync file   │
 │             │             │             │                │─compress────>│
 │             │             │             │                │  container  │
 │             │             │             │                │<────ok──────│
 │             │<─success────│             │                │             │
 │<─success────│             │             │                │             │
```

### Step-by-Step Process

1. **Initialization**
   - User executes: `indicer store --container file.dd`
   - CLI parses flags, sets `cnst.CONTAINERMODE = true`
   - Opens database connection
   - Memory-maps input file

2. **Container Manager Creation**
   - `NewContainerManager()` initializes manager
   - No files created yet (lazy initialization)
   - Mutex ready for concurrent access

3. **Worker Pool Dispatch**
   - Creates worker pool (size = CPU count)
   - Each worker assigned chunk range
   - All workers share single ContainerManager

4. **Chunk Processing** (per worker)
   ```
   a. Read chunk from mmap (zero-copy)
   b. Compute SHA3-512 hash
   c. Check if chunk exists (deduplication)
   d. If new chunk:
      - Call ContainerManager.WriteChunkToContainer()
      - Store metadata in BadgerDB
   ```

5. **Container Write** (mutex-protected)
   ```
   a. Acquire mutex lock
   b. Compress chunk with zstd
   c. Encrypt chunk with AES-256
   d. Check if chunk fits in current container
      - If no: finalize current, create new
   e. Append to current container
   f. Update offset tracker
   g. Release mutex
   h. Return (path, offset, size)
   ```

6. **Container Finalization**
   - When container reaches ~1GB or new container needed:
     ```
     a. Sync file writes (flush OS buffers)
     b. Close file handle
     c. Read entire container
     d. Compress with zstd level 15
     e. Write to .blob.zst file
     f. Delete original .blob file
     ```

7. **Cleanup**
   - Close final container
   - Compress final container
   - Close database
   - Unmap file

---

## Retrieval Flow

### Sequence Diagram

```
User          CLI         Restore       DBIO          FIO           Disk
 │             │            │             │            │             │
 │─restore────>│            │             │            │             │
 │  cmd        │            │             │            │             │
 │             │─Restore───>│             │            │             │
 │             │  Data      │             │            │             │
 │             │            │─get chunk─>│            │             │
 │             │            │  key       │            │             │
 │             │            │            │─GetChonk──>│             │
 │             │            │            │  Node      │             │
 │             │            │            │            │─read meta   │
 │             │            │            │            │─parse "|"   │
 │             │            │            │            │             │
 │             │            │            │            │─open file──>│
 │             │            │            │            │<─.blob.zst──│
 │             │            │            │            │             │
 │             │            │            │            │─decompress  │
 │             │            │            │            │  container  │
 │             │            │            │            │             │
 │             │            │            │            │─extract     │
 │             │            │            │            │  chunk at   │
 │             │            │            │            │  offset     │
 │             │            │            │            │             │
 │             │            │            │            │─decrypt     │
 │             │            │            │            │─decompress  │
 │             │            │            │            │             │
 │             │            │            │<─chunk─────│             │
 │             │            │<─chunk─────│            │             │
 │             │            │─write to──────────────────────────────>│
 │             │            │  output                               │
 │             │<─success───│                                       │
 │<─restored───│                                                    │
```

### Detailed Steps

1. **Metadata Retrieval**
   ```go
   metadata := db.Get("C|||:<chunk_hash>")
   // Container: "/path/file.blob.zst|268435456|262144"
   // Original:  "/path/file.blob"
   ```

2. **Format Detection**
   ```go
   parts := strings.Split(metadata, "|")
   if len(parts) == 3 {
       // Container format
   } else {
       // Original format (backward compatibility)
   }
   ```

3. **Container Read** (Container Format)
   ```
   a. Extract: containerPath, offset, size
   b. Check if file exists:
      - Try .blob.zst (compressed)
      - Fallback to .blob (uncompressed)
   c. Read entire container file
   d. Decompress if .zst extension
   e. Extract chunk: data[offset:offset+size]
   f. Decrypt chunk (AES-256)
   g. Decompress chunk (zstd)
   h. Return raw data
   ```

4. **Optimization: Container Caching** (Future Enhancement)
   - Currently: Reads entire container for each chunk
   - Opportunity: LRU cache of decompressed containers
   - Trade-off: Memory vs. I/O performance

---

## Concurrency Model

### Thread Safety Strategy

#### ContainerManager Mutex Protection

```go
type ContainerManager struct {
    mutex sync.Mutex  // Protects all fields below
    currentContainer string
    currentFile      *os.File
    currentOffset    int64
    // ...
}

func (cm *ContainerManager) WriteChunkToContainer(...) {
    cm.mutex.Lock()         // Acquire exclusive access
    defer cm.mutex.Unlock() // Release on return (even if error)

    // Critical section: only one goroutine at a time
    // - Check space
    // - Write data
    // - Update offset
}
```

**Why Single Mutex**:
- Containers require sequential writes (append-only)
- Parallel writes would corrupt file structure
- Mutex overhead is minimal vs. I/O time

**Alternatives Considered**:
1. **Per-container mutex**: Complex, minimal benefit (single active container)
2. **Lock-free queue**: Over-engineered for append-only pattern
3. **Multiple containers**: Requires complex load balancing

### Worker Pool Concurrency

```
┌─────────────────────────────────────────────────────────┐
│                    Main Goroutine                        │
│  • Memory-maps file                                      │
│  • Creates ContainerManager (shared)                     │
│  • Spawns N workers (N = CPU count)                      │
│  • Waits for completion                                  │
└────────────────┬────────────────────────────────────────┘
                 │
        ┌────────┴────────┬────────────┬─────────────┐
        ▼                 ▼            ▼             ▼
  ┌──────────┐      ┌──────────┐  ┌──────────┐  ┌──────────┐
  │ Worker 1 │      │ Worker 2 │  │ Worker 3 │  │ Worker N │
  │          │      │          │  │          │  │          │
  │ Chunks   │      │ Chunks   │  │ Chunks   │  │ Chunks   │
  │ 0-256KB  │      │ 256-512  │  │ 512-768  │  │ ...      │
  └────┬─────┘      └────┬─────┘  └────┬─────┘  └────┬─────┘
       │                 │            │            │
       └────────┬────────┴────────┬───┴────────────┘
                ▼                 ▼
         ┌──────────────────────────────┐
         │   ContainerManager (Mutex)   │
         │   • Sequential writes        │
         │   • Thread-safe operations   │
         └──────────────────────────────┘
```

**Parallelism Benefits**:
- CPU: Hash computation, compression, encryption
- I/O: Memory-map reads (parallel from OS page cache)

**Serialization Point**:
- ContainerManager writes (unavoidable for correctness)

### Error Handling

```go
tio.Err = make(chan error, cnst.GetMaxThreadCount())

// Each worker sends result
storeWorker(tio) {
    err := processChunk(...)
    tio.Err <- err  // Non-blocking (buffered channel)
}

// Main goroutine collects results
for active > 0 {
    err := <-tio.Err
    if err != nil {
        return err  // Fast-fail on first error
    }
    active--
}
```

---

## File Format Specifications

### Container File Structure

#### Uncompressed Container (`.blob`)

```
┌─────────────────────────────────────────────────────────┐
│ Offset 0: Chunk 1 (compressed + encrypted)              │
│           Size: Variable (e.g., 215,423 bytes)           │
├─────────────────────────────────────────────────────────┤
│ Offset 215,423: Chunk 2 (compressed + encrypted)        │
│                 Size: Variable (e.g., 198,765 bytes)     │
├─────────────────────────────────────────────────────────┤
│ Offset 414,188: Chunk 3 (compressed + encrypted)        │
│                 Size: Variable                           │
├─────────────────────────────────────────────────────────┤
│ ...                                                      │
├─────────────────────────────────────────────────────────┤
│ Offset ~1GB: Last chunk                                 │
└─────────────────────────────────────────────────────────┘

Total Size: ≤ 1GB (1,073,741,824 bytes)
Format: Raw binary append-only
No header, no footer, no index
```

**Key Properties**:
- **No metadata**: Pure data storage, metadata in database
- **Append-only**: Sequential writes, no random updates
- **Variable chunk sizes**: Compression creates different sizes
- **No alignment**: Chunks packed tightly, no padding

#### Compressed Container (`.blob.zst`)

```
┌─────────────────────────────────────────────────────────┐
│ ZSTD Frame Header                                        │
├─────────────────────────────────────────────────────────┤
│ Compressed Block 1                                       │
├─────────────────────────────────────────────────────────┤
│ Compressed Block 2                                       │
├─────────────────────────────────────────────────────────┤
│ ...                                                      │
├─────────────────────────────────────────────────────────┤
│ ZSTD Frame Checksum                                      │
└─────────────────────────────────────────────────────────┘

Original Size: ~1GB
Compressed Size: Varies (typically 40-80% depending on data)
Compression Level: 15 (zstd.SpeedBestCompression)
```

**Creation Process**:
```go
// 1. Read entire uncompressed container
data := io.ReadAll(srcFile)  // ~1GB in memory

// 2. Compress
compressed := cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))

// 3. Write to .zst file
dstFile.Write(compressed)

// 4. Delete original .blob
os.Remove(srcFile)
```

### Chunk Format (Inside Container)

#### Layer 1: Raw Chunk Data
```
Original chunk from file: 256KB raw bytes
```

#### Layer 2: After Compression
```
ZSTD compressed: ~40-200KB (varies by entropy)
```

#### Layer 3: After Encryption
```
AES-256-GCM encrypted: compressed_size + 28 bytes (GCM overhead)
```

#### Final Format in Container
```
[Encrypted(Compressed(RawData))] + [Next Chunk] + ...
```

### Database Key-Value Format

#### Chunk Metadata Entry

**Key Structure**:
```
Namespace: "C|||:"
Hash: SHA3-512 (64 bytes)
Full Key: "C|||:" + base64(hash)
```

**Value Structure (Container Mode)**:
```
Format: ASCII string
Pattern: "/absolute/path/to/container.blob.zst|offset|size"
Example: "/opt/indicer/data/BLOBS/Ax7bQ2n.blob.zst|268435456|262144"

Fields:
  - containerPath: UTF-8 string, absolute path
  - offset: int64, decimal string representation
  - size: int64, decimal string representation
Delimiter: "|" (pipe character, ASCII 124)
```

**Value Structure (Original Mode)**:
```
Format: ASCII string
Pattern: "/absolute/path/to/chunk.blob"
Example: "/opt/indicer/data/BLOBS/Bx9cD4f5g6h7i8j9k0l1m2n3o4.blob"

No delimiters, single path string
```

### Container Naming Convention

```
Pattern: {base64_hash[:25]}.blob[.zst]

Examples:
  Uncompressed: Ax7bQ2n3p5q7r9s1t3u5v.blob
  Compressed:   Ax7bQ2n3p5q7r9s1t3u5v.blob.zst

Generation:
  1. Create unique string: "container_{index}"
  2. Hash with SHA3-512
  3. Base64 encode (URL-safe, no padding)
  4. Truncate to 25 characters
  5. Append extension
```

---

## Configuration

### Global Flags

```go
// lib/cnst/const.go
var CONTAINERMODE bool  // Enable container storage mode

// main.go
containerMode := app.Flag(
    cnst.FlagContainerMode,
    "Use container-based storage (packs multiple chunks into 1GB containers)"
).Short(cnst.FlagContainerModeShort).Default("false").Bool()
```

### Constants

```go
// lib/fio/container.go
const MaxContainerSize = 1 * cnst.GB  // 1,073,741,824 bytes

// lib/cnst/const.go
const (
    BLOBSDIR    = "BLOBS"         // Directory for blob storage
    BLOBEXT     = ".blob"         // Uncompressed container extension
    BLOBZSTEXT  = ".blob.zst"     // Compressed container extension
    FileNameLen = 25              // Length of generated filenames
)

const DefaultChonkSize = 256 * KB  // Default chunk size
```

### Runtime Detection

```go
// cli/common.go
func common(chonkSize int, ...) {
    if chonkSize < 128 && !cnst.CONTAINERMODE {
        color.Yellow("⚠️  RECOMMENDATION: You're using a chunk size of %dKB (< 128KB).", chonkSize)
        color.Yellow("   Consider using --container flag for better filesystem efficiency.")
    }
}
```

**Logic**:
- Chunk size < 128KB → recommend container mode
- No forced behavior, user decides
- Warning shown once per execution

---

## Backward Compatibility

### Multi-Format Support

The system maintains **full backward compatibility** through intelligent format detection:

```go
func GetChonkNode(key []byte, db *badger.DB) ([]byte, error) {
    metadata, err := GetNode(key, db)
    if err != nil {
        return nil, err
    }

    // Parse metadata: detect format by delimiter
    parts := strings.Split(string(metadata), "|")

    if len(parts) == 3 {
        // NEW FORMAT: Container mode
        containerPath := parts[0]
        offset, _ := strconv.ParseInt(parts[1], 10, 64)
        size, _ := strconv.ParseInt(parts[2], 10, 64)
        return ReadChunkFromContainer(containerPath, offset, size, key)
    } else {
        // OLD FORMAT: Direct file path (backward compatibility)
        return ReadChonk(metadata, key)
    }
}
```

### Migration Scenarios

#### Scenario 1: Read Old Database (Original → Container Mode)
```
User Action: Enable --container flag on existing database
Database: Contains chunks stored in original format
Result:
  ✓ Old chunks read successfully (direct file paths)
  ✓ New chunks written to containers
  ✓ Mixed mode operation supported
```

#### Scenario 2: Read New Database (Container → Original Mode)
```
User Action: Disable --container flag on container-mode database
Database: Contains chunks stored in container format
Result:
  ✓ Container chunks read successfully (parse metadata)
  ✓ New chunks written as individual files
  ✓ Mixed mode operation supported
```

#### Scenario 3: Database Migration (Optional)
```
Not implemented (deferred to future version)

Potential approach:
  1. Iterate all chunk keys
  2. Read chunk data
  3. Re-write in new format
  4. Update metadata
  5. Delete old storage

Complexity: High, benefit: Low (mixed mode works)
```

### Format Conversion

**Automatic Detection Logic**:

```
Read Metadata
     │
     ▼
Contains "|"? ────YES───> Parse 3 fields ──> Container Read
     │
     NO
     │
     ▼
Direct Path ──> Original Read
```

**No Configuration Needed**:
- System detects format per-chunk
- No database-level flag required
- Transparent to user

---

## Performance Considerations

### Benchmarks (Theoretical)

#### File Creation Overhead

**Original Mode**:
```
Chunks:        1,000,000
Chunk Size:    256 KB
Files Created: 1,000,000
Syscalls:      1,000,000 × (open + write + close) = 3M syscalls

Filesystem Operations:
  - Inode allocation: 1M
  - Directory entry: 1M
  - Metadata updates: 1M+

Estimated Time (ext4, SSD): ~30-60 seconds (pure file creation)
```

**Container Mode**:
```
Chunks:        1,000,000
Chunk Size:    256 KB
Total Data:    ~250 GB unique
Containers:    250 (at 1GB each)
Files Created: 250
Syscalls:      1,000,000 × (append) + 250 × (open + close) = 1M + 500

Filesystem Operations:
  - Inode allocation: 250
  - Directory entry: 250
  - Metadata updates: 250 + 1M (offset tracking)

Estimated Time (ext4, SSD): ~1-2 seconds (file operations)
```

**Speedup**: ~30-60× reduction in filesystem overhead

#### Compression Performance

**Per-Chunk Compression (Original)**:
```
Operation:      Compress each 256KB chunk
Compression:    zstd level 15
Per-Chunk Time: ~10-50ms
Total Chunks:   1,000,000
Total Time:     10,000-50,000 seconds (2.7-13.9 hours)

Parallelization: Limited by worker count
```

**Container Compression (Container Mode)**:
```
Operation:      Compress per 1GB container (after finalized)
Compression:    zstd level 15
Per-Container:  ~30-120 seconds
Total Containers: 250
Total Time:     7,500-30,000 seconds (2.0-8.3 hours)

Benefit: Post-processing, doesn't block writes
```

**Note**: Container compression happens asynchronously (new container while old compresses)

### Space Efficiency

#### Original Mode
```
Chunk Size:       256 KB
Compressed:       ~100 KB (varies)
Encrypted:        ~100 KB + 28 bytes
Metadata:         ~4 KB (filesystem)
Per-Chunk Total:  ~104 KB

1M Chunks: 104 GB (plus inode overhead)
```

#### Container Mode
```
Chunk Size:       256 KB
Compressed:       ~100 KB (within container)
Encrypted:        ~100 KB + 28 bytes
Container:        ~1 GB (packed)
Container Comp:   ~400-600 MB (zstd level 15)
Metadata:         ~4 KB × 250 containers

1M Chunks:
  - Uncompressed containers: 100 GB
  - Compressed containers: 40-60 GB
  - Metadata: 50 bytes per chunk (in DB)
```

**Space Savings**: ~40-50% from container-level compression

### Memory Usage

#### Original Mode
```
Per Worker:
  - Chunk buffer: 256 KB
  - Compression buffer: ~256 KB
  - Encryption buffer: ~256 KB

Total (8 workers): ~6 MB active

Database: Managed by BadgerDB (configurable)
```

#### Container Mode
```
Per Worker:
  - Chunk buffer: 256 KB
  - Compression buffer: ~256 KB
  - Encryption buffer: ~256 KB

ContainerManager:
  - File handle: ~8 KB
  - Write buffer: ~256 KB (OS-managed)

Compression (peak):
  - Container in memory: 1 GB (during compression)
  - Compressed output: ~400-600 MB

Total (8 workers + compression): ~1.6 GB peak
```

**Trade-off**: Higher peak memory for better compression and I/O

### I/O Patterns

#### Original Mode
```
Pattern:   Random writes (new files)
Seeks:     None (sequential within file)
Fsync:     Per file (if enabled)
Fragmentation: High (1M files)

Disk Queue Depth: High variability
Cache Efficiency: Poor (many small files)
```

#### Container Mode
```
Pattern:   Sequential appends
Seeks:     None (append-only)
Fsync:     Per container (250 total)
Fragmentation: Low (250 files)

Disk Queue Depth: Consistent
Cache Efficiency: Excellent (few large files)
```

### Database Performance

#### Key-Value Size

**Original Mode**:
```
Key:   ~70 bytes ("C|||:" + base64(hash))
Value: ~50 bytes (file path)
Total: 120 bytes per chunk

1M Chunks: 120 MB metadata
```

**Container Mode**:
```
Key:   ~70 bytes ("C|||:" + base64(hash))
Value: ~100 bytes (path + "|" + offset + "|" + size)
Total: 170 bytes per chunk

1M Chunks: 170 MB metadata
```

**Increase**: 50 bytes per chunk (acceptable trade-off)

---

## Trade-offs

### Advantages of Container Mode

1. **Filesystem Efficiency**
   - ✅ Reduces file count by ~4000:1 ratio
   - ✅ Fewer inodes consumed
   - ✅ Faster directory listings
   - ✅ Easier backup/replication

2. **I/O Performance**
   - ✅ Sequential writes (SSD/HDD friendly)
   - ✅ Reduced syscall overhead
   - ✅ Better OS caching behavior
   - ✅ Lower context switching

3. **Compression Efficiency**
   - ✅ Better compression ratios (1GB blocks)
   - ✅ Reduced storage footprint (40-50% savings)
   - ✅ Lower network bandwidth for replication

4. **Small Chunk Optimization**
   - ✅ Ideal for chunks < 128KB
   - ✅ Mitigates file-per-chunk overhead

### Disadvantages of Container Mode

1. **Memory Usage**
   - ❌ Peak memory: ~1.6GB during compression
   - ❌ Container decompression: 1GB per read
   - ⚠️  Not ideal for memory-constrained systems

2. **Read Performance**
   - ❌ Must decompress entire container for single chunk
   - ❌ No random access to compressed containers
   - ⚠️  Future optimization: container caching

3. **Complexity**
   - ❌ More complex code paths
   - ❌ Additional failure modes (partial compression)
   - ❌ Debugging is harder

4. **Write Latency**
   - ❌ Mutex serializes writes (brief)
   - ⚠️  Negligible vs. I/O time in practice

5. **Container Corruption Risk**
   - ❌ Single container failure affects ~4000 chunks
   - ⚠️  Mitigated by: zstd checksums, atomic writes

### When to Use Container Mode

#### Recommended
- ✅ Chunk size < 128KB
- ✅ Large datasets (> 100GB)
- ✅ Filesystem with file count limits
- ✅ Network storage (NAS/SAN)
- ✅ Backup/archival scenarios

#### Not Recommended
- ❌ Memory < 4GB
- ❌ Frequent random chunk access
- ❌ Real-time low-latency requirements
- ❌ Very small datasets (< 1GB)

### Comparison Matrix

| Aspect                | Original Mode | Container Mode |
|-----------------------|---------------|----------------|
| File Count            | Very High     | Very Low       |
| Filesystem Overhead   | High          | Low            |
| Write Performance     | Good          | Excellent      |
| Read Performance      | Excellent     | Good           |
| Memory Usage          | Low           | High           |
| Storage Efficiency    | Good          | Excellent      |
| Compression Ratio     | Good          | Excellent      |
| Random Access         | Fast          | Slower         |
| Corruption Blast Rad. | 1 chunk       | ~4000 chunks   |
| Complexity            | Low           | Medium         |
| Backup Speed          | Slow          | Fast           |
| Restoration           | Granular      | Bulk           |

---

## Future Enhancements

### Potential Optimizations

1. **Container Caching**
   ```go
   type ContainerCache struct {
       cache *lru.Cache  // LRU cache of decompressed containers
       maxSize int64     // Max memory for cache (e.g., 4GB)
   }
   ```
   - Keep recently accessed containers in memory
   - Avoid repeated decompression
   - Configurable cache size

2. **Partial Container Reads**
   ```go
   // Instead of: decompress entire 1GB
   // Use: zstd seeking (if supported by lib)
   ```
   - Zstd supports frame-based seeking
   - Requires index at compression time
   - Trade-off: larger files, faster reads

3. **Background Compression**
   ```go
   type CompressionQueue struct {
       queue chan string  // Paths to compress
       workers int        // Background workers
   }
   ```
   - Don't block on compression
   - Compress in background goroutine
   - Monitor with status channel

4. **Container Rebalancing**
   ```
   Scenario: Deduplication reduces effective container fill
   Solution: Periodic compaction of sparse containers
   Benefit: Maintain optimal container density
   ```

5. **Multi-Level Containers**
   ```
   Concept: Container hierarchy
     - L1: 1GB containers (current)
     - L2: 100GB super-containers
     - L3: Archive (multi-TB)
   ```

### Monitoring & Observability

1. **Metrics to Track**
   ```go
   type ContainerMetrics struct {
       ActiveContainers     int
       TotalChunksWritten   int64
       AvgCompressionRatio  float64
       AvgWriteLatency      time.Duration
       CompressionQueueSize int
   }
   ```

2. **Health Checks**
   - Container integrity verification
   - Compression success rate
   - Write throughput monitoring
   - Memory pressure detection

3. **Debug Logging**
   ```
   [CONTAINER] Created new container: index=42, path=/data/BLOBS/Ax7.blob
   [CONTAINER] Wrote chunk: offset=12345678, size=200KB, container=42
   [CONTAINER] Compressing container 41: original=1.0GB, ratio=0.58
   [CONTAINER] Compression complete: container 41, time=45s, size=580MB
   ```

---

## Appendix

### Code References

| Component          | File Path                       | Key Functions                          |
|--------------------|---------------------------------|----------------------------------------|
| Container Manager  | `lib/fio/container.go`          | `NewContainerManager`, `WriteChunkToContainer`, `compressContainer` |
| Database Layer     | `lib/dbio/dbio.go`              | `SetBatchChonkNode`, `GetChonkNode`    |
| Storage Flow       | `lib/store/store.go`            | `storeEvidenceData`, `processChonk`    |
| CLI Integration    | `main.go`, `cli/common.go`      | Flag parsing, recommendations          |
| Constants          | `lib/cnst/const.go`             | `CONTAINERMODE`, `MaxContainerSize`    |
| Thread Structures  | `lib/structs/thread.go`         | `ThreadIO`                             |

### Testing Scenarios

1. **Basic Functionality**
   - Store file with container mode
   - Restore file and verify integrity
   - Compare checksums

2. **Concurrency**
   - Store multiple files simultaneously
   - Verify no data races (run with `-race`)
   - Check container integrity

3. **Container Transitions**
   - Store enough data to trigger multiple containers
   - Verify compression of finalized containers
   - Check metadata correctness

4. **Backward Compatibility**
   - Store with original mode
   - Enable container mode
   - Restore old chunks
   - Store new chunks
   - Verify mixed operation

5. **Error Handling**
   - Simulate disk full during write
   - Corrupt container file
   - Verify graceful degradation

6. **Performance**
   - Benchmark original vs container mode
   - Measure file creation time
   - Measure compression time
   - Measure read latency

### Glossary

- **Chunk**: Fixed-size block of data (default 256KB) used for deduplication
- **Container**: Large file (max 1GB) containing multiple packed chunks
- **Deduplication**: Process of storing identical chunks only once
- **Mutex**: Mutual exclusion lock ensuring thread-safe access
- **Thread IO**: Structure passed to worker goroutines for concurrent processing
- **BadgerDB**: Embedded key-value database used for metadata storage
- **Zstd**: Zstandard compression algorithm (by Facebook)
- **AES-256-GCM**: Advanced Encryption Standard with Galois/Counter Mode
- **Memory-mapped file**: File accessed directly through memory addresses (zero-copy)

---

**Document Version**: 1.0
**Last Updated**: February 15, 2026
**Author**: Indicer Development Team
**Status**: Production Ready
