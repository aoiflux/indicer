# Operating DUES

This page is for the people who run DUES and support it: where its data and
logs are, what it does when interrupted or when something fails, what its exit
codes mean, and how it behaves on read-only media and full disks. The server
and the compression library have their own pages: `docs/SERVER_API.md` and
`docs/COMPRESSION_ABI_REFERENCE.md`.

## The data root

Every command works on one data root: `--dbpath`, or `~/.dues` when it is not
given. A data root holds:

| Entry | What it is |
|---|---|
| `dues-manifest.json` | The database's layout and encryption settings, in plain JSON with no secrets. Written atomically. |
| `kvdb/` | The badger database: every record, chunk location and relation. It also holds `DUES-MANIFEST.mirror.json`, a copy of the manifest. |
| `blob/` | Chunk blobs (file mode) or container files (container mode). Older hierarchical databases also have `blob/blocks/*.bidx`. |
| `graph/` | The graph store for enrichment and micro-artefacts, when either has run: the image `graphene.csr`, the log written since `graphene.wal`, the logs earlier compactions retired `graphene.NNNNNN.wal`, and the schema marker `dues.schema.json` (see Graph compaction) |
| `fts/` | The full-text index, when one was built |
| `dues.keys/` | The actors' signing keys and the keyring of their public keys (see [Keys and signing](#keys-and-signing)) |
| `uploads/` | Part files of server uploads in progress |
| `exports/` | Packages written into the data root, `dues server`'s exports among them (its default `--export-dir`); packages of the root leave it out (see [Packages](#packages)) |
| `.dues.lock` | The root lock (see below) |

The layout is fixed when the database is created:
- chunk size (`--chonksize`);
- chunk hash (`--hash-algo`);
- storage (file or `--container`);
- table compression (`--quick` turns it off);
- encryption.

An existing database keeps its own. A flag that contradicts it is an error
(exit 2) that names the flag.

