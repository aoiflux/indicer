# DUES server API

`dues server` serves one DUES database over Connect, gRPC and gRPC-Web, and offers HTTP endpoints for browser uploads. This page is for people who write clients for it and for people who run it.

The service is defined in [`protos/dues.proto`](../protos/dues.proto). The generated Go code is in `pb/` and `pb/pbconnect/`.

## Running it

```
dues server [--listen HOST:PORT]... [--token-file PATH] [--cors-origin ORIGIN]... [global options]
```

- **Addresses.** By default the server listens on `127.0.0.1:50051` and `[::1]:50051` only. If the host has no IPv6 loopback, the IPv6 address is skipped and a note is printed. `--listen` replaces the defaults and may be given more than once.
- **Token.** A non-loopback address (`0.0.0.0`, `::`, `:50051`, a LAN address or a host name) is refused unless a bearer token is set. The token must be at least 32 visible ASCII characters. It comes from `--token-file PATH` (the file's first line) only. `--token` was removed, because other users can see a command line; passing it is a usage error that names `--token-file`. The `DUES_TOKEN` environment variable is ignored.

  Once a token is set, every listener requires it, loopback included.
- **Encryption.** The server speaks plain HTTP. A warning is printed when it listens on a non-loopback address. To serve remote clients, put a TLS-terminating proxy or an SSH tunnel in front of it.
- **Configuration errors.** A bad address, origin or token exits with code 2 before the database is opened.

Examples:

```
dues server --dbpath ./caseA
dues server --listen 0.0.0.0:50051 --token-file ./token
dues server --listen 192.168.1.10:50051 --token-file ./token --cors-origin https://ui.example.org
```

## Authentication

When a token is set, send it on every request except health checks:

```
Authorization: Bearer <token>
```

A missing or wrong token gets `401 Unauthorized` with `WWW-Authenticate: Bearer realm="dues"`. Connect and gRPC clients see this as the `unauthenticated` code. The server compares tokens in constant time.

## Browser access

- **Origins.** A page may call the server when its `Origin` is a loopback origin (`localhost`, a name under `.localhost`, or a loopback IP, any port, http or https) or exactly one of the `--cors-origin` values. Origins are compared without regard to case, and with default ports left out.
- **Refused origins.** A request that carries any other `Origin`, `null` included, gets `403` before it is served. This applies to preflight and non-preflight requests alike, so a foreign page cannot get the browser to send even a "simple" request (CSRF).
- **No Origin.** A request without an `Origin` header is not from a browser page, and is served.
- **Preflights.** Preflights are answered `204` without needing the token, and allow:
  - methods `GET, POST, OPTIONS`
  - request headers `Authorization`, `Content-Type`, the Connect and gRPC-Web headers, `Upload-Id` and `Upload-Offset`
- **Exposed headers.** `Grpc-Status`, `Grpc-Message`, `Grpc-Status-Details-Bin`, `Request-Id`, `Upload-Offset` and `Deprecation`.
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
- **Internal failures.** Any other failure, a panic included, is `internal`. Its message names only the request ID, `internal error (request <id>)`, so no internal detail (paths, keys) reaches clients.

## Health

- `GET /healthz` answers `200 ok` while the process serves.
- `GET /readyz` answers `200 ready`. It answers `503` while the server shuts down, or once its database is closed.

Neither needs the token. Any other path, `/` included, is `404` (or `405` for the wrong method).

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
  - optionally `size`: more bytes than this are refused, and fewer fail the upload
- Each later message carries `file` bytes, in order.
- It is stored like a browser upload (below). The reply is the stored evidence.
- A client that goes away mid-stream leaves nothing behind.
- Errors:
  - `data_loss`: a hash mismatch
  - `resource_exhausted`: the server is full or busy
  - `failed_precondition`: a stream that ends short of its `size`

## Browser uploads (v2)

An upload is a session. Its bytes are written in order, at offsets the client names, so an upload that is cut off can carry on where the server says it stopped. All endpoints take and give JSON, except `chunk`, which takes raw bytes.

**1. Start.**

```
POST /web/upload/start
{"file_path": "C:\\case\\disk.img", "file_type": "windows_ntfs", "file_hash": "<base64>", "size": 1073741824}
→ 200 {"upload_id": "…", "offset": 0, "size": 1073741824, "chunk_max": 16777216, "expires_at": "…"}
```

- **Fields.**
  - `file_hash` is the base64 of the file's plain content hash, as for `AppendIfExists`, and must be of the right length.
  - `file_type` is optional: at most 64 characters of `a-z`, `0-9`, `_`, `-` and `.`.
  - `size` is the file's size.
- **Refusals.**
  - `400`: a bad field
  - `413`: a size over 256 GiB
  - `429` (with `Retry-After`): 16 uploads are already in progress
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
  - `413`: a chunk over `chunk_max`, or past the declared size
  - `507`: the server's disk is full. The session keeps what it held before the chunk, and the chunk can be sent again
  - `404`: an unknown or expired session
- **One at a time.** Chunks of one session are written one at a time.

**3. Finalize.**

```
POST /web/upload/finalize
{"upload_id": "…"}
→ 200 {"done": true, "upload_id": "…", "outcome": "stored", "index_state": "complete",
       "evi_file": {"file_path": "…", "file_id": "…", "file_size": …, "chunk_map": {…}}}
```

- **Before it runs.** The session must hold every declared byte; otherwise the answer is `409` with the held offset.
- **What it does.** The server compares the bytes' hash with `file_hash`. A mismatch is `422`, and nothing is stored. Otherwise the file is stored and indexed: one upload is stored at a time. The evidence is named by the last element of `file_path`, and records `file_path` as where it came from.
- **The result.**
  - `outcome` is `stored`, or `aliased` when the content was already stored. An aliased upload is recorded as an alias of the evidence holding it, and `file_type` is not applied to that evidence.
  - `warning` is set when the file was stored but its index is incomplete, or an EWF image's stored digests did not match.
- **A finalize that takes long.** When storing takes longer than 25 s, the answer is `202` with the session's status. Ask `status`, or call `finalize` again, for the result.
- **Once only.** A session is finalized once. Every later `finalize` or `status` call gets the same result.

**Status and abort.**

```
POST /web/upload/status {"upload_id": "…"}
→ {"upload_id": "…", "state": "receiving|finalizing|done|failed", "offset": …, "size": …,
   "expires_at": "…", "result": {…}, "error": "…"}

POST /web/upload/abort {"upload_id": "…"}
→ {"ok": true}
```

`abort` removes what the session received. A session being finalized cannot be aborted (`409`).

**Expiry.**
- A session ends 30 minutes after its last chunk or status call, and in any case 24 hours after it started. What it received is then removed.
- A finished session's result is kept for 10 minutes.
- Sessions do not survive a restart: the server removes leftover uploads when it starts.

## Browser uploads (v1, deprecated)

A `start` without `size` opens a v1 session. Its chunks may leave out `Upload-Offset`, and are appended. Its `finalize` waits for the result however long storing takes. v1 sessions are still served, with `Deprecation: true` on their responses and a warning in the server's log. Clients should move to v2, which can resume.

## Shutdown

On the first interrupt or termination signal, the server shuts down in this order:

1. It stops taking new requests, and `/readyz` turns `503`.
2. The requests being served get 25 s to finish.
3. Their work is then told to stop: searches end, and an upload being stored stops, to be uploaded again.
4. That work gets 10 s more to return.
5. If work is still running after that, the server exits without closing the database. badger recovers it at its next open.

A stop by signal exits with code 130 (interrupt) or 143 (termination).
