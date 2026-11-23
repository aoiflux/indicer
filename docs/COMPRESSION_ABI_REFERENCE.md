# qwikpak API and ABI Reference

This is the integration reference for qwikpak, the DUES compression product,
for frontends that call the native library (Flutter and other FFI hosts) and
for scripts that call the `compression` command. It covers:

- the native functions the library exports and who owns which memory;
- the JSON request and response envelopes, error codes and limits;
- every operation, with its parameters and result;
- the `.duesarc` format and split archives;
- the `compression` command and its exit codes.

## How a call works

1. The host calls `DuesDispatchJSON` with a JSON request envelope.
2. The dispatcher sends the request to the product it names (`qwikpak`).
3. The operation runs and the dispatcher returns a JSON response envelope.
4. The host frees the response with `DuesFreeString`.

Before its first request, a host may call `DuesConfigure` once to choose
where the library logs and keeps temporary files.

`compression_product` is accepted as a legacy name for `qwikpak`.

## Native ABI (FFI)

Source: `cmd/ffi/main.go`.

### Exported functions

```c
char* DuesConfigure(char* configJSON);
char* DuesDispatchJSON(char* requestJSON);
char* DuesShutdown(long long timeout_ms);
void  DuesFreeString(char* ptr);
```

### DuesConfigure

The library reads no environment variables; this call is its configuration.

