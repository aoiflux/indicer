# DUES TUI: Implementation

The TUI is a Bubble Tea v2 program (`charm.land/bubbletea/v2`, with
`charm.land/bubbles/v2` and `charm.land/lipgloss/v2`). This document is for
developers changing it: how it is wired to the database, how its jobs run and
stop, and the rules its screens follow.

## Wiring

`dues tui` runs `cli.TUICmd` (`cli/cmdtui.go`):

1. Opens the database for writing (`Common`, `dbopen.ReadWrite`), so recovery
   runs and the root lock is held for the session.
2. Silences terminal output once, for the whole session (`console.Quiet`):
   anything the storage code prints would draw over the TUI. It used to be
   silenced per action, which raced when actions ran side by side. It also
   removes any progress reporter from the context (`progress.Without`), so
   actions report no progress.
3. Runs `tui.Run(ctx, tui.Config{Info, Actions, Profile, Theme})` with the
   command's context. `Profile` is the colour depth the command line resolved
   (`--color`, `--plain`, ...; see [CLI_UX.md](CLI_UX.md)) when stdout is a
   terminal, so the TUI follows the same switches. `Theme` is `--theme`.
   `Run` gives bubbletea an empty environment (`tea.WithEnvironment`), as
   it would otherwise read `TERM` and the like itself. With no `TERM`, its
   renderer uses only the sequences every terminal has, as it always did on
   Windows, where `TERM` is not set.
4. When `Run` returns, restores output, compacts the value log if anything
   was stored, and closes the database. If `Run` returned `tui.ErrJobsRunning`,
   it does none of this: the database is left unclosed (and output silenced)
   for the job still running, and badger recovers it at its next open.

The `tui` package never touches the database. `tui.Actions` are functions
`cli` supplies (`tuiActions` in `cmdtui.go`), each taking a context and
returning what it did:

| Action | Implemented with |
|---|---|
| `Store(ctx, path, StoreOptions) (Outcome, error)` | `storeFile`/`storeFolder`, the code `dues store` runs, writing its report to a buffer. `StoreOptions` are `dues store`'s `--no-index`, `--revrel`, `--simhash`, `--fts` and `--enrich` |
| `Search(ctx, query)` | `search.SearchReport`, and the summary lines `dues search` prints |
| `Restore(ctx, id, path, replace)` | `restoreToPath` (verified, atomic). An existing file without `replace` is `tui.ErrTargetExists` |
| `NearIn`, `NearOut` (`ctx, query, NearOptions`) | `near.In`, `near.Out`, and the summary `dues near` prints. `NearOptions` are `--deep` and `--advanced-deep` |
| `Reset(ctx)` | `dbopen.Handle.Reset`, which also clears the graph and the full-text index |
| `List(ctx, limit) (EvidenceList, error)` | Completed evidence records, at most `limit`, and the total |
| `Stats(ctx)` | `store.GatherStats` |
| `Micro(ctx) (MicroView, error)` | The graph opened read-only; `TopFiles` (500 files, 20 artefacts each) |

A read-only graph handle open in the process makes a writable open fail, so
`tuiActions` keeps the two apart (`graphGate`). A store with enrichment waits
for the micro views already reading the graph. While it runs, a micro view is
refused with a message saying so.

An `Outcome` is the action's report, line by line. `Warning` marks what the
command line reports with exit code 2 (a `lifecycle.WarningError`): the action
finished, but needs the examiner's attention.

`tui.Info` describes the database (root, chunk size, chunk hash, encryption).
The TUI shows it and cannot change it: the layout belongs to the database.

## Jobs (`tui/jobs.go`)

Every action and every screen load is a job, run by the `runner`:

- Each job runs on its own goroutine under `internal/safe`, so a panic in it
  becomes an error naming a panic ID.
- Each has its own context, derived from the command's. A job ends when it is
  cancelled (`ctrl+x`: cause `errCanceled`), when the TUI stops (`errStopping`)
  or when the command's context ends (a signal: its cause). A job that fails
  because its context ended reports that cause, not "context canceled".
- A reset (`exclusiveJob`) starts only when no job runs, and no job starts
  while it runs (`busyError`). Other jobs (`sharedJob`, and `loaderJob` for
  screen loads) run side by side.
- `await(job)` is the Bubble Tea command that reports the job's end as a
  `jobDoneMsg`.

### Why `Run` waits

Bubble Tea does not wait for the commands it runs when the program ends: their
goroutines are abandoned, and messages sent to an ended program are dropped.
The old TUI closed the database as soon as the program ended, under any store
still running (TUI-3). `Run` therefore, after the program ends for any reason:

1. closes the runner, so a command Bubble Tea runs late starts nothing
   (`errClosed`);
2. cancels every job (`errStopping`);
3. waits up to `drainTimeout` (30 s) for them.

If a job is still running, `Run` returns `ErrJobsRunning` naming it.

`Run` gives the program `tea.WithContext(ctx)` and `tea.WithoutSignalHandler()`:
a signal cancels the command's context, which ends the program and every job
with the signal as cause, and `Run` returns that cause (exit 130/143).
Bubble Tea's own handler would quit without one.

## Model (`tui/tuiapp.go`)

