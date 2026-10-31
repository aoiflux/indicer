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
| `graph/` | The graph store for enrichment and micro-artefacts, when either has run |
| `fts/` | The full-text index, when one was built |
| `uploads/` | Part files of server uploads in progress |
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
moved or copied as a whole. Copy it only while no DUES process uses it.

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
3. the `DUES_PASSWORD` environment variable;
4. `--password` (warns, because other users can see the command line).

An empty password means no password.

## Concurrent use

The root lock serialises access between DUES processes:
- Commands that only read take it shared, so any number can run at once:
  `list`, `stats`, `search`, `near`, `restore`, `micro list`, and `repair`
  without `--fix` or `--upgrade`.
- Commands that write take it exclusively: `store`, `repair --fix`,
  `repair --upgrade`, `reindex`, `enrich`, `micro extract`, `server`, `tui` and
  `reset`.

A command that cannot take the lock fails at once with
`database is in use by another DUES process (<root>)`. It does not wait.

Inside one process, the server and the TUI run their own work side by side. A
reset in the TUI is refused while other jobs run.

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

A restore, a report or an archive that is interrupted leaves nothing at its
target. Output is written to a temporary file beside the target and renamed
into place only at the end. The next write to the same path removes any
temporary file an interrupted one left.

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
progress is one plain line when a phase starts, one when it ends, and one
every 15 seconds. Under CI (`CI`, `GITHUB_ACTIONS`, `GITLAB_CI`, `BUILDKITE`,
`JENKINS_URL`, `TF_BUILD`, `TEAMCITY_VERSION`, or `--ci`) DUES behaves as on a
pipe, and prompts refuse.

| Variable | Effect |
|---|---|
| `DUES_COLOR` | `auto`, `always`, `never` or `ansi`, as `--color` |
| `DUES_PLAIN=1` | as `--plain` |
| `DUES_NO_FUN=1` | as `--no-fun` |
| `DUES_THEME` | `dark` or `light`: the terminal's background |
| `NO_COLOR` | any value: no colour |
| `FORCE_COLOR`, `CLICOLOR_FORCE` | colour even on a pipe |
| `TERM=dumb` | no colour, no redraw, ASCII |

[CLI_UX.md](CLI_UX.md) has the rest: palette, marks, the progress display and
the accessibility review.

## Logs and crash files

DUES writes its log to the first directory it can write in, in this order:
1. `--log-dir`;
2. `DUES_LOG_DIR`;
3. the per-user cache directory: `%LocalAppData%\dues\logs` on Windows,
   `~/.cache/dues/logs` on Linux, `~/Library/Caches/dues/logs` on macOS;
4. `dues-<user>/logs` in the temporary directory.

If none is writable, DUES prints one warning and runs without a log. A logging
failure never stops a command.

Each process writes its own files, named
`<component>-<UTC time>-<pid>.ndjson`. The component is `dues` for the CLI
and `qwikpak` for the compression command. Each line is one JSON object with
`run_id`, `pid` and `component`. A file rotates at 50 MB, and the newest 10
files are kept per directory. Set `DUES_DEBUG=1` for debug-level entries.

Values of fields whose names mention a password, secret, token, credential
or authorization, or end in `key`, are replaced with `[REDACTED]`. So are the values of
`--password` and `--token` on a logged command line.

Each run also creates `crash-<run id>.txt` in the log directory and removes it
at a clean exit if nothing was written to it. The file receives:
- the report of a panic DUES recovered (exit 70);
- the runtime's trace of a crash nothing can recover from: a concurrent map
  write, a stack overflow, a panic on an unguarded goroutine.

After a crash, DUES prints the crash file's path and the run ID, which matches
the `run_id` of the log entries.

The compression library logs only when its host sets `DUES_LOG_DIR`.
Otherwise it writes warnings and errors to the cache directory, in a file
created at the first such entry.

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

A store that runs out of space fails and leaves the database whole. Earlier
evidence still restores, and storing the file again after making space resumes
the attempt.

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