- **Input:** a NUL-terminated UTF-8 JSON object. NULL is read as `{}`.

  ```json
  { "log_dir": "C:/Users/me/AppData/Local/MyApp/logs", "temp_dir": "D:/scratch" }
  ```

  - `log_dir`: where the library logs, as the command-line tools do, in
    rotating NDJSON files. Without it, see [Logging and output](#logging-and-output).
  - `temp_dir`: where archive work directories and decoded containers go. It
    must exist. Without it, `/tmp` on Unix and `AppData\Local\Temp` under the
    user's profile on Windows.
  - Both are optional and must be absolute. Any other key is refused.
- **When:** once, before any other call. Without it, the first
  `DuesDispatchJSON` or `DuesShutdown` sets up the defaults. Moving the log or
  the temporary directory under running tasks is not safe, so a later call is
  refused and changes nothing: `invalid_request` with `details.reason`
  `already_configured` (it was called before) or `requests_started` (a request
  already ran with the defaults).
- **Output:** a response envelope. On success its result gives the
  directories in effect; `log_dir` is empty when the library uses its default
  log, which is created at its first entry.

  ```json
  { "version": "v1", "status": "ok", "result": { "log_dir": "C:/Users/me/AppData/Local/MyApp/logs", "temp_dir": "D:/scratch" } }
  ```

- **Failures:** a bad document is an `invalid_request` whose message starts
  with `configuration:`. The call never returns NULL and never lets a panic
  reach the host.
- The library logs the configuration in effect, and where each value came
  from, in a `settings` record.

### DuesDispatchJSON

- **Input:** a NUL-terminated UTF-8 JSON request envelope. NULL is read as `{}`
  (which is refused as a request with no version).
- **Size limit:** at most 64 MiB. The library reads at most one byte past the
  limit, so a larger or unterminated buffer is refused with `limit_exceeded`
  without being copied.
- **Output:** a UTF-8 JSON response envelope. It is never NULL and never empty.
- **Failures:** the call never lets a panic reach the host. A failure inside
  the library is an `internal_error` response; when it was a panic, its
  details carry a `panic_id` that names it in the library's log.
- **Threads:** the function may be called from several threads at once.

### DuesShutdown

Call this before unloading the library or exiting the host.

- It cancels every async task.
- It refuses every later request: they return `canceled` with reason
  `shutdown`.
- It waits up to `timeout_ms` milliseconds for the tasks to end. With 0 or
  less it does not wait.
- It returns a response envelope whose result counts the tasks it cancelled
  and those still running when it returned. Free it with `DuesFreeString`.

```json
{ "version": "v1", "status": "ok", "result": { "canceled_tasks": 2, "running_tasks": 0 } }
```

A task still running after the timeout keeps its files until it notices the
cancellation. Do not unload the library while `running_tasks` is above zero.

### DuesFreeString

- Call it once for every string `DuesConfigure`, `DuesDispatchJSON` or
  `DuesShutdown` returned.
- Never free those strings with any other allocator.
- Passing NULL does nothing.

### Memory ownership

- The request buffer stays the caller's.
- The response buffer becomes the caller's, released with `DuesFreeString`.

### Process memory

Operations on different archives run at once. One that creates an archive
holds one compression state per CPU, about 50 MB each at the default (best)
level. Creations at the same level share those states, and the last to end
releases them. Apart from that, the library keeps a few MB per CPU of decoder and
table-encoder state for as long as it is loaded.

### Logging and output

The library never writes to the host's stdout or stderr.

- **`DuesConfigure` gave a `log_dir`:** the library logs there, as the
  command-line tools do, in rotating NDJSON files.
- **Otherwise:** it logs only warnings and errors (recovered panics among
  them) to the user cache directory (`dues/logs`). The file is created only
  when there is something to write.

The library does not install a crash handler: the host owns the process.

## Building

The build scripts are `build-compression.ps1` and `build-compression.sh`. They
write to `dist/compression`.

### Command-line tool

```powershell
./build-compression.ps1 -Mode cli -Targets windows/amd64
./build-compression.ps1 -Mode cli -Targets linux/amd64
./build-compression.ps1 -Mode cli -Targets darwin/arm64
```

Output: `dist/compression/compression-<os>-<arch>[.exe]`.

### Native library

`-Mode ffi` builds the library from `cmd/ffi`, with its C header.

By default (`-FfiBuildMode auto`):

- Windows: `c-shared` gives a `.dll`.
- Linux and macOS: `c-archive` gives a `.a`.

```powershell
./build-compression.ps1 -Mode ffi -Targets windows/amd64 -FfiBuildMode c-shared
./build-compression.ps1 -Mode ffi -Targets linux/amd64 -FfiBuildMode c-archive
./build-compression.ps1 -Mode ffi -Targets linux/amd64 -FfiBuildMode c-shared
```

Output: `dist/compression/dues_engine-<os>-<arch>.<dll|so|dylib|a>`.

For Flutter on Windows, use `c-shared`: the GNU `.a` that `c-archive` produces
on Windows does not link with MSVC toolchains.

## JSON envelopes

Source: `internal/core/jsonbridge/jsonbridge.go`.

### Request

```json
{
  "version": "v1",
  "product": "qwikpak",
  "operation": "create_archive",
  "params": { },
  "timeout_ms": 600000
}
```

- `version`, `product` and `operation` are required; `version` must be `v1`.
- `params` defaults to `{}`.
- `timeout_ms` is optional.
  - When it is positive, it bounds the operation; for an async request, it
    bounds the task the request starts.
  - When it runs out, the operation stops and fails with `canceled`, reason
    `timeout`.
  - A negative value is `invalid_request`.

### Success

```json
{ "version": "v1", "status": "ok", "result": { }, "duration_ms": 12 }
```

`duration_ms` is how long the operation ran, in milliseconds, on success and
on error. A request refused before it ran (a bad envelope, an unknown product)
has none. For an async request it is the time to start the task.

### Error

```json
{
  "version": "v1",
  "status": "error",
  "error": { "code": "invalid_archive", "message": "…", "details": { "key": "value" } }
}
```

## Error codes

| Code | Meaning | What a frontend does |
|---|---|---|
| `invalid_request` | The request is wrong: a missing or bad parameter, an input that does not exist, an unsupported format, a missing (`details.reason: password_required`) or wrong (`wrong_password`) password. From `DuesConfigure`: a bad document, or a call that came too late (`already_configured`, `requests_started`). | Show the message; fix the request. |
| `unknown_product`, `unknown_operation` | The envelope names something the library does not have. | Programming error. |
| `invalid_archive` | The archive cannot be extracted here as it stands. Its data is damaged (a checksum, a split part, a stored chunk), its manifest is invalid, or entries have names that are unsafe or invalid on this system, or that collide. `details.count` and `details.entry_1` … `entry_20` name the entries. | Show the entries; the archive must be repaired or extracted elsewhere. |
| `conflict` | With `on_conflict: error`, entries would replace existing files; or an entry is a file where a directory exists (or the reverse). The entries are named as for `invalid_archive`. Nothing was written. | Ask the user, then retry with `overwrite` or `skip`, or another output. |
| `limit_exceeded` | A limit would be passed. `details.limit` names it: `max_total_bytes`, `max_entry_bytes`, `max_entries`, `max_ratio`, `max_path_bytes`, `free_space`, or `max_output_bytes`. For a request over 64 MiB it gives `limit_bytes`; for too many async tasks, `limit`. | Show it; let the user raise the limit if they trust the archive. |
| `canceled` | The operation was cancelled. `details.reason` is `canceled` (`cancel_task`), `timeout` (`timeout_ms`) or `shutdown` (`DuesShutdown`). | Treat as terminal. |
| `internal_error` | A failure inside the library. `details.panic_id`, when present, names a recovered panic in the log. | Report it with the panic ID. |

## Limits

Extraction and file decompression write at most what their limits allow. The
limits are checked on the bytes actually written, not only on the sizes an
archive declares. Every limit can be raised per request with a `limits`
object:

```json
"limits": {
  "max_total_bytes": 1099511627776,
  "max_entry_bytes": 1099511627776,
  "max_entries": 1048576,
  "max_ratio": 10000,
  "max_path_bytes": 4096
}
```

| Limit | Default | Bounds |
|---|---|---|
| `max_total_bytes` | 1 TiB | all bytes written |
| `max_entry_bytes` | `max_total_bytes` | one entry |
| `max_entries` | 1,048,576 | entries in the archive |
| `max_ratio` | 10,000 | output bytes per archive byte, once 1 GiB has been written |
| `max_path_bytes` | 4096 | one entry's path |

Some checks need no `limits` field:

- **Free space.** Before writing, the declared output is checked against the
  free space of the output's volume (`free_space`).
- **Deduplication bombs.** In a `.duesarc`, every path counts, so content
  stored once but listed under many paths cannot pass the limits.
- **`decompress_bytes`.** Its output is bounded by `max_output_bytes`: 64 MiB by
  default, at most 256 MiB.

## Async tasks

`create_archive`, `extract_archive`, `list_archive`, `verify_archive`,
`compress_file` and `decompress_file` accept `"async": true`.

- **Start:** the request returns at once with `{ "task_id": "…", "status": "queued" }`.
- **Follow:** poll `get_progress` until `status` is `completed`, `failed` or
  `canceled`.
- **Lifetime:** a task does not end with the request that started it. It keeps
  the request's `timeout_ms`.
- **Concurrency:** at most 4 tasks run or wait at once; a fifth is
  `limit_exceeded`. Operations that read or write a `.duesarc` database take
  turns inside the library.
- **Retention:** finished tasks are kept for an hour, at most 256 of them.
- **Panics:** a panic inside a task fails that task with an `internal_error`
  and a `panic_id`. The host is not affected.

## Operations

### capabilities

Params: `{}`.

```json
{ "product": "qwikpak", "operations": ["…"], "levels": ["fast", "default", "best"],
  "formats": ["duesarc", "zip", "tar", "tar.gz", "tar.xz"] }
```

### create_archive

Archives a file, a folder, or files given one by one.

```json
{
  "input_path": "C:/evidence/case",
  "input_paths": ["C:/evidence/a.e01", "C:/evidence/notes/b.txt"],
  "format": "duesarc",
  "output_path": "C:/evidence/case.duesarc",
  "chunk_size_kb": 256,
  "password": "…",
  "split_size_mb": 0,
  "work_dir": "",
  "keep_work_dir": false,
  "async": false
}
```

**Inputs**

- Give `input_path` (a file or a folder), or `input_paths` (files only).
- A folder is walked without following links. Its regular files and its
  directories, empty ones included, are archived.
- Symbolic links, devices and other special files are not archived. They are
  counted in `skipped_inputs` and named in `skipped`.
- Files from `input_paths` keep their paths relative to the deepest folder
  they share; files on different volumes go under `input_NNN/`.

**Format and output**

- `format` defaults to the extension of `output_path`, else `duesarc`. An
  output path with an unknown extension is refused.
- Without `output_path`, the archive is written beside the input, named
  after it (`bundle` for `input_paths`).
- An existing archive of the same name is replaced atomically. Until the new
  archive is complete, the old one stays as it was.
- A failed write never leaves a partial archive: a write error, including one
  while closing the archive, fails the request.

**Options**

- `chunk_size_kb` (`.duesarc` only): a multiple of 4 from 4 to 65536 (256 by
  default).
- `password` (`.duesarc` only): encrypts the archive's database (Argon2id key
  derivation). Without one, the archive is compressed but not encrypted.