The root `model` holds the shared `env` (info, actions, runner, terminal
size), the menu, the screen showing (nil on the menu), the last finished
job's status line, and the quit dialog's state.

- **Messages.** A `jobDoneMsg` goes to the screen that started the job, if it
  still shows, and always updates the menu's status line (`summarize`; a
  loader reports only a failure). Leaving a screen never loses a job's outcome.
- **Keys.** `ctrl+c` quits, or, while jobs run, opens the quit dialog with No
  chosen; Yes cancels the jobs and quits when the last one ends; `ctrl+c`
  while stopping quits at once. `ctrl+x` cancels the screen's job, or every
  job on the menu. `esc` closes a screen's own question first (`escaper`),
  else goes back. `q` goes back only when no text field has focus
  (`screen.typing()`); otherwise it is text.
- **Panics.** `Update` and `View` run under `internal/safe`. A panic in
  `Update` records the error, cancels every job and quits; `Run` waits for
  the jobs and returns a `lifecycle.InternalError` (exit 70). A panic in
  `View` is drawn, and the next key quits.

### Screens

Each screen implements `screen`:

```go
type screen interface {
	init() tea.Cmd
	update(msg tea.Msg) tea.Cmd
	view() string
	resize()
	typing() bool // a text field has focus: q is text
	job() int     // the job the screen waits for, or 0
}
```

| File | Screen |
|---|---|
| `tuistore.go` | Path, the store options as checkboxes (full text and enrichment unavailable without the index), Store and Cancel; "store another?", which hides the options |
| `tuilist.go` | Evidence list (`listLimit` 5,000), copy, restore with a replace question |
| `tuisearch.go` | Query; the field always has focus |
| `tuirestore.go` | ID and path, Restore and Cancel; replace question (No chosen); "restore another?" |
| `tuinear.go` | Mode list, input, Deep and Advanced deep checkboxes, Analyse |
| `tuireset.go` | Typed confirmation, No then Yes; Yes is disabled until the folder name is typed and nothing else runs |
| `tuistats.go` | Statistics in a viewport |
| `tuimicro.go` | Micro-artefact tree in a viewport |

Shared pieces: `widgets.go` (the yes/no `prompt`, the `running` job),
`render.go` (status lines, error text, the frame, rows, the list window,
buttons) and `tuistyles.go` (the styles).

## Rules the screens follow

These are the defects the W11 rewrite fixed; keep them fixed.

- **No type assertion on a list item without `ok`** (TUI-4). Lists are built
  with `env.newList`, which turns off filtering, the quit keys and the help.
- **Every string from the evidence goes through `clean`** (`termsafe.Line`)
  before it is drawn: names, paths, artefact values, report lines, error
  messages (TUI-5). `truncate` cuts by runes.
- **Bounded drawing** (TUI-5). The evidence list loads at most 5,000 rows and
  draws a window around the selection (`window`); the micro view reads a
  bounded set (`TopFiles`).
- **No blocking work in `Update`.** The clipboard is written by a command
  (`copyCmd`, `writeClipboard`), not in `Update`. Database work is a job.
- **Destructive actions default to No** (TUI-1). Reset needs the folder name
  typed, and `Enter` in that field moves to No. A restore over an existing
  file asks first (TUI-2).
- **Cancel goes back; it never quits** (TUI-4).
- **Styles follow the terminal's background.** `newStyles(dark)` builds the
  styles for a dark or a light background: the CLI's roles (`internal/ui`)
  in that theme's values, and greys with a value for each. Screens draw
  with `env.st` and never keep a style of their own.
  - `--theme dark` or `light` is used as given.
  - Under `--theme auto` (the default), `Init` asks the terminal for its
    background (OSC 11), and the TUI is dark until it answers. The answer
    (`tea.BackgroundColorMsg`) rebuilds the styles. The menu, and the
    screen showing if it implements `restyler`, take them over: text inputs
    and the statistics, which are drawn ahead. A terminal that never
    answers keeps the dark styles.
  - An input is made with `env.newInput`. Typed text and the prompt keep the
    terminal's own colour, and the cursor is reverse video.
- **No emoji.** Menu entries and titles are words: emoji differ in width
  between fonts and terminals. `☐` and `☑` are text symbols and stay.

## Tests

- `tui/harness_test.go`: a driver that runs the model as the program does
  (each command on a goroutine, its message fed back), with fake actions that
  block until released (`gate`).
- `tui/tui_test.go`: one test per defect above, the quit dialog, outcomes after
  leaving a screen, the reset rules, the layout shown read-only, panics.
- `tui/jobs_test.go`: the runner (reset alone, causes, panics, closing,
  waiting).
- `tui/run_test.go`: `Run` as a program, with keys written to its input: it
  waits for jobs after a signal, reports a stuck job, and quits through the
  dialog.
- `tui/styles_test.go`: every text colour reaches WCAG AA (4.5:1) on what it
  is drawn on, in both themes, in truecolor and at 256 colours; an answer
  from the terminal restyles the menu and an open screen; `--theme` is
  kept; only `auto` asks the terminal; no emoji in the sources.
- `cli/cmdtui_test.go`: the actions on a real database, and that `TUICmd`
  closes the database, or leaves it open to a stuck job (`runTUI` and
  `leftOpen` seams).