Paths inside the database are relative to the data root, so a data root can be
moved or copied as a whole. Copy it only while no DUES process uses it, or
make a package of it (see [Packages](#packages)), which keeps writers out
while it copies.

On Linux and macOS a new data root, and the directories DUES creates in it,
are created with mode 0700, and chunk blobs and containers with mode 0600: they
hold evidence content, in the clear when the database is not encrypted. Data
roots created by earlier releases keep their modes (0755 and 0644); to close
them, run `chmod -R go-rwx <root>`. On Windows, access comes from the folder's
ACLs.

### Encryption and passwords

- A database created without a password is not encrypted. DUES says so when it
  creates the database, and `list` and `stats` say so too.
- A database created with a password is encrypted with a key derived by
  Argon2id from the password and a per-database salt.
- Opening an encrypted database without its password, or with the wrong one,
  is an error (exit 2).
- Databases written before the manifest existed keep their old key scheme and
  stay readable.

The password is read from the first of these that is given:
1. `--password-file PATH` (its first line);
2. `--password-stdin` (the first line of standard input);
3. the config file's `password-file` (see [The config file](#the-config-file)).

So a config file with `password-file` creates every new database encrypted,
unless the command line gives `--password-file ''` (no password) or
`--password-stdin`. A database created without a password ignores it.

An empty password means no password. `--password`/`-p` was removed, because
other users can see a command line; passing it is a usage error that names
these two. The `DUES_PASSWORD` environment variable is ignored.

## Concurrent use

The root lock serialises access between DUES processes:
- Commands that only read take it shared, so any number can run at once:
  `list`, `stats`, `search`, `near`, `restore`, `micro list`, `package
  export`, and `repair` without `--fix` or `--upgrade`.
- Commands that write take it exclusively: `store`, `repair --fix`,
  `repair --upgrade`, `reindex`, `enrich`, `micro extract`, `server`, `tui` and
  `reset`.

A command that cannot take the lock fails at once with
`database is in use by another DUES process (<root>)`. It does not wait.

Inside one process, the server and the TUI run their own work side by side. A
reset in the TUI is refused while other jobs run.

`dues store <folder> --store-jobs N` stores up to N evidence of the folder at
once (default 1; 0 picks a number for the machine). It suits folders of many
evidence under 256 MiB: a larger one already keeps the machine busy, so it is
stored alone. A folder of mostly large images gains little, and was slower when
the folder and the database shared one disk. Copies of one content are stored
one after another, and the report reads as a store of one at a time.
Evidence stored at once share the store workers between them; a
`--store-workers` given on the command line or in the config file is the
width of each instead.
The evidence stored at once must fit in a quarter of the free memory: a
number that does not fit is refused before anything is stored, and 0 picks
no more than fit. Large chunks (`--chonksize`) are what usually make them
not fit.
`dues reindex --store-jobs N` does the same for the evidence a reindex
selects. That suits `--all` over many small evidence. The config file's
`store.store-jobs` applies to `store` only.

`dues server --upload-jobs N` stores up to N uploads at once, by the same
rules: default 1, 0 picks a number for the machine, uploads of one content
(or one client path) one after another, a large one alone, and the uploads
stored at once share the store workers. Uploads arrive one by one, so the
server does not refuse a number that may not fit: an upload that would not
fit in memory beside the ones being stored waits for them. No more than 16
uploads may be in progress, so a number above 16 is cut to 16. The log
records `upload queued` when an upload must wait its turn, `upload storing`
with its wait when it starts, and `upload stored` or `upload failed` when it
ends.

An export through the server pauses the database, one export at a time. The
uploads being stored get `--export-drain-timeout` (default 5 min) to finish,
and are then stopped and requeued; the reads still running get 60 s, and are
then cut. From the end of the upload drain until the package is written, every
read is refused with `Retry-After: 5`, and `/readyz` answers `503 paused`. For
a large database that is the whole copy, so plan exports for when reads can
wait. Uploads keep arriving meanwhile, and are stored once the database
reopens. `docs/SERVER_API.md` (Pauses) gives the details.

## After a crash or a kill

DUES makes chunks durable before it records them, and records an ingest's
progress in its evidence record. A process that dies therefore leaves either
work that can be finished, or work that is marked for retry. It never leaves
records that point at missing data.

Every writable open recovers what an earlier process left:
- An ingest that had not finished storing is marked failed. Storing the same
  file again resumes it under the same ID.
- An ingest whose chunks were all stored is checked, then committed. If the
  same content was committed meanwhile, it becomes an alias and its duplicate
  records are removed.
- An unfinished duplicate cleanup is finished.
- An index pass that was running is marked `interrupted`.
- DUES temporary files are removed from the data root, and part files from
  `uploads/`.

`dues repair` lists what recovery would do, without changing anything.
`dues repair --fix` does it now. Add `--reindex-failed` to rebuild the indexes
that failed or were interrupted. `dues reindex --failed` does the same at any
time.

If a database was not closed cleanly and a read-only command opens it, DUES
upgrades that open to a writable one to recover, and says so. When another
process holds the database, or the media is read-only, that is not possible.
The command then fails with `database needs recovery`, and
`dues repair --fix` must run with write access.

`dues repair --verify` reads back every stored chunk and checks it against its
hash. It prints `OK` or `DAMAGED` for each evidence and exits 1 if any is
damaged.

`dues repair` prints one JSON document on stdout, whatever flags ran, so a
script can read it whole:

- the recovery report: `totalProblematic`, the counts, `fixed`, and
  `evidence`, one row for each evidence an ingest left unfinished;
- a section for each step a flag ran, absent otherwise:
  - `upgrade`: `files`, `records`, `migrated`, `alreadyStored`,
    `damagedCount` and `damaged`; `recordedAsContainer`; and `blockFiles`
    (`none`, `kept` or `deleted`) with `blockFileCount`;
  - `quarantined`: each index `--upgrade` set aside, as `id`, `name` and
    `format`;
  - `revrel`: the `evidence` that lacked reverse relations, and the relations
    `added`;
  - `reindex`: each evidence's `id`, `name`, `outcome` (`ok`, `incomplete` or
    `failed`) and `error`, with the `incomplete` and `failed` counts;
  - `verify`: `verified`, `damaged`, and `results`, each with `id`, `name`,
    `size` and, when damaged, `problem`;
- `error`: the error the command ends with, if any.

The document is printed once the database is open, also when a step fails:
`repair --verify` exits 1 on damaged evidence and still prints every result.
The lines each step prints, and the question `--upgrade` asks before it
deletes the block index files, go to stderr.

A restore, a report or an archive that is interrupted leaves nothing at its
target. Output is written to a temporary file beside the target and renamed
into place only at the end. The next write to the same path removes any
temporary file an interrupted one left.

When `dues server` starts, it tidies its export folder:
- It removes unfinished exports (`*.duesarc.partial` and their temporary
  files), and an export's package or JSON file whose other half is missing.
- It keeps the finished exports of this database, and serves them to the
  same token label until they expire. It removes those that have expired.
- It leaves another database's exports, and any other file, alone.

The startup lines say how many it removed, kept and left.

## Signals and interruption

The first Ctrl-C (SIGINT), or SIGTERM, cancels the command. It stops at a safe
point, removes its temporary files, closes the database and exits with 130
(interrupt) or 143 (termination). On Unix, SIGHUP is treated like SIGTERM.

On Windows:
- Ctrl-C and Ctrl-Break count as an interrupt.
- Closing the console window, logging off and shutting down count as a
  termination.

A second signal ends the process at once with exit 130. Recovery at the next
writable open then finishes what it left.

When the console window is closed, Windows kills the process about five
seconds later, whether or not it has finished stopping. Recovery handles what
that leaves; the window cannot be extended.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | The operation failed |
| 2 | A usage error (bad flag or argument, a flag that contradicts the database's layout, an existing output without `--force`). Or the operation completed but needs attention: evidence stored while its index is incomplete, an E01 whose stored MD5/SHA-1 does not match, a restore of incomplete content, a search or near that could not read everything, objects `micro extract` could not read. |
| 70 | An internal error. DUES recovered a panic; the crash file has the details. |
| 130 | Interrupted (SIGINT, Ctrl-C, Ctrl-Break) |
| 143 | Terminated (SIGTERM, console close) |

A crash the Go runtime cannot recover from (a stack overflow, say) exits with
the runtime's own code, 2. The crash file then holds its trace, so check it
when exit 2 comes with no message.

The message on stderr names the kind: `dues: error: ...` for a failure (exit 1),
`dues: warning: ...` for an operation that completed but needs attention
(exit 2). On a terminal the same lines read `✗ error: ...` and
`▲ warning: ...`.

## Output and the terminal

Results go to stdout and everything else to stderr: the run header, progress,
notes, warnings and errors. On a pipe nothing holds an escape code, and
progress is one plain line when a phase starts, one when it ends, and, while
the command runs, one heartbeat line: every 15 seconds at first, then every
tenth of the time it has run, never more than 5 minutes apart (a two-hour
store prints about 55). Under `--ci` DUES behaves as on a pipe, and prompts
refuse.

| Flag | Effect |
|---|---|
| `--color` | `auto` (256 colours on a terminal, none on a pipe), `always`, `never`, `ansi` (the theme's 16 colours) or `truecolor` |
| `--plain` | no colour, no redraw, ASCII |
| `--ci` | as on a pipe, for CI logs |
| `--theme` | `dark` or `light`: the terminal's background |
| `--taskbar` | progress on the terminal's tab and taskbar |
| `--no-fun` | no extras |

DUES reads no environment variable: `NO_COLOR`, `FORCE_COLOR`, `TERM`, `CI`
and the like change nothing. To keep a switch on, put it in the config file.

[CLI_UX.md](CLI_UX.md) has the rest: palette, marks, the progress display and
the accessibility review.

## The config file

Settings kept between runs go in a JSON file. DUES reads, in this order of
choice:
1. the file `--config FILE` names, which must exist;
2. none, under `--no-config`;
3. `~/.config/dues/dues.json`, when it exists, where `~` is the home
   directory the account database records (never `HOME` or `USERPROFILE`).
   With no home directory there is no default file.

`--config` and `--no-config` together are a usage error. The compression
command reads `~/.config/dues/compression.json` the same way.

The file is one JSON object, at most 64 KiB, in UTF-8. Its keys are flag
names, exactly as typed; a command's own settings go in an object named after
it (`store`, `restore`, `server`) and apply to that command only. The one
exception is `dues tui`: it stores and restores in the same process, with no
tuning flags of its own, so the `store` and `restore` objects tune it too.
`dues server` does not take them: it stores uploads with the default store
settings, since uploads run side by side and one upload's worker counts would
be multiplied by the number running. Its own `server.upload-jobs` sets how
many run.
Paths must be absolute: `~` is not expanded. `dues help config` lists the
keys.

| Key | Where |
|---|---|
| `color`, `plain`, `ci`, `no-fun`, `theme`, `taskbar` | top level |
| `log-dir`, `temp-dir`, `debug`, `graph-compact` | top level |
| `dbpath`, `password-file`, `preset`, `compress-level`, `low` | top level |
| `pipeline`, `hash-strategy`, `io-engine`, `io-uring-queue-depth`, `store-workers`, `store-queue`, `store-jobs` | `store` |
| `restore-workers`, `restore-queue`, `restore-buffer-mb` | `restore` |
| `listen`, `token-file`, `cors-origin`, `upload-jobs`, `export-dir`, `export-keep-hours`, `export-drain-timeout` | `server` |
| `log-dir`, `temp-dir` | `compression.json`, top level |

`server.export-drain-timeout` is a duration string, such as `"90s"` or
`"10m"`; `server.export-keep-hours` is a number of hours.

The layout of a new database (chunk size, container, hash algorithm, quick)
is not a key: a database fixes it when it is created, and a value that
contradicts an existing database would make every command fail. Neither are
choices for one run (`--fix`, `--force`, `--out`, ...) or the password.

Precedence:
- A flag wins over the file, and the file over the default.
- A preset on the command line (`--preset`, `-Q`, `-o`, `-L`) replaces the
  file's `preset`, `compress-level` and `low`.
- A list on the command line (`--listen`, `--cors-origin`) replaces the
  file's.
- `--no-plain`, `--no-ci`, `--no-taskbar`, `--no-debug` and `--no-no-fun`
  undo a switch the file turns on.

A file that cannot be used stops every command, help included, with exit 2
before anything is written: unreadable, not a regular file, over 64 KiB, not
UTF-8, bad JSON, an unknown or miscased key, a repeated key, `null`, a wrong
type, a bad value or a relative path. The message names the file, the line
and column, and the key; every bad value is reported at once. For the default
file it adds that `--no-config` runs without it. A value a later check
refuses (a temporary directory that does not exist, too many workers) says
`(set in config file PATH)`.

On Linux and macOS the file must belong to you or root, and neither its group
nor others may write it (`chmod go-w`), as ssh asks of its files. On Windows
access comes from the file's ACLs and is not checked.

`dues config` prints, as JSON on stdout, the file read and every key with the
value in effect and its source (`flag`, `config` or `default`). It opens no
database and reads neither the password file nor the token file.

## Server tokens

`dues server --token-file PATH` reads the server's bearer tokens. The file
holds one token on its first line, which may read and upload, or, after a
`#dues-tokens v1` header, one `<token> <scopes> <label>` per line with the
scopes `read`, `upload`, `export` or `all`. `docs/SERVER_API.md` describes
the format and what each scope allows.

- **The file.** It is read by the config file's rules above: a regular file
  of at most 64 KiB, and on Linux and macOS one that belongs to you or root
  and that neither its group nor others may write. Others may read it, as
  they can a mounted container secret. A file that cannot be used stops the
  server with exit 2, naming the line, never the token.
- **Labels in the log.** The log names a token by its label, in the field
  `bearer`: on `server request`, `server scope refused`, `upload started`,
  `upload stored`, `upload failed` and `upload of another token`, and on the
  export records (see [Server exports and pauses](#server-exports-and-pauses)).
  With `upload started` and its `request_id`, `upload stored` ties each stored
  evidence to the token that sent it; the export records tie each package to
  the token that made it and each download to the token that took it. No
  field holds the token itself.
- **Export is never anonymous.** A server without tokens refuses every export
  route. Only a token whose scopes include `export` (or `all`) may export, and
  it takes a copy of the whole case and pauses the database each time: give
  it only to trusted operators.
- **Scopes are not tenants.** Every token works on the one database. A token
  with `upload` learns whether content it holds is stored, and the 16
  uploads in progress are shared by all tokens. Give `upload` only to
  clients trusted not to take every slot.
- **Revoking a token.** In a `#dues-tokens v1` file, remove its line, or
  put `#` before it. In a one-token file, replace the token: comments need
  the header, so a `#` before that token stops the server from starting.
  Then restart the server. A restart ends every upload in progress, of every token;
  their clients upload again.

## Logs and crash files

DUES writes its log to the first directory it can write in, in this order:
1. `--log-dir`, or the config file's `log-dir`;
2. the per-user cache directory: `AppData\Local\dues\logs` in the user's
   profile on Windows, `~/.cache/dues/logs` on Linux and
   `~/Library/Caches/dues/logs` on macOS, where `~` is the home directory the
   account database records;
3. `dues-<user>/logs` in the temporary directory (`--temp-dir`, else `/tmp`,
   or `AppData\Local\Temp` in the profile on Windows).

DUES finds these directories from the operating system, never from `HOME`,
`XDG_CACHE_HOME`, `LOCALAPPDATA`, `TMPDIR` or `TEMP`.

If none is writable, DUES prints one warning and runs without a log. A logging
failure never stops a command.

Each process writes its own files, named
`<component>-<UTC time>-<pid>.ndjson`. The component is `dues` for the CLI
and `qwikpak` for the compression command. Each line is one JSON object with
`run_id`, `pid` and `component`. A file rotates at 50 MB, and the newest 10
files are kept per directory. `--debug` adds debug-level entries.

Each run's log holds one `settings` record: the config file read (or `none`
and why), every presentation, log, temporary-file and database setting in
effect, and whether a flag, the config file or the default set it. When an
explicit `--log-dir` could not be written, it also gives the one asked for.
The password is never logged; its entry, `unlock`, says where it came from
(`password-file`, `password-stdin` or `none`) and whether a flag or the
config file chose it.

A run whose options are refused logs the error, as `command failed`, and no
`settings` record. A broken config file stops `dues` before its log opens, so
that run writes no log; the compression command logs it.

Values of fields whose names mention a password, secret, token, credential
or authorization, or end in `key`, are replaced with `[REDACTED]`. So are the values of
the removed `--password` and `--token` flags, should an old script still pass
them.

Each run also creates `crash-<run id>.txt` in the log directory and removes it
when the run ends, a second Ctrl-C included, if nothing was written to it. The
file receives:
- the report of a panic DUES recovered (exit 70);
- the runtime's trace of a crash nothing can recover from: a concurrent map
  write, a stack overflow, a panic on an unguarded goroutine.

After a crash, DUES prints the crash file's path and the run ID, which matches
the `run_id` of the log entries.

The compression library logs fully only when its host passes a `log_dir` to
`DuesConfigure` ([COMPRESSION_ABI_REFERENCE.md](COMPRESSION_ABI_REFERENCE.md)).
Otherwise it writes warnings and errors to the cache directory, in a file
created at the first such entry.

### Execution traces

`--trace FILE` writes a Go execution trace of the run to FILE, for
`go tool trace FILE`. FILE must not exist yet: DUES never writes a trace over
a file, and refuses the run with exit 2. The trace is closed when the command
ends, and also when a second Ctrl-C ends it at once, so the file still reads.

Each evidence a store or a reindex works on is a task in the trace, named
`store evidence` or `reindex evidence`, with its path or evidence ID logged
in it. The steps of each are regions of its task:

| Region | Step |
|---|---|
| `open` | Open the evidence, and decode its container |
| `fingerprint` | The quick fingerprint |
| `identify` | Look for the content among the stored evidence, hashing it when that is needed |
| `begin` | Record the attempt |
| `chunks` | The chunk pass |
| `index` | The index pass; in a store, it runs beside the chunk pass on a goroutine of its own |
| `flush` | Write what the chunk pass found |
| `index-wait` | Wait for what is left of the index pass |
| `commit` | Commit the evidence |
| `partition-hashes` | Record each partition's hash |
| `fts`, `enrich` | The full-text index and the graph |

In a folder store or a reindex with `--store-jobs` above 1, a worker that
waits until an item may start shows a `held` region.

The log records `trace START` and `trace COMPLETE` with the file's path. The
flag is for one run: it has no config key, and the `settings` record does not
list it. A trace grows with what the run's goroutines do: a store of 256 MiB
in eight files wrote 0.7 MB.

### Server exports and pauses

`dues server` logs each export, and each pause of the database it needs. An
export's records carry `export_id`; a pause's carry `pause_for`, which is
`export <export_id>`. `bearer` is the token's label, and `request_id` matches
the access log's `server request`, which gives the status and bytes sent.

| Record | Level | Fields | When |
|---|---|---|---|
| `export started` | info | `export_id`, `request_id`, `bearer`, `without_derived`, `bytes_expected` | `start` was accepted |
| `export sizing skipped` | warn | `export_id`, `elapsed_ms` | sizing the database took more than 20 s; the export goes on |
| `export sizing failed` | error | `export_id`, `request_id`, `error` | the database could not be sized; `start` answered `500`. A request that ended meanwhile is not recorded |
| `server pause started` | info | `pause_for`, `drain_ms`, `uploads_storing`, `uploads_waiting`, `requests` | the pause begins |
| `server pause cancelled uploads` | warn | `pause_for`, `count` | uploads still storing after the drain were stopped |
| `upload requeued` | info | `upload_id`, `bearer`, `attempt` | a stopped upload went back to its place in the queue |
| `server pause cancelled requests` | warn | `pause_for`, `count` | reads still running after 60 s were cut |
| `upload ingest session not closed` | warn | `pause_for`, `error` | the store session could not be closed before the pause; the server also logs it, without `pause_for`, when it closes the database |
| `server paused` | info | `pause_for`, `gen`, `wait_ms`, `uploads_cut`, `requests_cut` | the database is closed, and the copy starts |
| `server pause given up` | error | `pause_for`, `stage`, `error` | the pause ended before the database was closed (`stage`: `uploads`, `requests`, `quiesced` or `suspend`); the server goes on as before |
| `export written` | info | `export_id`, `bearer`, `bytes`, `sha256`, `content_sha256`, `files`, `signed_by`, `key_id`, `duration_ms`, `expires_at`, `notices`, `warnings` | the package is ready to download |
| `export failed` | warn, or error for an internal failure | `export_id`, `bearer`, `error`, `retryable`, `duration_ms` | the export ended without a package |
| `export cancelled` | info | `export_id`, `bearer`, `stage` | a running export was cancelled (`stage`: `pausing` or `packaging`) |
| `server resume failed` | warn | `pause_for`, `attempt`, `error` | one try to reopen the database failed |
| `server resume given up` | error | `pause_for`, `error` | every try failed; the server stops with that error |
| `server resumed` | info | `pause_for`, `gen`, `paused_ms`, `attempts` | the database is open again |
| `export downloaded` | info | `export_id`, `request_id`, `bearer`, `range` | a `GET` of the package ended; `range` says whether it asked for part |
| `export removed` | info | `export_id`, `reason` | its files were removed: `expired`, `cancelled`, `left by an earlier run`, `orphan` or `unreadable sidecar` |
| `export of another token` | warn | `request_id`, `export_id`, `bearer` | a token asked for an export it did not start; it was answered `404` |
| `export file not removed` | warn | `export_id`, `file`, `error` | a file could not be removed; the next round, or the next start, tries again |
| `export sidecar not valid`, `export not read` | warn | `export_id`, `error` | at start, an export's files did not match; they are removed |
| `export not opened`, `export not closed` | error, warn | `export_id`, `error`, and `request_id` when not opened | the package could not be opened for a download, or closed after it |
| `export folder not unlocked` | warn | `error` | an export folder outside the data root could not be unlocked at the stop |

`export written` holds the notices and warnings of the package writer, such
as why a package is unsigned; the status a client reads does not, as they may
name server paths. `server resume given up` is the record to alert on: it
stops the server. `server pause given up` also follows a cancel during the
pause, so read it beside the `export cancelled` or `export failed` record of
the same export.

## Read-only media

Read commands open the database read-only and never write to it. They work on
write-protected media, and on a data root the user may only read:
- The root lock is skipped when the root cannot be written; DUES notes this.
- badger always opens its `DISCARD` file for writing, even for a read-only
  open, and ends the process when it cannot. On such media DUES therefore
  opens badger from a temporary directory. That directory holds links to the
  database files (copies where links are refused) and badger's own `DISCARD`.
  DUES says how much it copied, and removes the directory when the command
  ends.
- The graph store opens without a process lock, because no writer can exist
  there.

Write commands on read-only media fail with an error saying the media is
read-only, and change nothing.

## Graph compaction

The graph store is an image and a log. Every open loads the image and replays
the log written since, so enrichment and micro extraction make each open
slower and need more memory, until a compaction folds the log into a new
image. An open warns when the log passes 1 GiB or 5 million records.

A command that wrote the graph (store, reindex, enrich, micro extract,
repair) compacts it as it ends when the log has passed either size, as
`dues graph compact` does, on the command's progress line:
- A compaction that is refused or fails prints a warning. The command still
  succeeds, and the store opens as it did.
- Ctrl-C skips it, or stops it and leaves the store as it was.
- `--graph-compact never` (config key `graph-compact`) turns it off.
- The TUI and `dues server` never compact. Stop the server and run
  `dues graph compact`.

`dues graph compact` compacts the graph:
- It holds the database writable, so no other DUES command writes meanwhile.
  Stop `dues server` first: it holds the database open.
- It refuses to start, and changes nothing, when the volume cannot take the
  new image beside the old one and the log (plus 64 MiB), or when the machine
  has less free memory than twice what the graph holds now.
- Node and edge IDs do not change.
- The log is kept as `graphene.NNNNNN.wal`, with each commit's actor, time
  and signature, which the image does not hold. Each segment records the
  hash of the one before it.
- Ctrl-C while the image is built leaves the store as it was. A crash at any
  point leaves a store that opens; a half-written image (`graphene.csr.tmp`)
  is ignored and replaced by the next compaction.

`dues graph status` describes the store from its files, replaying nothing and
writing nothing: the schema, the image and its node and edge counts, the
log's records and commits and how many are signed, and the segments. It fails
when the segments' hash chain is broken, or when the store was compacted and
its image is gone.

A store whose image is gone is refused by every open: its log holds only what
came after the compaction, and replaying it alone would hand out node IDs
again. The graph is derived from the database, so move `graph/` aside and run
`dues enrich` or `dues micro extract` to build it again.

Every new graph store gets an empty image when it is made. DUES v0.39 and
older link a graphene that reads this release's log as an empty graph and
appends to it; they refuse the image instead, so they fail loudly rather than
misread the store.

To back up the graph, copy `graph/` whole while no DUES command runs, or use
`dues package export`. The image, the log and the segments belong together.

## Keys and signing

Every graph commit is signed with Ed25519, and every open checks the
signatures of the log it replays.

A database signs as its own actor, `database`:
- Its key is made the first time the database's graph is written, or when
  the database is exported.
- In an encrypted database, the database password protects the key. A
  command that writes the graph needs the password anyway.
- In an unencrypted database, the key is stored unprotected, with mode 0600
  on Linux and macOS; on Windows it gets the folder's ACLs. Whoever can read
  the data root can sign as the database.

An examiner signs as an actor of their own:
- `dues actor create NAME` makes one, with a key its passphrase protects
  (Argon2id and AES-256-GCM). The actor's ID derives from its name, so every
  workstation agrees on it.
- `--actor NAME` with `--actor-key-password-file FILE` or
  `--actor-key-password-stdin` signs a command's graph commits as that actor.
  The config keys are `actor` and `actor-key-password-file`.
- `--password-stdin` and `--actor-key-password-stdin` cannot both be given.

`dues actor list` lists every key of the keyring. `dues actor rotate NAME`
gives an actor a new key and keeps the old one, so what it signed still
verifies. `dues actor revoke KEY-ID` stops a key from signing; what it signed
before still verifies. Create, rotate and revoke hold the database writable.

The keys live in `dues.keys/`:
- `keyring.json`: every actor's public keys, and which are revoked;
- `actors.json`: the actors' names and IDs;
- `actors/*.key`: the private keys.

An open checks each commit against the keyring:
- A signature that does not verify fails the open.
- A commit signed by a key the keyring does not hold is let through, with a
  warning that counts them.
- After `dues graph compact --require-signed`, every open also refuses an
  unsigned commit, a record outside any commit, a commit by an unknown key,
  and a writable open with no actor to sign.

A signature is only as strong as the custody of the data root. Whoever holds
the root holds its private keys, and can rewrite the graph and sign it again.
To check signatures against something the root cannot change, export a
public key with `dues actor export-public NAME --out FILE` and keep the file
elsewhere. `dues package verify --trusted-key FILE` checks a package against
it (see [Packages](#packages)).

Back up `dues.keys/` with the rest of the data root. Without its key file,
an actor cannot sign until it is rotated; what it signed still verifies from
the keyring. The database actor gets a new key by itself the next time its
graph is written.

## Disk space

DUES keeps 128 MiB free on the volume that holds the database:
- A writable open refuses to start with less.
- An ingest checks the free space every 16 MiB written or 250 ms, and stops
  before it goes below the reserve.

The error names the volume, what is free and why DUES keeps the reserve.

The reserve exists because badger writes its tables through memory-mapped
files. On Linux, when the disk cannot back a mapped page, the kernel kills the
process (SIGBUS), and no error handling can run. A compaction of a very large
database can write more than the reserve, and another program can fill the
disk meanwhile, so keep well more than 128 MiB free.

`dues graph compact` checks the volume itself before it starts (see Graph
compaction).

A server export needs the package's size plus 128 MiB free on the export
folder's volume: `start` answers `507` otherwise, and the export checks the
exact size again once the database is paused. Reopening the database after
it needs 128 MiB free on the database's volume; the server tries 4 times in
about 21 s, then stops with an error. While an export writes into the data
root's `exports/`, an upload `start` also counts the bytes the export has
still to write.

A store that runs out of space fails and leaves the database whole. Earlier
evidence still restores, and storing the file again after making space resumes
the attempt.

## Packages

`dues package export` copies a data root into a package: a `.duesarc` file
that holds the root's files as they are on disk, under `db/`, and a
`manifest.json` listing each file with its size and SHA-256.
- It takes the root lock shared for the whole copy. Commands that read run
  beside it; commands that write are refused until it ends, and it refuses to
  start while one runs.
- It refuses a database that was not closed cleanly (run `dues repair --fix`)
  and a database made before manifests existed.
- It leaves out the locks, temporary files a killed writer left, a graph
  image a compaction is still writing (`graph/*.tmp`), `uploads/` and
  `exports/`. `--without-derived` leaves out `fts/` and `graph/` too: `reindex
  --fts`, `enrich` and `micro extract` rebuild them.
- The package may not go inside the data root, except into its `exports/`
  folder, which must exist and be named exactly so (lowercase, not a link).
- An encrypted database is copied as its encrypted files, so the package
  needs no password. With one (`--password-file`), it is checked first. Its
  full-text index and graph are not encrypted, on disk or in the package:
  export warns, and the manifest names them in `plaintext_components`.
  `manifest.json` itself is readable by anyone; for an encrypted database it
  gives no evidence counts.
- `--split-size-mb N` writes parts of N MiB and a `.split.json` sidecar with
  each part's SHA-256, as the compression tool does. A split export needs
  twice the package's size free while it writes; export checks the space
  first.
- `--host-info` records the machine's OS, architecture and name. Without it
  the package says nothing of where it was made.
- The keyring of public keys always goes into the package, also with
  `--without-derived`. Private keys never do.

The package prints a content hash: the SHA-256 of the manifest's file list.
It leaves out times and the host, so two packages of the same database state
have the same content hash, which can go in case notes.

The database actor signs the package (see [Keys and signing](#keys-and-signing)):
- The signature covers the content hash, the creation time and what the
  manifest says of the database.
- Export makes the database actor's key when the database has none.
- An encrypted database's key opens only with its password. Exported without
  it, the package is unsigned, and export says so.
- `--actor NAME`, with its passphrase, signs as that actor instead. This
  needs no database password. A named actor that cannot sign fails the
  export.

`dues package inspect PACKAGE` describes a package from its manifest alone.
`dues package verify PACKAGE` reads all of it: a split package's parts against
their sidecar, then every file against its size and SHA-256, and that nothing
is missing or added. A graph whose marker records a compacted image must
include the image (`graph/graphene.csr`). Each problem is named, and any fails
the command (exit 1).

Verify also checks a signed package's signature:
- The signature must verify, and the packaged keyring must hold its key,
  not revoked.
- The signing key travels in the package. A signature alone therefore shows
  only that nothing changed after signing: whoever changed the package could
  sign it again.
- `--trusted-key FILE` requires a signature by that public key, written by
  `dues actor export-public`. Without it, verify says the signature was not
  checked against a trusted key.
- A package signed with an unprotected key gets a warning: whoever could
  read its data root could sign as it.
- A package that holds a private key fails.

`--deep` also checks the database the package holds, as `dues repair
--verify` checks a data root:
- The files are copied into `dues-verify-*` in `--temp-dir` (default: the
  system's temporary directory), each checked against the manifest as it is
  written. This needs the package's size free there.
- The copy is opened read-only and its evidence counted against the manifest.
  Then every chunk of every evidence is read back against its hash. Each
  evidence gets an `OK` or `DAMAGED` line on stderr, and any damaged one fails
  the command (exit 1).
- An encrypted database needs its password (`--password-file` or
  `--password-stdin`).
- The copy is removed afterwards, also on failure or interruption. A copy
  left by a killed verify is removed by the next deep verify.

A package can match its manifest and still hold damaged evidence: the
manifest records the files as they were at export. Only `--deep` reads the
evidence.

`dues package import PACKAGE --dbpath TARGET` makes a new database from a
package:
- TARGET must be missing or an empty folder. To add a package's evidence to
  a database that exists, use `dues package merge`.
- A package from a newer DUES (a later manifest version, database format or
  graph schema) is refused before anything is written.
- The files are written into `.<target>.dues-import-*` beside TARGET, each
  checked against its size and SHA-256 as it is written and synced. The
  database is then opened read-only, and its evidence counted against the
  manifest. Only then is the folder renamed to TARGET.
- An encrypted database is opened only with its password (`--password-file`);
  without it, its files are checked and it is not opened.
- A signed package's signature is checked as verify checks it.
  `--trusted-key FILE` imports only a package that key signed.
- The new database's actor gets a key of its own the first time its graph is
  written. The source's public keys stay in the keyring, so what they signed
  still verifies.
- A failure or an interruption removes the folder and leaves no TARGET. A
  folder left by a killed import is removed by the next import of the same
  TARGET.

`dues package merge PACKAGE --dbpath TARGET` copies the evidence of a
package into a database, which is made, with the layout flags given, when it
is missing. Use it to bring a case exported elsewhere into one you work on;
use import to restore a database as it was.
- The package's listing and signature are checked first; `--trusted-key FILE`
  merges only a package that key signed. Without it, a signature says only
  that the package is self-consistent: anyone can make one that is. The
  records inside a package are not trusted either: every merged evidence is
  checked against the hash its record holds before it is committed.
- TARGET is then opened for writing, with recovery, and stays locked until
  the merge ends: no server or store can use it meanwhile.
- The package's files, except its full-text index and graph, are copied to
  `.<target>.dues-merge-<16 hex digits of its content hash>` beside TARGET
  (`--stage-dir DIR` puts it elsewhere, never inside TARGET), each checked
  against the manifest as it is written. The full-text index and graph are
  only checked: TARGET builds its own. A package that does not match its
  manifest is refused before any evidence is stored. Plan on free space for
  the copy and about as much again for what TARGET stores.
- The copy is opened read-only (an encrypted one with `--source-password-file`
  or `--source-password-stdin`). Each evidence is read from it, checked
  against its chunk hashes and its recorded content hash, stored again in
  TARGET's chunk size, hash and encryption, indexed, and committed with where
  it came from: the package's content hash, the source evidence ID, and who
  signed the package. `dues list` shows it as `merged`. Its full-text index
  and graph are built when the source had them, and the graph commits are
  signed by TARGET's actor. An E01 is checked again against its stored MD5 and
  SHA-1; `--require-verified` fails one that does not match.
- Content TARGET already holds becomes an alias of the evidence holding it.
  The source's aliases come along, marked as merged; local aliases are never
  replaced. A source evidence keeps its ID when TARGET does not use it.
- An encrypted package goes into an unencrypted TARGET only with
  `--allow-plaintext`. Private keys never come with a package.
- `--store-jobs N` merges N evidence at once, as for a folder store.
- stdout gets one line per evidence, in the package's order (`merged`,
  `aliased`, `present`, `skipped` with its reason, or `failed` with the
  error), then a summary. Exit 1 when an evidence failed or the package is
  damaged or untrusted; 2 on a stored digest mismatch or an index that did
  not finish (run `dues reindex`).
- A rerun of the same package resumes. Evidence already merged is `present`,
  and the copy is checked file by file and only what differs is copied again.
  The copy is removed when every evidence is resolved, or failed in a way a
  rerun would repeat (damaged bytes, a content hash that does not match). An
  interruption, or a failure a rerun may not repeat, keeps it, with a notice.
- A folder at the copy's name that a merge of this package did not make (no
  marker naming it, a link, not yours, or, on Linux and macOS, not mode 0700)
  is refused and left alone. Whoever can write beside TARGET can change the
  copy between runs; the rerun's check finds what they changed.
- On Windows the copy takes its ACL from the folder it is in, as the
  database does. Keep it in a folder only you can write.
- Restoring merged evidence by hash takes TARGET's hash of it, which
  `dues list` shows, not the source's.

All five take the package, any of its parts, or its sidecar. The format is
in `docs/COMPRESSION_ABI_REFERENCE.md` (Database packages).

**Server exports.** A token with the `export` scope can have `dues server`
write a package (`docs/SERVER_API.md`, Exports):
- It is the format `dues package export` writes, never split, and verified
  and imported the same way. The server pauses the database while it copies
  it, instead of taking the root lock shared.
- It is signed by the actor the server's database was opened with: `--actor`
  when `dues server` was started with one, else the database actor.
- It is written to `--export-dir` (default `exports/` in the data root) as
  `<export_id>.duesarc`, beside `<export_id>.json`, which records the token
  label that made it, the database, its hashes and when it expires.
- It is kept `--export-keep-hours` (default 24), and at most 4 at once.
- A folder outside the data root is used by one server at a time (it holds
  `.dues-exports.lock` there). On Linux and macOS it must belong to you or
  root, and neither its group nor others may write it. On Windows the server
  does not check its ACLs: give only your account access to it. A link into
  the data root, and a folder inside another database's data root, are
  refused.
- The package and its JSON file are written readable by the server's account
  only (mode 0600 on Linux and macOS).

## Resetting a database

`dues reset` deletes the database in the data root and nothing else there:
- It asks you to type the data root's folder name. `--yes` skips that, and is
  required when standard input is not a terminal.
- It refuses a folder that holds no DUES database, a filesystem root, and any
  folder that is or contains your home folder or the working folder.
- It holds the root lock throughout, so it cannot run while another DUES
  process uses the database.

The records go first and the manifest last, so a reset that is interrupted
leaves a database without records, or a manifest alone. Running reset again
finishes it.

Reset keeps `dues.keys/`: keys are custody records, and the keyring lets what
they signed be checked later. It keeps `exports/` too: a package is a copy
made for someone else. A database made again in the root gets a new
database key; the old one stays in the keyring. A server on the new database
leaves the old database's server exports in `exports/` alone: remove them by
hand once they are no longer needed.