- `work_dir` (`.duesarc` only): an empty directory to build the archive's
  database in. A directory that already holds an archive's database or
  manifest is refused. Without it, a temporary directory is used and removed
  afterwards, unless `keep_work_dir` is set.
- `split_size_mb`: write the archive as parts (see "Split archives").

**Result**

```json
{ "input_path": "C:/evidence/case", "input_paths": ["C:/evidence/case"], "format": "duesarc",
  "output_path": "C:/evidence/case.duesarc", "output_files": ["C:/evidence/case.duesarc"],
  "input_is_dir": true, "input_bytes": 123456, "archive_bytes": 45678,
  "chunk_size_kb": 256, "split_size_mb": 0, "work_dir": "",
  "archived_files": 10, "archived_dirs": 3, "skipped_inputs": 0, "skipped": [],
  "ratio": 2.703, "saved_percent": 63.0 }
```

`ratio` is `input_bytes / archive_bytes` (3 decimals) and `saved_percent` is
`1 - archive_bytes / input_bytes` in percent (1 decimal, negative when the
archive is the larger). Both are absent unless both sizes are positive.

### extract_archive

Extracts a `.duesarc`, zip, tar, tar.gz or tar.xz archive into a folder.

```json
{
  "input_path": "C:/evidence/case.duesarc",
  "output_path": "C:/restored/case",
  "format": "",
  "password": "…",
  "on_conflict": "overwrite",
  "limits": { },
  "async": false
}
```

