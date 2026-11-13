# DUES server API

`dues server` serves one DUES database over Connect, gRPC and gRPC-Web, and offers HTTP endpoints for browser uploads. This page is for people who write clients for it and for people who run it.

The service is defined in [`protos/dues.proto`](../protos/dues.proto). The generated Go code is in `pb/` and `pb/pbconnect/`.

## Running it

```
dues server [--listen HOST:PORT]... [--token-file PATH] [--cors-origin ORIGIN]... [--upload-jobs N] [global options]
```

- **Addresses.** By default the server listens on `127.0.0.1:50051` and `[::1]:50051` only. If the host has no IPv6 loopback, the IPv6 address is skipped and a note is printed. `--listen` replaces the defaults and may be given more than once.
- **Tokens.** A non-loopback address (`0.0.0.0`, `::`, `:50051`, a LAN address or a host name) is refused unless a bearer token is set. Tokens come from `--token-file PATH` only; [Tokens and scopes](#tokens-and-scopes) describes the file. `--token` was removed, because other users can see a command line; passing it is a usage error that names `--token-file`. The `DUES_TOKEN` environment variable is ignored.

  Once a token is set, every listener requires one, loopback included. The startup lines name each token by its label, with its scopes; the token itself is never printed or logged.
- **Encryption.** The server speaks plain HTTP. A warning is printed when it listens on a non-loopback address. To serve remote clients, put a TLS-terminating proxy or an SSH tunnel in front of it.
- **Uploads at once.** `--upload-jobs N` stores up to N uploads at once: from 0 to 64, default 1, and 0 picks a number for this machine. No more than 16 uploads may be in progress, so a higher number is cut to 16, with a note. Low-resource mode stores one at a time. See [Browser uploads](#browser-uploads) for the order they are stored in.
- **Configuration errors.** A bad address, origin, token or `--upload-jobs` value exits with code 2 before the database is opened.

Examples:

```
dues server --dbpath ./caseA
dues server --listen 0.0.0.0:50051 --token-file ./token
dues server --listen 192.168.1.10:50051 --token-file ./token --cors-origin https://ui.example.org
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
| `export` | the export routes, when a release adds them |
| none | `GET /healthz`, `GET /readyz` |

Scopes say what a token may do. They do not divide the database: every token works on the one store. A token with `upload` sees, through `AppendIfExists`, whether content whose hash it holds is stored, and the stored evidence's `file_id`, size and chunk map. Uploads of the same client path, or of the same content, by different tokens are stored one after another, and a later upload of stored content becomes an alias, whichever token stored it first.

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
  - request headers `Authorization`, `Content-Type`, the Connect and gRPC-Web headers, `Upload-Id` and `Upload-Offset`
- **Exposed headers.** `Grpc-Status`, `Grpc-Message`, `Grpc-Status-Details-Bin`, `Request-Id` and `Upload-Offset`.
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

## Errors

Every response carries a `Request-Id` header. The server's log records each request under that ID, and records the detail of every failure.

- **HTTP endpoints** (health, uploads) answer errors as JSON:

  ```json
  {"error": "upload session not found or expired", "request_id": "0199…"}
  ```

- **RPCs** answer with a Connect or gRPC status code. A failure the client can act on has its own code and message:
  - `invalid_argument` for a bad request
  - `not_found` for an unknown ID
  - `resource_exhausted` for a message over the limit
  - `deadline_exceeded` for a search over its budget
  - `permission_denied` for a token without the procedure's scope

  A refusal before the procedure is found (a wrong token, `401`, or a busy or stopping server, `503`) is a bare HTTP status: Connect and gRPC clients see only its code, `unauthenticated` or `unavailable`.
- **Internal failures.** Any other failure, a panic included, is `internal`. Its message names only the request ID, `internal error (request <id>)`, so no internal detail (paths, keys) reaches clients.

## Health

- `GET /healthz` answers `200 ok` while the process serves.
- `GET /readyz` answers `200 ready`. It answers `503` while the server shuts down, or once its database is closed.

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
  - `507`: less free space than the size plus 64 MiB

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

## Shutdown

On the first interrupt or termination signal, the server shuts down in this order:

1. It stops taking new requests, and `/readyz` turns `503`.
2. The requests being served get 25 s to finish.
3. Their work is then told to stop: searches end, and an upload being stored stops, to be uploaded again. An upload still waiting its turn fails with `503`, to be uploaded again too.
4. That work gets 10 s more to return.
5. If work is still running after that, the server exits without closing the database. badger recovers it at its next open.

A stop by signal exits with code 130 (interrupt) or 143 (termination).
