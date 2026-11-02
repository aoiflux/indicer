# DUES TUI: Quick Start

The TUI is a full-screen, keyboard-driven way to work with one DUES database
in one session: store evidence, list it, search it, restore from it, run NeAR,
look at statistics and micro-artefacts, and reset it. It runs the same code as
the command line, so what it stores, finds and restores is what `dues store`,
`dues search` and `dues restore` would. For scripts and batch work, use the
command line.

## Launch

```bash
dues tui
dues tui --dbpath ./caseA
dues tui --dbpath ./caseA --password-file ./caseA.key
```

The TUI opens the database for writing and holds its lock until it ends, so
no other DUES process can write to it meanwhile. The password comes from
`--password-file` or `--password-stdin`, as for every command.

The chunk size and chunk hash are part of the database's layout, fixed when it
was created. The menu shows them; the TUI cannot change them. A database
without a password is not encrypted, and the menu says so.

The TUI asks the terminal for its background colour as it starts, and uses
light colours on a light background, dark ones otherwise. A terminal that
does not answer gets the dark colours; `--theme light` or `--theme dark`
chooses for it. `--color` and `--plain` apply as on the command line.

## Keys

| Key | What it does |
|---|---|
| `↑`/`↓`, `Tab`/`Shift+Tab` | Move between items and fields |
| `Enter` | Select, confirm, or press the focused button |
| `Space` | Toggle the focused checkbox |
| `esc` | Close a question, or go back to the menu |
| `q` | Go back to the menu, or quit from the menu. In a text field, `q` is typed |
| `ctrl+x` | Cancel the screen's job; on the menu, cancel every job |
| `ctrl+c` | Quit. While jobs run, DUES asks first |

## Jobs

Storing, searching, restoring, NeAR and the loading of the list, statistics and
micro-artefact screens run as jobs behind the screen. Leaving a screen with
`esc` does not stop its job: the menu lists the jobs still running, and shows
the last one to finish, with a warning when it needs your attention. Several
jobs can run at once; a reset runs only when nothing else does, and nothing
starts while it runs.

`ctrl+x` cancels a job. A cancelled store leaves an unfinished attempt, which
storing the same file again resumes.

Quitting while jobs run asks first, with **No** chosen. **Yes** cancels the jobs
and quits once they have stopped. A second `ctrl+c` while stopping ends the
program at once, but DUES still waits up to 30 seconds for the jobs before it
closes the database. If a job has still not stopped, DUES leaves the database
unclosed, says so, and exits with an error; badger recovers the database the
next time it is opened. A signal (SIGINT, SIGTERM) stops the jobs the same way,
and DUES exits with code 130 or 143.

## Screens

### Store

Enter the path of a file or a folder and press `Enter` (or the **Store**
button). Evidence is stored as `dues store` stores it with its default
options: containers (EWF, VHD/VHDX, split raw) decoded and the content
indexed. The **Skip the index** checkbox stores the bytes only. A file whose
content is already stored is recorded as an alias of the evidence holding it.

The screen shows the store's report: the evidence ID, the index state, and any
warning (for example EWF digests that do not match). **Cancel** goes back to
the menu.

The TUI does not record reverse relations, the full-text index or graph
enrichment. NeAR needs reverse relations: `dues store --revrel` records them,
and `dues repair --fix --revrel` adds them to evidence already stored.

### List

Lists the stored evidence, at most the first 5,000; the screen says when
there are more, and `dues list` lists them all.

- `↑`/`↓` (`k`/`j`), `PgUp`/`PgDn`, `Home`/`End` (`g`/`G`): move.
- `c`: copy the selected evidence's ID to the clipboard.
- `Enter` or `r`: restore it to `restored_<ID>.bin` in the working directory.
  An existing file is replaced only after you agree.

### Search

Type the query and press `Enter`. The search covers every stored object, as
`dues search` does, and replaces `report.json` in the working directory. The
screen shows how many objects match, what was searched, and any object that
could not be read.

### Restore

Enter the ID or hash of a stored object and the path to restore to (default
`restored`). Every chunk, and the whole content, is checked against its hash.
An existing file is replaced only after you agree (**No** is chosen); a restore
that fails leaves the file as it was.

### NeAR

Choose a stored object or a file outside the database, enter its ID or path,
and press **Analyse**. For a stored object, the **Deep** checkbox also matches
similar chunks. The report replaces `near_report.json` in the working
directory.

### Reset

Deletes everything in the database: stored evidence, indexes, the graph and
the full-text index. Type the database folder's name to confirm, then press
**Yes, delete everything**. **No** is chosen, and `Enter` in the name field
never resets. A reset cannot start while another job runs.

### Statistics

Counts of evidence, chunks, partitions and indexed files, the chunk layout,
and the space used on disk. The chunks shared by objects are counted from
reverse relations, so evidence stored without them is not counted there.

### Micro-artefacts

The files with the most micro-artefacts (at most 500, and 20 artefacts each),
under their evidence and partition, with the totals for every file. Run
`dues micro extract` first; it also writes the full HTML view.

## Things to know

- Everything the TUI shows from the evidence (names, paths, artefact values,
  error messages) is made safe to draw, so a crafted name cannot control the
  terminal.
- Search and NeAR write their reports to the working directory, replacing the
  previous ones.