**Input and format**

- `input_path` may be an archive, a split archive's `.001` part or its
  `.split.json` sidecar.
- Without `format`, the format is taken from the extension, else from the
  file's first bytes. A zip holding a valid `.duesarc` manifest is read as
  `.duesarc`.
- Without `output_path`, the folder is the archive's name without its
  extension.

**Names**

Entry names are checked before anything is written. These are refused
everywhere:

- absolute paths, and `..`;
- a backslash or a NUL byte;
- two entries with the same name (compared case-insensitively on Windows and
  macOS);
- a file whose name is another entry's directory.

On Windows these are refused too:

- `:` (an alternate data stream);
- a trailing dot or space;
- `<>"|?*` and control characters;
- device names (`CON`, `NUL`, `COM1`, …) with or without an extension.

Every refused entry is reported (`invalid_archive`), and nothing is renamed.
A leading `./` is ignored.

**Entries that are not extracted**

Symbolic links, hard links, devices and pipes are not created. They are
counted in `skipped_entries` and named in `skipped`.

**Atomicity**

- Entries are written into a hidden staging directory inside the output
  (`.qwikpak-stage-…`), then moved into place.
- If anything fails before the move, the output is left exactly as it was.
- If a move fails, the moves already made are undone and replaced files come
  back.
- A folder the extraction created is removed again if it is left empty.

**`on_conflict`**

- `overwrite` (default): an existing file is replaced. The replacement is
  complete on disk before the old file goes.
- `skip`: an existing file is kept and counted in `kept_existing`.
- `error`: every target is checked as its entry is reached, and again before
  anything moves. If any exists, the request fails with `conflict`, nothing is
  written, and no later entry's content is extracted or restored.

**Checks**

- Limits apply (see "Limits"), and free space is checked first.
- A split set's parts are checked against their sidecar first.
- A `.duesarc` file is restored with every chunk checked against its hash and
  the whole file against its recorded hash.
- Damage is `invalid_archive`.

**Result**

```json
{ "input_path": "…", "output_path": "…", "extracted_files": 10, "extracted_dirs": 3,
  "replaced_files": 0, "kept_existing": 0, "skipped_entries": 0, "skipped": [] }
```

### list_archive

Lists an archive's entries without extracting it.

```json
{ "input_path": "C:/evidence/case.duesarc", "format": "", "password": "…", "async": false }
```

For a `.duesarc`, it also lists the archived files (`logical_files`) from the
manifest and the database. An encrypted archive needs its password for that.

```json
{ "input_path": "…", "entry_count": 42, "entries": [
    { "name": "db/kvdb/000001.vlog", "compressed_bytes": 123, "original_bytes": 456, "is_dir": false } ],
  "has_manifest": true, "manifest_version": "v2", "logical_file_count": 1,
  "logical_files": [
    { "id": "base64 content ID", "hash": "base64 hash", "name": "file.txt", "path": "dir/file.txt",
      "size": 1234, "total_size": 1234, "compressed_size": 640,
      "completed": true, "failed": false, "status": "completed" } ] }
```

Notes:

- `compressed_size` is the bytes that file's content takes in the archive.
  Files with the same content share it.
- For zip and tar archives, `has_manifest` is false and `logical_files` is
  empty.

### verify_archive

Reads an archive back and checks it.

```json
{ "input_path": "C:/evidence/case.duesarc", "format": "", "password": "…", "async": false }
```

What it checks:

- **Split parts:** every part against its sidecar's SHA-256.
- **Entries:** every entry read to its end, so every checksum the format
  carries is checked.
- **`.duesarc` content:** the manifest is validated, and every stored chunk
  of every file the archive restores is read back and checked against its
  hash. This needs the password of an encrypted archive.
- **Consistency:** every path in the manifest must have its content in the
  database, at its listed size.

Any failure is `invalid_archive`, naming the affected paths.

```json
{ "input_path": "…", "format": "duesarc", "has_split_input": false, "split_verified": false,
  "input_bytes": 1048576, "valid": true, "entry_count": 42, "file_count": 37, "dir_count": 5,
  "manifest_found": true, "manifest_valid": true, "manifest_version": "v2",
  "manifest_pipeline": "store", "manifest_db_path": "db",
  "verified_bytes": 1032192, "verified_evidence": 30 }
```

Field notes:

- `file_count` counts the archived files (for a `.duesarc`) or the file
  entries (zip, tar).
- `verified_evidence` counts the distinct contents checked.
- `split_verified` is true when a sidecar described the parts.

### get_progress

```json
{ "task_id": "cp-…" }
```

```json
{ "task_id": "cp-…", "operation": "create_archive", "status": "running", "running": true,
  "percent": 42, "stage": "packaging", "message": "packaging archive",
  "error": "", "error_code": "", "error_details": { }, "result": { },
  "started_at_utc": "…", "updated_at_utc": "…", "completed_at_utc": "" }
```

