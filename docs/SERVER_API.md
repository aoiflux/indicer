# DUES server API

`dues server` serves one DUES database over Connect, gRPC and gRPC-Web, and offers HTTP endpoints for browser uploads and database exports. This page is for people who write clients for it and for people who run it.

The service is defined in [`protos/dues.proto`](../protos/dues.proto). The generated Go code is in `pb/` and `pb/pbconnect/`.

## Running it

```
dues server [--listen HOST:PORT]... [--token-file PATH] [--cors-origin ORIGIN]... [--upload-jobs N]
            [--export-dir DIR] [--export-keep-hours N] [--export-drain-timeout D] [global options]
```

- **Addresses.** By default the server listens on `127.0.0.1:50051` and `[::1]:50051` only. If the host has no IPv6 loopback, the IPv6 address is skipped and a note is printed. `--listen` replaces the defaults and may be given more than once.
- **Tokens.** A non-loopback address (`0.0.0.0`, `::`, `:50051`, a LAN address or a host name) is refused unless a bearer token is set. Tokens come from `--token-file PATH` only; [Tokens and scopes](#tokens-and-scopes) describes the file. `--token` was removed, because other users can see a command line; passing it is a usage error that names `--token-file`. The `DUES_TOKEN` environment variable is ignored.

  Once a token is set, every listener requires one, loopback included. The startup lines name each token by its label, with its scopes; the token itself is never printed or logged.
- **Encryption.** The server speaks plain HTTP. A warning is printed when it listens on a non-loopback address. To serve remote clients, put a TLS-terminating proxy or an SSH tunnel in front of it.
- **Uploads at once.** `--upload-jobs N` stores up to N uploads at once: from 0 to 64, default 1, and 0 picks a number for this machine. No more than 16 uploads may be in progress, so a higher number is cut to 16, with a note. Low-resource mode stores one at a time. See [Browser uploads](#browser-uploads) for the order they are stored in.
- **Exports.** A token with the `export` scope can have the server write a package of the database and download it; see [Exports](#exports).
  - `--export-dir DIR` is where packages go. By default it is the data root's `exports` folder. Inside the data root, only `exports`, spelled exactly so, or a folder in it may hold packages. On the command line a relative folder is taken from the working folder; in the config file it must be absolute.
  - `--export-keep-hours N` keeps a written package N hours: from 1 to 720, default 24.
  - `--export-drain-timeout D` is how long an export waits for the uploads being stored before it stops them: a duration such as `90s` or `10m`, from `1s` to `1h`, default `5m0s`. See [Pauses](#pauses).

  When some token has `export`, the startup lines say where exports go and how long they are kept.
- **Configuration errors.** A bad address, origin, token, `--upload-jobs`, `--export-dir`, `--export-keep-hours` or `--export-drain-timeout` value exits with code 2 before the database is opened. An export folder another DUES server uses, or one that fails the checks in [Exports](#exports), stops the server at start.

Examples:

```
dues server --dbpath ./caseA
dues server --listen 0.0.0.0:50051 --token-file ./token
dues server --listen 192.168.1.10:50051 --token-file ./token --cors-origin https://ui.example.org
dues server --token-file ./tokens --export-dir /srv/dues-exports --export-keep-hours 48
```

## Tokens and scopes

When tokens are set, send one on every request except health checks:

```
Authorization: Bearer <token>
```

**The token file.** `--token-file` takes one of two forms.

- **One token.** Without a header, the file's first line is the token, at least 32 visible ASCII characters, not starting with `#`. It may read and upload, and its label is `default`. Any other non-blank line is refused: a file of several tokens needs the header.
- **Several tokens.** A file whose first line is `#dues-tokens v1` holds one token per line, as `<token> <scopes> <label>`, separated by spaces:

  ```
  #dues-tokens v1
  # examiner workstation
  3f9c…(64 hex digits)…  read,upload  examiner-1
  b81e…(64 hex digits)…  all          admin
  ```

  - A token is at least 32 characters of `A-Z a-z 0-9 - . _ ~ + /`, which may end in `=` (RFC 6750). `openssl rand -hex 32` makes one. A token therefore never starts with `#`.
  - Scopes are `read`, `upload` and `export`, separated by commas. `all` is exactly these three: a scope added in a later release is not granted by `all` in a v1 file.
  - A label is 1 to 31 characters of `A-Z a-z 0-9 . _ -`. Labels must differ, without regard to case. A label is shorter than any token, so a line with its columns swapped is refused rather than a token being taken for a label.
  - Blank lines and lines starting with `#` are skipped. Putting `#` before a token's line turns that token off.
  - A file holds at most 64 tokens, and no token twice.
- **Errors.** A file that cannot be used stops the server with exit 2. The message names the line and what is wrong with it, never what the line holds.
- **The file itself.** The file is read by the config file's rules: a regular file of at most 64 KiB, and on Linux and macOS one that belongs to you or root and that neither its group nor others may write (`chmod go-w`). Others may read it, as they can a mounted container secret.
- **Revoking a token.** In a `#dues-tokens v1` file, remove or comment out its line. In a one-token file, replace the token with a new one: comments need the header, so a `#` before that token stops the server from starting. Then restart the server: the file is read only at start. A restart ends every upload in progress, of every token: their clients upload again.

**What each scope allows.**

| Scope | Routes |
| --- | --- |
| `read` | `GetEviFiles`, `GetPartiFiles`, `GetIdxFiles`, `Search` |
| `upload` | `StreamFile`, `AppendIfExists`, `/web/upload/*` |
| `export` | `/web/export/*`: `start`, `status`, `cancel` and `download` |
| none | `GET /healthz`, `GET /readyz` |

Scopes say what a token may do. They do not divide the database: every token works on the one store. A token with `upload` sees, through `AppendIfExists`, whether content whose hash it holds is stored, and the stored evidence's `file_id`, size and chunk map. Uploads of the same client path, or of the same content, by different tokens are stored one after another, and a later upload of stored content becomes an alias, whichever token stored it first.

A token with `export` takes the whole case:
- An export copies the whole database, every token's evidence included. An encrypted database is copied as its encrypted files, but its full-text index and graph are not encrypted, in the database or in the package; `without_derived` leaves them out.
- Every export pauses the database, and reads are refused while it is paused (see [Pauses](#pauses)). A token with `export` can start and cancel exports again and again, and so keep reads from being served.

Give `export` only to trusted operators who may take a copy of the case.

**Refusals.**

- A missing or wrong token gets `401 Unauthorized` with `WWW-Authenticate: Bearer realm="dues"`. Connect and gRPC clients see this as the `unauthenticated` code. Every token is compared, in constant time.
- A token without the route's scope is refused with `WWW-Authenticate: Bearer realm="dues", error="insufficient_scope", scope="<scope>"`. `/web` routes and Connect unary calls get `403 Forbidden`. gRPC, gRPC-Web and Connect streaming calls get the `permission_denied` code in their status, over HTTP 200, as those protocols carry errors. The message names the scope that is missing. The log records `server scope refused` with the token's label and the scope.

**Without tokens.** When no token is set, the server listens on loopback only, and every request may read and upload. Export is refused with `403`: its message says to start the server with a `#dues-tokens v1` file that gives a token `export` or `all`.

**In the log.** The access log, `server request`, names the token a request came with by its label, in the field `bearer`, when tokens are set.

## Browser access

- **Origins.** A page may call the server when its `Origin` is a loopback origin (`localhost`, a name under `.localhost`, or a loopback IP, any port, http or https) or exactly one of the `--cors-origin` values. Origins are compared without regard to case, and with default ports left out.
- **Refused origins.** A request that carries any other `Origin`, `null` included, gets `403` before it is served. This applies to preflight and non-preflight requests alike, so a foreign page cannot get the browser to send even a "simple" request (CSRF).
- **No Origin.** A request without an `Origin` header is not from a browser page, and is served.
- **Preflights.** Preflights are answered `204` without needing a token, and allow:
  - methods `GET, POST, OPTIONS`
  - request headers `Authorization`, `Content-Type`, the Connect and gRPC-Web headers, `Upload-Id`, `Upload-Offset`, and, for a download that resumes, `Range`, `If-Range`, `If-None-Match` and `If-Match`
- **Exposed headers.** `Grpc-Status`, `Grpc-Message`, `Grpc-Status-Details-Bin`, `Request-Id`, `Upload-Offset`, and, for downloads, `ETag`, `Repr-Digest`, `Content-Disposition`, `Content-Range` and `Accept-Ranges`.
- **Downloads.** A package download needs the `Authorization` header. A plain link, or a link with a `download` attribute, sends no token and gets `401`: a page downloads with `fetch` and saves the stream.
- **Host names.** On a loopback address the `Host` header must name a loopback host, or the request gets `421 Misdirected Request`. This stops a page whose name was rebound to 127.0.0.1 (DNS rebinding) from reaching the server.

## Limits and timeouts

| What | Limit |
| --- | --- |
| Connections | 512 at once; more wait for one to close |
| Requests in flight | 256; more get `503` with `Retry-After: 1` |
| Request headers | 64 KiB, within 10 s |
| JSON request bodies | 64 KiB |
| Unary RPC messages | 1 MiB after decompression |
| `StreamFile` messages | 16 MiB + 64 KiB each |
| Response messages | 64 MiB |
| Idle keep-alive connections | closed after 120 s; HTTP/2 connections are pinged after 30 s of silence and dropped 15 s after an unanswered ping |
| Unary RPCs | 30 s to read the request, 5 min for the response |
| `Search` | 2 min; at most 2 searches at once |
| Upload bodies (`chunk`, `StreamFile`) | must keep arriving: each read gets 1 min |
| Exports | one at a time; at most 4 written packages kept |
| Export `start` | sizes the database for up to 20 s, then answers |
| Package downloads | 4 at once; more get `429` with `Retry-After: 30`. `HEAD` is not counted. Each write to the client must go through within 1 min |

## Errors

Every response carries a `Request-Id` header. The server's log records each request under that ID, and records the detail of every failure.

- **HTTP endpoints** (health, uploads, exports) answer errors as JSON:

  ```json
  {"error": "upload session not found or expired", "request_id": "0199…"}
  ```

- **RPCs** answer with a Connect or gRPC status code. A failure the client can act on has its own code and message:
  - `invalid_argument` for a bad request
  - `not_found` for an unknown ID
  - `resource_exhausted` for a message over the limit
  - `deadline_exceeded` for a search over its budget
  - `permission_denied` for a token without the procedure's scope
  - `unavailable`, with `Retry-After: 5` and the message `the database is paused for an export; retry later`, for `GetEviFiles`, `GetPartiFiles`, `GetIdxFiles`, `Search` and `AppendIfExists` while the database is paused, and for such a call a pause cut (see [Pauses](#pauses)). A Connect unary call gets it as HTTP `503`.

  A refusal before the procedure is found (a wrong token, `401`, or a busy or stopping server, `503`) is a bare HTTP status: Connect and gRPC clients see only its code, `unauthenticated` or `unavailable`.
- **Internal failures.** Any other failure, a panic included, is `internal`. Its message names only the request ID, `internal error (request <id>)`, so no internal detail (paths, keys) reaches clients.

## Health

- `GET /healthz` answers `200 ok` while the process serves.
- `GET /readyz` answers `200 ready`. It answers `503 not ready` while the server shuts down, or once its database is closed. While the database is paused for an export it answers `503 paused`, with `Retry-After: 5`.

Neither needs a token. Any other path, `/` included, is `404` (or `405` for the wrong method), once the token is accepted.

## The service

All procedures are at `/dues.DuesService/<Method>`, by POST.

### IDs and chunk maps

Every object is described by a `BaseFile`:

- `file_id`: the base64 of the object's record key. Pass it back as `evi_file_id` or `parti_file_id`.
- `file_path`:
  - an evidence's name
  - a partition's name
  - an indexed file's path in its volume
- `file_size`: the object's size in bytes.
- `chunk_map`: maps the base64 hash of each stored chunk that holds part of the object to that chunk's stored size. A chunk partly in the object counts whole. Holes and content kept inline in a record have no chunk.
- `chunk_map_truncated`: the object has more chunks than one response carries (1M entries). `chunk_map` then holds the first of them.

### Listings: `GetEviFiles`, `GetPartiFiles`, `GetIdxFiles`

- **What they list.**
  - `GetEviFiles` lists the stored evidence, in the order it was stored. Evidence still ingesting, failed, or recorded as a duplicate is not listed.
  - `GetPartiFiles(evi_file_id)` lists an evidence's partitions, in the order they lie in it.
  - `GetIdxFiles(parti_file_id)` lists a partition's indexed files.
  - An index that `dues repair --upgrade` quarantined as wrong lists no partitions or files.
- **Paging.**
  - A response holds at most `page_size` objects: 1000 when 0, and at most 10000.
  - A response also ends early rather than carry more than 1M chunk-map entries.
  - When `next_page_token` is set, pass it as `page_token` to get the next page. Tokens are opaque.
- **Errors.**
  - `invalid_argument`: a negative `page_size`, a token this server did not give, or an ID that is not base64.
  - `not_found`: an ID that names nothing stored.

### `AppendIfExists`

`AppendIfExists` asks whether content is already stored, so a client need not upload it.

- **Request.**
  - `file_hash`: the base64 of the file's plain content hash, in the database's file hash algorithm (BLAKE3-256, or SHA3-256 for a database created with `--hash-algo sha3`). The evidence's content root is accepted too.
  - `file_path`: the path the client has the file at.
- **When stored.** The answer is `exists` and the evidence's `BaseFile`. The client's path is recorded as an alias of that evidence. `appended` is true the first time a path is recorded. The evidence itself is never renamed.
- **When not stored.** The answer is `exists: false`.

### `Search`

`Search` counts `keyword`, taken as one literal term matched without regard to case, in every stored object.

- `total_count`: the occurrences in the stored evidence, counted once however many partitions and files they also fall in.
- `keyword_count_map`: the count in each object, keyed by `file_id`. It holds at most 100,000 objects; when more matched, the highest counts are kept and `truncated` is set.
- **Keyword limits.** A keyword shorter than 2 bytes or longer than 1024 bytes is `invalid_argument`.

### `StreamFile`

`StreamFile` is an upload as a client stream.

- The first message carries `file_meta`:
  - `file_path`
  - `file_hash`, as for `AppendIfExists`
  - optionally `file_type`
  - optionally `size`: more bytes than this are refused, and fewer fail the upload. Without it, a stream is refused once it passes 256 GiB, the cap on one upload.
- Each later message carries `file` bytes, in order.
- It is stored like a browser upload (below). The reply is the stored evidence.
- A client that goes away mid-stream leaves nothing behind.
- A stream that ends while the database is paused for an export is stored once the database resumes (see [Pauses](#pauses)). Give the client a deadline longer than `--export-drain-timeout` plus the time an export takes.
- Errors:
  - `data_loss`: a hash mismatch
  - `resource_exhausted`: the server is full or busy
  - `failed_precondition`: a stream that ends short of its `size`
  - `out_of_range`: a `size` over 256 GiB, or a stream without `size` that passes it

## Browser uploads

Uploads and `StreamFile` are stored with the default store settings: the store tuning flags and the config file's `store` object apply to `dues store` and the TUI, not to the server. Uploads stored at once share the store workers between them, as the evidence of `dues store --store-jobs` do.

An upload is a session, and belongs to the token that started it: any other token is told, for its `chunk`, `finalize`, `status` and `abort`, that no such session exists (`404`). The log records `upload of another token` with that token's label. Its bytes are written in order, at offsets the client names, so an upload that is cut off can carry on where the server says it stopped. All endpoints take and give JSON, except `chunk`, which takes raw bytes.

**1. Start.**

```
POST /web/upload/start
{"file_path": "C:\\case\\disk.img", "file_type": "windows_ntfs", "file_hash": "<base64>", "size": 1073741824}
→ 200 {"upload_id": "…", "offset": 0, "size": 1073741824, "chunk_max": 16777216, "expires_at": "…"}
```

- **Fields.**
  - `file_hash` is the base64 of the file's plain content hash, as for `AppendIfExists`, and must be of the right length.
  - `file_type` is optional: at most 64 characters of `a-z`, `0-9`, `_`, `-` and `.`.
  - `size` is the file's size. It is required: a start without it (the removed v1 protocol, which had no sizes and no offsets) is refused.
- **Refusals.**
  - `400`: a bad field
  - `413`: a size over 256 GiB
  - `429` (with `Retry-After`): 16 uploads are already in progress (receiving, waiting their turn, or being stored). The 16 are shared by every token, so a token with `upload` can hold all of them: give `upload` only to clients trusted not to.
  - `507`: less free space than the size plus 128 MiB, and, while an export into the data root is being written, the bytes it has still to write

**2. Send chunks.**

```
POST /web/upload/chunk
Upload-Id: <upload_id>
Upload-Offset: <offset>
<at most chunk_max bytes>
→ 200 {"ok": true, "offset": <bytes the session holds>}   (and Upload-Offset)
```

- **Where a chunk goes.** A chunk must start at the offset the session holds.
  - A chunk entirely within what the session already holds is a retry whose answer was lost. It gets `200` and writes nothing.
  - Any other offset gets `409` with the held offset, in `offset` and in `Upload-Offset`. Send from there.
- **Refusals.**
  - `400`: a missing or bad `Upload-Offset`
  - `413`: a chunk over `chunk_max`, or past the declared size
  - `507`: the server's disk is full. The session keeps what it held before the chunk, and the chunk can be sent again
  - `404`: an unknown or expired session, or one another token started
- **One at a time.** Chunks of one session are written one at a time.

**3. Finalize.**

```
POST /web/upload/finalize
{"upload_id": "…"}
→ 200 {"done": true, "upload_id": "…", "outcome": "stored", "index_state": "complete",
       "evi_file": {"file_path": "…", "file_id": "…", "file_size": …, "chunk_map": {…}}}
```

- **Before it runs.** The session must hold every declared byte; otherwise the answer is `409` with the held offset.
- **What it does.** The server compares the bytes' hash with `file_hash`. A mismatch is `422`, and nothing is stored. Otherwise the file is stored and indexed. The evidence is named by the last element of `file_path`, and records `file_path` as where it came from.
- **Its turn.** Up to `--upload-jobs` uploads are stored at once, one by default. A finalized upload waits its turn when:
  - an upload of the same `file_path`, or of the same content, is being stored or waits before it. Such uploads are stored one after another, in the order their hash checks end. A later upload of the same content becomes an alias of the first.
  - a large upload (an image of 256 MiB or more once decoded) is being stored or waits before it. A large upload is stored alone.
  - the uploads being stored would, with it, hold more memory than the server allows. An upload stored alone always fits.

  Other uploads may be stored before one that waits for its `file_path` or content.
- **The result.**
  - `outcome` is `stored`, or `aliased` when the content was already stored. An aliased upload is recorded as an alias of the evidence holding it, and `file_type` is not applied to that evidence.
  - `warning` is set when the file was stored but its index is incomplete, or an EWF image's stored digests did not match.
- **A finalize that takes long.** When storing takes longer than 25 s, the answer is `202` with the session's status. Ask `status`, or call `finalize` again, for the result.
- **Once only.** A session is finalized once. Every later `finalize` or `status` call gets the same result.

**Status and abort.**

```
POST /web/upload/status {"upload_id": "…"}
→ {"upload_id": "…", "state": "receiving|finalizing|done|failed", "queued": true, "offset": …, "size": …,
   "expires_at": "…", "result": {…}, "error": "…"}

POST /web/upload/abort {"upload_id": "…"}
→ {"ok": true}
```

`queued` is `true` while a finalizing upload waits its turn to be stored, and is left out otherwise.

`abort` removes what the session received. A session being finalized cannot be aborted (`409`), even while it waits its turn.

**Expiry.**
- A session ends 30 minutes after its last chunk or status call, and in any case 24 hours after it started. What it received is then removed.
- A finished session's result is kept for 10 minutes.
- Sessions do not survive a restart: the server removes leftover uploads when it starts.

**During an export.** While the database is paused for an export (see [Pauses](#pauses)):
- `start`, `chunk`, `status` and `abort` work as usual, and the limit of 16 uploads in progress still applies.
- A finalized upload waits its turn until the database resumes, with `queued` set. Its `finalize` answers `202` once it has waited 25 s, as above.
- An upload being stored when the export begins gets `--export-drain-timeout` to finish. One still storing then is stopped, and goes back to its place in the queue with the bytes it received: the client sends nothing again. The log records `upload requeued`.

## Exports

A token with the `export` scope can have the server write a package of its database, and download it. The package is the one `dues package export` writes: a `.duesarc` file, never split. `dues package verify` checks it, and `dues package import` makes a database of it.

To write it, the server pauses the database (see [Pauses](#pauses)). Reads are refused from the end of the upload drain until the package is written, which for a large database is the whole copy. One export runs at a time.

All four endpoints need a token with `export`; none is open without one. They take and give JSON, except `download`.

**1. Start.**

```
POST /web/export/start
{"without_derived": false}
→ 202 {"export_id": "…", "state": "pausing", "without_derived": false, "created_at": "…",
       "bytes_written": 0, "bytes_expected": 52428800}
```

- **The request.** `without_derived: true` leaves out the full-text index (`fts/`) and the graph (`graph/`), which `dues reindex --fts`, `dues enrich` and `dues micro extract` rebuild. The body must be a JSON object: `{}` asks for the whole database.
- **The space check.** Before it answers, `start` sizes the database, for up to 20 s, and checks that the export folder's volume has room for the package plus 128 MiB. A database too large to size in 20 s is exported without this early check: the export checks the exact size once the database is paused, and fails then if the package does not fit.
- **The answer.** `202` with the export's status (below). Keep `export_id`: `status`, `cancel` and `download` take it.
- **Refusals.**
  - `409`: an export is running, or the database is still resuming after one
  - `409`: 4 written packages are kept; cancel one, or wait for one to expire
  - `503`: the server is shutting down, or the request ended while the database was being sized
  - `507`: the export folder's volume has less free space than the package needs plus 128 MiB
  - `500`: the free space could not be checked; the log has the detail

**2. Status.**

```
POST /web/export/status
{"export_id": "…"}
→ 200 {"export_id": "…", "state": "done", "without_derived": false, "created_at": "…",
       "bytes_written": 52301824, "bytes_expected": 52428800, "size": 52301824,
       "sha256": "<hex>", "content_sha256": "<hex>", "signed_by": "database", "key_id": "…",
       "download": "/web/export/download/<export_id>", "expires_at": "…"}
```

- `state`:
  - `pausing`: the server waits for the uploads being stored and the reads running to stop.
  - `packaging`: the package is being written.
  - `done`: the package is written and can be downloaded. The database may still be resuming.
  - `failed` or `cancelled`.
- `bytes_written` is what has been written of the package so far. `bytes_expected` is `start`'s estimate, above the final size: it counts the open database's files as they stand on disk, and the database keeps its newest log files at their full preallocated size until it closes, which can add a few hundred MiB. It is left out when sizing took more than 20 s.
- Once `done`:
  - `size` and `sha256` describe the package file.
  - `content_sha256` is the package's content hash, as `dues package export` prints it.
  - `signed_by` and `key_id` name the key that signed it. Both are left out when the package is unsigned.
  - `plaintext_components`, for an encrypted database, names the parts held in the clear (`fts`, `graph`).
  - `download` is the path to download it from.
- `expires_at`: when a written package is removed, or when the status of a failed or cancelled export is forgotten. It is left out while the export runs.
- `error` and `retryable` are set when the export failed:

  | `error` | `retryable` |
  | --- | --- |
  | `another pause of the database is running; retry when it ends` | yes |
  | `work on the database did not stop in time; retry later` | yes |
  | `the server stopped before the export was written` | yes |
  | `the database did not close cleanly and was recovered; export again` | yes |
  | `not enough free space for the package` | no |
  | `the export could not be written (export <id>)` | no |

  `retryable` means a new `start` may work. The log's `export failed` record has the detail.
- **Refusals.** `400` without `export_id`. `404` (`export not found or expired`) for an unknown ID, an export whose status was forgotten, or one another token started.

**3. Cancel.**

```
POST /web/export/cancel
{"export_id": "…"}
→ 202 or 200, with the export's status
```

- **While it runs** (`pausing` or `packaging`): `202`, with the state still running. The export stops, leaves no file, and turns `cancelled`; the database resumes.
- **Once written** (`done`): the package is removed, and the answer is `200` with the state `cancelled`. It is refused with `409` while the package is being downloaded. When the package cannot be removed (on Windows, while another program has it open), the answer is `500`, the export stays `done`, and a later cancel tries again. A sidecar that cannot be removed once its package is gone is left for the next start to remove.
- **Once ended** (`failed` or `cancelled`): `200`, and nothing changes.
- **Refusals.** As for `status`.

**4. Download.**

```
GET /web/export/download/<export_id>
Authorization: Bearer <token>
→ 200, the package
```

- **The token.** The `Authorization` header is required. A plain link, or a link with a `download` attribute, sends no token and gets `401`. A page downloads with `fetch` and saves the stream; `curl` and scripts send the header as they do for every other call.
- **Headers.**
  - `ETag: "sha256-<hex>"`: the package file's SHA-256, the same as `sha256` in the status.
  - `Repr-Digest: sha-256=:<base64>:` (RFC 9530): the same hash, of the whole file, on a partial answer too.
  - `Content-Type: application/zip`, and `Content-Disposition: attachment; filename=<export_id>.duesarc`.
  - `Last-Modified`: when the package was written. `Cache-Control: no-store`.
- **Ranges and conditions.** `Range` gets `206` (`416` past the end). To resume a download, send `Range` with `If-Range: <ETag>`: a package that changed is sent whole. `If-None-Match` gets `304` and `If-Match` `412` as usual. `HEAD` answers the headers without the body.
- **Refusals.**
  - `404`: an ID not in the form `start` gives, an unknown or expired export, or one another token started
  - `409`: the export is not written yet
  - `429` with `Retry-After: 30`: 4 downloads are running
  - `500`: the package file could not be opened
- **A stalled download.** A download whose client reads nothing for 1 min is cut. Resume it with `Range` and `If-Range`.
- **Checking it.** The SHA-256 of the saved file must equal the ETag's hex. Then run `dues package verify` on it.

**Who sees an export.** An export belongs to the token label that started it, as an upload does. Any other token is told, for its `status`, `cancel` and `download`, that it does not exist (`404`). The log records `export of another token` with that token's label.

**Signing.** The package is signed as `dues package export` signs it, by the actor the server's database was opened with: `--actor` when `dues server` was started with one, else the database actor (see `docs/OPERATIONS.md`, Keys and signing). When that actor cannot sign, as an encrypted database's actor without the password, the package is unsigned, and the status leaves out `signed_by`.

**How long packages are kept.**
- A written package is removed `--export-keep-hours` after it was written (default 24 hours). The server looks every minute. A package being downloaded is removed once the download ends.
- At most 4 written packages are kept at once. While 4 are kept, `start` is refused with `409`.
- The status of a failed or cancelled export is kept 10 minutes, then forgotten.
- **After a restart.** Exports that were running are gone, and the server removes what they left. Written packages of this database are kept: the same token label can ask their status and download them until they expire. Their expiry was fixed when they were written; a new `--export-keep-hours` applies to new exports only. A label therefore owns its packages across a restart: give a label to someone new only once the packages made under it are gone.

**The export folder.**
- Each export is two files: `<export_id>.duesarc`, the package, and `<export_id>.json` beside it. The JSON file records the owner's label, the database, the hashes and the expiry; it holds no token.
- The default folder is the data root's `exports`. A folder outside the data root is checked when the server starts:
  - Only one DUES server may use it: the server holds `.dues-exports.lock` in it while it runs.
  - On Linux and macOS it must belong to you or root, and neither its group nor others may write it (`chmod go-w`). On Windows access comes from the folder's ACLs and is not checked: give only your account access to it. A folder at the top of a drive, such as `D:\exports`, usually lets every signed-in user change it.
  - A folder inside another database's data root is refused: that database's server cleans its own `exports` folder.
  - A link that leads into the data root is refused: a package never holds packages.
- Packages of a database that is not encrypted hold its evidence in the clear. The server writes both files readable by its own account only (mode 0600 on Linux and macOS); keep the folder as private as the data root.
- The server removes only its own files there: unfinished packages, expired ones, and an export's file whose other half is missing or does not match it. Another database's packages, and any other file, are left alone.
- While a package is written into the data root's `exports` folder, an upload `start` counts the bytes the export has still to write against the free space, as both share the volume.

## Pauses

An export needs the database closed while it copies it. The server therefore pauses the database, in this order:

1. Uploads waiting their turn stay queued. Uploads being stored get `--export-drain-timeout` (default 5 min) to finish. Those still storing then are stopped and requeued, in their place, with the bytes they received.
2. Reads are refused from then on. The reads still running get 60 s to finish; then they are cut. Both answer `unavailable` with `Retry-After: 5`.
3. The database is closed, and the package is written. The server keeps the data root's lock, so no other DUES process opens the database meanwhile.
4. The database is reopened. Reads are served again, and queued uploads are stored.

- **What is refused.** `GetEviFiles`, `GetPartiFiles`, `GetIdxFiles`, `Search` and `AppendIfExists` are refused from step 2 until the database is reopened. For a large database that is the whole copy. Uploads, `StreamFile` and the export endpoints stay open.
- **Health.** `/readyz` answers `503 paused` throughout, so a balancer that routes on it takes the server out until the database reopens.
- **Giving up.** A cancel during steps 1 and 2 gives the pause up, and the server goes on as before. So does work that has not stopped 10 s after it was cut: the export then fails with `work on the database did not stop in time; retry later`. The log records `server pause given up`.
- **Reopening.** The server tries 4 times in about 21 s, and each try needs 128 MiB free on the database's volume. When every try fails, the server stops with an error, and the log records `server resume given up`. A package already written is kept, and served again after the restart.
- **Repeated pauses.** Each pause refuses reads for up to about 70 s before the copy even starts. A token with `export` can start and cancel exports again and again: give it only to trusted operators.

## Shutdown

On the first interrupt or termination signal, the server shuts down in this order:

1. It stops taking new requests, and `/readyz` turns `503`.
2. The requests being served get 25 s to finish.
3. Their work is then told to stop: searches end, and an upload being stored stops, to be uploaded again. An upload still waiting its turn fails with `503`, to be uploaded again too. A running export stops and leaves no file, and package downloads are cut.
4. That work gets 10 s more to return.
5. If work is still running after that, the server exits without closing the database. badger recovers it at its next open.

A database paused for an export when the server stops is released, not reopened: the next open finds it closed.

A stop by signal exits with code 130 (interrupt) or 143 (termination).