- `status` is one of `queued`, `running`, `completed`, `failed` or `canceled`.
- `running` stays true until the task's work has returned. A cancelled task
  can still be finishing, and holds its slot until then.
- For a failed or cancelled task, `error_code` and `error_details` follow the
  error codes above.
- A completed task carries its operation's result in `result`.

### cancel_task

```json
{ "task_id": "cp-…" }
```

```json
{ "task_id": "cp-…", "status": "canceled", "message": "" }
```

- A task that already finished returns its status, with the message `task is
  not cancelable`.
- Cancellation is cooperative: the task stops at its next check, removes its
  temporary files, and leaves outputs as they were.

### compress_bytes

```json
{ "data_base64": "…", "level": "fast | default | best" }
```

Result: `{ "original_bytes": 100, "compressed_bytes": 45, "data_base64": "…" }`.

The data is compressed as one zstd frame.

### decompress_bytes

```json
{ "data_base64": "…", "max_output_bytes": 67108864 }
```

Result: `{ "compressed_bytes": 45, "original_bytes": 100, "data_base64": "…" }`.

- Data that is not zstd is `invalid_request`.
- Output over `max_output_bytes` is `limit_exceeded`. The limit is 64 MiB by
  default and at most 256 MiB.

### compress_file

Archives one file as a `.duesarc`.

```json
{ "input_path": "C:/evidence/report.pdf", "output_path": "", "password": "…", "async": false }
```

- Without `output_path`, the archive is `<input>.duesarc`, beside the file.
- `level` is accepted for compatibility and not used.

```json
{ "input_path": "…", "output_path": "C:/evidence/report.pdf.duesarc",
  "original_bytes": 123, "compressed_bytes": 45, "format": "duesarc",
  "ratio": 2.733, "saved_percent": 63.4 }
```

`ratio` and `saved_percent` compare `original_bytes` with `compressed_bytes`,
as in `create_archive`.

### decompress_file

Undoes `compress_file`.

```json
{ "input_path": "C:/evidence/report.pdf.duesarc", "output_path": "", "password": "…",
  "limits": { }, "async": false }
```

The format is found by the file's first bytes. Two are read:

- a `.duesarc` archive holding exactly one file (anything more is
  `invalid_request`: use `extract_archive`);
- a zstd stream (`.dues.zst`, `.zst`).

Output:

- Without `output_path`, the output is the input without `.duesarc`,
  `.dues.zst` or `.zst`.
- The file is written atomically, replacing an existing one.

```json
{ "input_path": "…", "output_path": "C:/evidence/report.pdf",
  "original_bytes": 123, "compressed_bytes": 45, "format": "duesarc",
  "ratio": 2.733, "saved_percent": 63.4 }
```

## The .duesarc format

A `.duesarc` is a zip holding:

- `manifest.json`, first;
- `db/`, a DUES database of the archived files' deduplicated, compressed (and,
  with a password, encrypted) chunks.

Files are stored as their own bytes. A disk image is archived as the file it
is, not decoded. Files with the same content are stored once.

Manifest version 2, which this version writes:

```json
{
  "version": "v2",
  "created_at_utc": "2026-10-19T10:00:00Z",
  "pipeline": "store",
  "db_path": "db",
  "chunk_size_kb": 256,
  "input_is_dir": true,
  "input_bytes": 123456,
  "entries": [
    { "path": "sub", "type": "dir", "size": 0, "mtime": "2026-10-01T08:00:00Z" },
    { "path": "sub/a.txt", "type": "file", "evidence": "base64 content ID", "size": 12, "mtime": "…" }
  ]
}
```

What version 2 records:

- **Every path.** Every archived path is listed with the content that holds
  it. Duplicate files, empty files and empty directories all come back, in
  their tree, with their modification times.
- **No host paths.** The manifest records no absolute path of the machine
  that made the archive.
- **Nothing in the clear for an encrypted archive.** `manifest.json` can be
  read without the password, so in an archive made with a password it holds
  `"encrypted": true` and no `entries`, `input_bytes` or `input_is_dir`. The
  full manifest is stored inside the encrypted database, and extraction,
  listing and verification follow that copy, whatever `manifest.json` says.
  Listing such an archive's files therefore needs its password.

Version 1 archives (from earlier versions) still extract:

- **Placement.** Each file is placed by the source path it was stored from,
  relative to the input the manifest names; for the oldest archives, by its
  name.
- **Name clashes.** Two files that would share a path get distinct names
  (`name~<content ID>.ext`) instead of overwriting each other.

## Database packages

`dues package export` writes a `.duesarc` of another kind: a copy of a whole
DUES data root, for moving or keeping a database. Its manifest is version 3,
kind `database`. `extract_archive`, `list_archive` and `verify_archive` refuse
it with `invalid_archive`, naming `dues package inspect`, `verify` and
`import`, which read it. Earlier versions of this library refuse it as an
unsupported manifest version.

The zip holds:

- `db/`, the data root's directories and files as they are on disk, sorted by
  path, stored without compression (they are compressed or encrypted
  already), each dated at the package's creation;
- `manifest.json`, last: its sizes and hashes are computed as the files are
  copied, so the data is read once. Readers find it through the zip's central
  directory.

```json
{
  "version": "v3",
  "kind": "database",
  "pipeline": "export",
  "created_at_utc": "2026-11-03T12:00:00Z",
  "db_path": "db",
  "created_by": { "product": "dues", "version": "0.4.0", "commit": "3484a4d" },
  "database": {
    "format": "dues-db", "format_version": 1,
    "components": { "graph_schema": 3 },
    "layout": { "chunk_kib": 256, "chunk_hash": "blake3", "storage": "file",
                "blob_encoding": "v2", "value_encoding": "zstd", "badger_compression": "zstd" },
    "encryption": "none", "encrypted": false
  },
  "stats": { "evidence": 200, "logical_bytes": 209715200 },
  "dirs": [ "blob", "kvdb" ],
  "files": [ { "path": "dues-manifest.json", "size": 412, "sha256": "…" } ],
  "content_sha256": "…",
  "host": { "os": "windows", "arch": "amd64", "hostname": "lab-3" },
  "signature": { "scheme": "ed25519", "actor": "database",
                 "actor_id": "8f56dd6450c89522", "key_id": "1c0b7e2a9d4f3365",
                 "public_key": "…", "key_protection": "database", "signature": "…" }
}
```

- `files` is sorted by path, byte-wise, and every path is relative and plain
  (no `.`, `..`, backslash or absolute path).
- `content_sha256` is the SHA-256 of one line per file,
  `path NUL size NUL sha256 LF`, in order. It leaves out times and the host,
  so two packages of one database state share it.
- `components.graph_schema` is the schema version in the graph's marker,
  `graph/dues.schema.json`; it is left out when the package holds no graph.
  A reader refuses a version later than its own. A marker that records a
  compacted image needs `graph/graphene.csr` beside it: a package without
  it fails verify and import.
- `stats` is left out for an encrypted database. `plaintext_components`
  (`fts`, `graph`) names what an encrypted database's package carries
  unencrypted. `host` is written only with `--host-info`.
- `db/dues.keys/` holds the keyring (`keyring.json`, `actors.json`) and
  never a private key (`*.key`): a package listing one fails verify and
  import.
- `signature` is left out of an unsigned package. `public_key` and
  `signature` are base64; `actor_id` and `key_id` are 16 hex digits;
  `key_protection` is `passphrase`, `database` or `none`.
- The signature is Ed25519 over a statement, not over the manifest's JSON.
  The statement is a series of fields, each a 4-byte big-endian length and
  its UTF-8 bytes, in this order:
  1. `dues.package.v1`, then `signature.scheme`;
  2. `version`, `kind`, `pipeline`, `created_at_utc`, `db_path`,
     `content_sha256`;
  3. `dirs`: its count in decimal, then each entry;
  4. `stats` as `<evidence> <logical_bytes>` in decimal, or empty when left
     out;
  5. `database.format`, `format_version`, `components.graph_schema` (0 when
     left out), `encryption`, `encrypted` (`true` or `false`);
  6. `plaintext_components`: its count, then each entry;
  7. the `layout` fields, in the order shown above;
  8. `signature.actor`, `actor_id`, `key_id`, the public key's raw 32 bytes,
     and `key_protection`.

  Numbers are decimal text. `created_by` and `host` are not covered.
- A reader also checks that `actor_id` is the ID of `actor` and `key_id` the
  ID of `public_key`, and that the packaged keyring holds the key, not
  revoked, with the same protection. Given a trusted key, the key IDs must
  match. The key travels in the package, so only a key held elsewhere shows
  who signed it.
- Fields may be added within version 3; readers ignore the ones they do not
  know. A reader refuses a later version.

A package may be split like an archive (`--split-size-mb`), with the same
sidecar.

## Split archives

With `split_size_mb`, an archive is written as parts of that many MiB:
`<archive>.001`, `<archive>.002`, …

A sidecar, `<archive>.split.json`, describes the parts:

```json
{ "version": 1, "archive": "case.duesarc", "size": 2500000, "sha256": "…",
  "parts": [ { "name": "case.duesarc.001", "size": 1048576, "sha256": "…" } ] }
```

Writing:

- Each part is written atomically, and the sidecar last. After a crash
  part-way, the old sidecar still describes the old parts, so a reader refuses
  the mix instead of reading it.
- Parts left by an earlier, longer archive of the same name are removed. So
  is an unsplit file of that name.
- Writing an unsplit archive removes an earlier split set of that name.

Reading:

- Readers take the `.001` part, the sidecar, or the archive's name when its
  sidecar exists. They read the parts in place; nothing is assembled on disk.
- Part sizes are checked when the set is opened, and SHA-256s before
  extraction and in `verify_archive`.
- `name.NNN` counts as a part only when a sidecar names it, or when `name` has
  an archive extension. `image.001` on its own is a file.
- Split archives from earlier versions have no sidecar. They are read without
  verification (`split_verified: false`).

## The compression command

```
compression [--config FILE | --no-config] [--log-dir DIR] [--temp-dir DIR] <command> [flags]
```

The global options come before the command. `--log-dir` chooses the log
directory (default: the per-user cache directory). `--temp-dir` chooses where
work directories go; it must exist (default: `/tmp`, or `AppData\Local\Temp`
on Windows). The command reads no environment variables.

Both can be kept in a config file, `~/.config/dues/compression.json`, read
when it exists; `--config FILE` reads another, which must exist, and
`--no-config` reads none. The file is one JSON object with the keys
`log-dir` and `temp-dir`, each an absolute path, for example
`{"log-dir": "D:\\logs", "temp-dir": "D:\\scratch"}`. An option on the
command line wins over the file. A file that cannot be used stops every
command, `-h` included, with exit 2 and a message naming the file, line and
column. The rules are those of DUES's own file
([OPERATIONS.md](OPERATIONS.md#the-config-file)). The run's `settings` log
record names the file read and whether an option, the file or the default
chose each directory. The FFI library reads no config file: its host passes
the same settings to `DuesConfigure`.

| Command | Operation | Main flags |
|---|---|---|
| `create-archive` | `create_archive` | `--input`, `--input-file` (repeatable), `--output`, `--format`, `--chunk-size-kb`, `--split-size-mb`, `--work-dir`, `--keep-work-dir`, password flags |
| `extract-archive` | `extract_archive` | `--input`, `--output`, `--format`, `--on-conflict`, `--max-total-bytes`, `--max-entries`, `--max-ratio`, password flags |
| `list-archive` | `list_archive` | `--input`, `--format`, password flags |
| `verify-archive` | `verify_archive` | `--input`, `--format`, password flags |
| `compress-file` | `compress_file` | `--input`, `--output`, password flags |
| `decompress-file` | `decompress_file` | `--input`, `--output`, limit flags, password flags |
| `compress` | `compress_bytes` | `--text`, `--level` |
| `decompress` | `decompress_bytes` | `--data-base64`, `--max-output-bytes` |
| `capabilities` | `capabilities` | |
| `dispatch` | any | `--request JSON` or `--request-file PATH` (`-` for stdin) |

The command runs one request and exits, so it has no async mode. `--async`,
`get-progress` and `cancel-task` were removed; the library keeps them.

**Passwords**, in order of precedence:

1. `--password-file PATH`: the first line of the file.
2. `--password-stdin`: the first line of standard input.

`--password` was removed, because other users of the machine can see a
password on the command line; passing it is a usage error that names these
two.

**Output:** the response envelope goes to stdout; everything else goes to
stderr.

**Summary:** after `create-archive`, `extract-archive`, `compress-file` and
`decompress-file`, `--summary` (a global option, before the command) also says
on stderr what the command did. `auto`, the default, does so when stderr is a
terminal; `on` always; `off` never. `dispatch` has none, and neither does a
failure: the envelope says it.

```
archived 1,204 files and 87 folders (3 skipped) in 12.4s
  input 10.2 GiB, archive 3.10 GiB: 3.29x smaller, 69.6% saved
  written as 4 parts, described by C:/evidence/case.duesarc.split.json
```

**Exit codes:**

| Code | When |
|---|---|
| 0 | `status` is `ok` |
| 2 | `invalid_request`, `unknown_product`, `unknown_operation`, or bad flags |
| 1 | any other error |
| 70 | an internal error from a recovered panic |
| 130 / 143 | interrupted (Ctrl-C, SIGTERM); the operation stops and removes its temporary files |

**Cleanup:** before it first creates a work directory, the command removes
`duesarc-*` work directories that a killed process left under the temporary
directory. A directory is removed only when it is unlocked and older than 24
hours. The library does the same.

Examples:

```sh
compression create-archive --input ./evidence --output ./evidence.duesarc --password-file ./pw
compression create-archive --input ./evidence --format zip --split-size-mb 4000
compression --temp-dir D:/scratch extract-archive --input ./evidence.duesarc --output ./restored --on-conflict error --password-file ./pw
compression verify-archive --input ./evidence.duesarc.001 --password-stdin < ./pw
compression decompress-file --input ./report.pdf.duesarc
```

## Flutter example

```dart
import 'dart:convert';
import 'dart:ffi' as ffi;

import 'package:ffi/ffi.dart';

typedef _DispatchNative = ffi.Pointer<Utf8> Function(ffi.Pointer<Utf8>);
typedef _DispatchDart = ffi.Pointer<Utf8> Function(ffi.Pointer<Utf8>);
typedef _ShutdownNative = ffi.Pointer<Utf8> Function(ffi.LongLong);
typedef _ShutdownDart = ffi.Pointer<Utf8> Function(int);
typedef _FreeNative = ffi.Void Function(ffi.Pointer<Utf8>);
typedef _FreeDart = void Function(ffi.Pointer<Utf8>);

class QwikpakClient {
  QwikpakClient(ffi.DynamicLibrary lib)
      : _configure = lib.lookupFunction<_DispatchNative, _DispatchDart>('DuesConfigure'),
        _dispatch = lib.lookupFunction<_DispatchNative, _DispatchDart>('DuesDispatchJSON'),
        _shutdown = lib.lookupFunction<_ShutdownNative, _ShutdownDart>('DuesShutdown'),
        _free = lib.lookupFunction<_FreeNative, _FreeDart>('DuesFreeString');

  final _DispatchDart _configure;
  final _DispatchDart _dispatch;
  final _ShutdownDart _shutdown;
  final _FreeDart _free;

  Map<String, dynamic> _take(ffi.Pointer<Utf8> p) {
    try {
      return jsonDecode(p.toDartString()) as Map<String, dynamic>;
    } finally {
      _free(p);
    }
  }

  /// Call once, before any other call, to choose the directories.
  Map<String, dynamic> configure({String? logDir, String? tempDir}) {
    final doc = jsonEncode({
      if (logDir != null) 'log_dir': logDir,
      if (tempDir != null) 'temp_dir': tempDir,
    }).toNativeUtf8();
    try {
      return _take(_configure(doc));
    } finally {
      calloc.free(doc);
    }
  }

  Map<String, dynamic> call(String operation, [Map<String, dynamic>? params, int? timeoutMs]) {
    final request = jsonEncode({
      'version': 'v1',
      'product': 'qwikpak',
      'operation': operation,
      'params': params ?? <String, dynamic>{},
      if (timeoutMs != null) 'timeout_ms': timeoutMs,
    }).toNativeUtf8();
    try {
      return _take(_dispatch(request));
    } finally {
      calloc.free(request);
    }
  }

  /// Call before the app exits.
  Map<String, dynamic> shutdown({int timeoutMs = 5000}) => _take(_shutdown(timeoutMs));
}
```

```dart
final client = QwikpakClient(ffi.DynamicLibrary.open('dues_engine-windows-amd64.dll'));
client.configure(logDir: 'C:/Users/me/AppData/Local/MyApp/logs');

final start = client.call('create_archive', {
  'input_path': 'C:/evidence/folder',
  'output_path': 'C:/evidence/folder.duesarc',
  'async': true,
});
if (start['status'] != 'ok') throw Exception(start['error']['message']);
final taskId = start['result']['task_id'] as String;

while (true) {
  final p = client.call('get_progress', {'task_id': taskId})['result'] as Map<String, dynamic>;
  if (p['status'] == 'completed') break;
  if (p['status'] == 'failed' || p['status'] == 'canceled') {
    throw Exception('${p['error_code']}: ${p['error']}');
  }
  await Future.delayed(const Duration(milliseconds: 500));
}

final res = client.call('extract_archive', {
  'input_path': 'C:/evidence/folder.duesarc',
  'output_path': 'C:/restored/folder',
  'on_conflict': 'error',
});
if (res['status'] != 'ok' && res['error']['code'] == 'conflict') {
  // Ask the user, then retry with 'overwrite' or 'skip'.
}

client.shutdown();
```

## Integration checklist

- Call `DuesConfigure` first if the app keeps its logs or temporary files in
  a place of its own.
- Send the full envelope with product `qwikpak`.
- Branch on `status`, then on `error.code`. Show `error.message` and, for
  `invalid_archive` and `conflict`, the entries in `details`.
- Free every response with `DuesFreeString`.
- Offer `.duesarc` as the recommended format. Zip and tar are for exchange
  with other tools; they are not deduplicated or verified beyond their own
  checksums.
- Ask for the archive's password when an error has `details.reason`
  `password_required` or `wrong_password`.
- Use async tasks for long operations; poll `get_progress` and offer
  `cancel_task`.
- Let the user choose `on_conflict` when extracting into a folder that is not
  empty.
- Call `DuesShutdown` before the app exits.

## Versioning

- JSON API version: `v1`. The fields added in this version are additive.
- Product name: `qwikpak` (`compression_product` accepted).
- Archives: manifest `v2` written, `v1` read.
- Split sets: sidecar version 1 written; sets without a sidecar read.
