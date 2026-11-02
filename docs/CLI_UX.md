# DUES in the terminal

How DUES looks and behaves in a terminal: what goes to stdout and stderr, the
colours and marks, the progress display, the switches that turn decoration
off, and the accessibility review behind them. The code is `internal/ui`
(presentation) and `internal/progress` (what long operations report), on top
of `internal/console` (the only writer to the terminal).

## Output contract

1. **Results on stdout, everything else on stderr.** Results are JSON
   (`list`, `micro list`, and one document from `repair`), reports (`store`, `restore`, `reindex`, `stats`),
   help and version text, including the identity line `dues --version`
   prints. The run header, progress, notes, warnings, errors and the
   occasional extra go to stderr. `dues list | jq` reads pure JSON.
2. **Piped output is the contract.** On a terminal stdout differs from piped
   stdout only by colour codes; marks, art and layout appear on stderr, or in
   help and version on a terminal. Nothing on a pipe holds an escape code
   unless `--color=always` asks for colour.
3. **Colour is added where text is written, never stored in a report.**
4. **Text from evidence is sanitised before it is styled.** File names,
   partition labels and error text pass through `internal/termsafe`, so a
   hostile name cannot move the cursor or change the terminal's title.
5. **No terminal queries from the command line.** It never asks the
   terminal for its background colour or capabilities: such queries are
   slow over SSH and leave stray bytes in the shell. It reads its flags and
   what the operating system says about each stream (a terminal or not, and
   its width); it reads no environment variable. The TUI is the exception:
   it owns the screen and reads the keyboard, and under `--theme auto` it
   asks once for the background (OSC 11), taking the answer as it takes a
   key.
6. **Extras never touch evidence processing, output files, logs, stdout or
   exit codes.**

Messages on stderr read `dues: warning: ...` on a pipe, the form scripts
match, and `▲ warning: ...` on a terminal. A command that finished with a
warning (exit 2) says `warning`, not `error`.

## Switches

| Flag | Effect |
|---|---|
| `--color auto\|always\|never\|ansi\|truecolor` | Colour policy for stdout and stderr. `auto` gives 256 colours on a terminal and none on a pipe; `always` colours a pipe too; `ansi` keeps colour but only the terminal theme's 16 colours; `truecolor` uses 24-bit colour. |
| `--plain` | Keep the terminal, drop decoration: no colour or escape codes at all, ASCII only, no redraw, no spinner, no art, no extras. The mode for screen readers, reduced motion, copying logs, and terminals without Unicode or colour. |
| `--ci` | Behave as if no terminal is attached: plain output on every stream, and prompts refuse as they do on a pipe. For CI logs. |
| `--theme auto\|dark\|light` | The terminal's background, for the tuned palettes. On the command line `auto` means unknown; the TUI asks the terminal, and is dark until it answers. |
| `--taskbar` | Progress on the terminal's tab and taskbar (OSC 9;4). |
| `--no-fun` | No extras; everything else unchanged. |

DUES reads no environment variable, so the standard ones (`NO_COLOR`,
`FORCE_COLOR`, `CLICOLOR_FORCE`, `TERM`, `CI` and the like) change nothing;
the flags above cover what they did. A build for an organisation that wants
no extras sets `-ldflags "-X indicer/internal/ui.funDefault=off"`.

## Colour

Colour carries meaning, and never alone: every state also has a mark and a
word. Colour goes on marks, labels and short tokens (names, IDs, hashes,
counts); sentences stay in the terminal's own foreground.

| Role | Meaning | Dark background | Light background | Unknown background | 16-colour |
|---|---|---|---|---|---|
| info | notices, hints | `#6CA8FF` | `#1F5FBF` | `#2D7BDB` | 94 |
| success | done, verified | `#4FC47F` | `#17702F` | `#298D4A` | 32 |
| warning | needs attention, exit 2 | `#E8C547` | `#7A5E00` | `#967904` | 33 |
| error | failed, mismatch | `#FF6B6B` | `#B8222A` | `#DD4640` | 91 |
| highlight | text to type, active step | `#4CCFD6` | `#006A73` | `#158995` | 36 |
| forensics | domain metadata, brand | `#B69CFF` | `#6B3FC0` | `#8F65D8` | 95 |
| evidence | evidence identity: names, IDs, hashes | `#F0A93A` | `#8A5300` | `#C56C05` | 93 |
| muted | units, separators, timings | `#9DA3AD` | `#575E68` | terminal colour | 90 |

- The background comes from `--theme`. Without it, it is unknown, which is
  the common case: macOS Terminal and xterm start light, Windows Terminal and
  VS Code dark.
- `internal/ui/palette_test.go` checks the contrast of every value, and of the
  nearest xterm-256 colour a 256-colour terminal shows instead:
  - the dark and light palettes reach WCAG AA (4.5:1) on six dark and five light
    reference backgrounds;
  - the unknown-background palette reaches 3:1 on all eleven. It is used bold,
    on marks and short tokens.
- Muted text keeps the terminal's own colour when the background is unknown.
- **Faint** is only for the progress display's passing details: the step
  rows under the overall row, the `next:` row and a bar's empty track.
  - On a known background it is the muted colour. On an unknown one it is
    SGR 2 (faint), which dims the terminal's own colour, so it is never
    invisible.
  - A terminal that ignores SGR 2 shows normal text. `--color=never` keeps
    it, as it keeps bold.
  - Results, warnings, errors, the overall row, the final rows and the closing
    line are never faint.

## Marks

| Set | Used on | Marks | Spinner | Bar |
|---|---|---|---|---|
| Full | every terminal, unless `--plain` | `✓ ✗ ▲ ● ■ · – ◆` | braille | `━╸─` |
| ASCII | `--plain` | `+ x ! * # . -` | `\|/-\` | `#>-` |

The Windows console host, a terminal whose font lacks these characters, a
non-UTF-8 locale and East Asian terminals that draw box characters two
columns wide need `--plain`.

No emoji anywhere. Every mark is one column wide.

## The banner

Bare `dues`, `dues help` and `dues version` open with the banner when stdout
is a terminal:

```
●─●─●  ┌─╮ ╷ ╷ ┌─╴ ╭─╴  Deduplicated Unified Evidence Store
 ╲│╱   │ │ │ │ ├─  ╰─╮  v0.39.0 "<pineapple> spacebar"
  ●    └─╯ ╰─╯ └─╴ ╶─╯  74431db · windows/amd64 · go1.27.3
```

- The emblem is a relationship graph: three nodes joined to one.
- The ASCII set draws both the emblem and the wordmark in ASCII (`o-o-o`).
- The build line shows the commit, the platform and the Go version as far as
  they fit; a `-dirty` commit leaves the Go version out.
- From 61 columns all of it shows, from 23 the emblem and wordmark, below that
  the wordmark alone.

Long operations open with a run header on stderr instead: what runs, on
what, into which database, and the settings in effect. On a narrow terminal
it breaks at spaces.

Where stdout is not a terminal there is no art. DUES names itself in one
line, the identity line, with the commit as semver build metadata:

```
DUES 0.39.0+74431db (<pineapple> spacebar) - Deduplicated Unified Evidence Store
```

Piped help opens with it, piped `dues version` prints it alone, and
`dues --version` (or `-v`) prints it anywhere, on stdout. Under `--plain` on a
terminal, `dues version` keeps its overview under that line.

## Progress

Long operations (store, restore, reindex, enrich, NeAR, micro extract,
search, and repair with `--verify`, `--upgrade` or `--fix`) show their
progress.

### On a terminal

A live display of at most seven lines, redrawn ten times a second: one
overall row for the whole command and, under it, a faint row for each step,
as docker draws its layers.

```
⠹ ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━╸──────────  81%  step 2/2  2.61/4.20 GiB  148 MiB/s  ETA ~11s
  ✓ identify     4.20 GiB in 4.1s · 1.02 GiB/s
  ⠼ storing      62% · new 1.80 GiB · 31% dedup · encrypted
  ⠼ indexing     121,804 files · 3,120 files/s · partition 2, ntfs · …\config\SOFTWARE
  · next: full-text · graph
```

- **The overall row** measures the whole command:
  - Work that counts bytes (identify, store, entropy, partition hashes) is one
    fraction of all its bytes, steps still to come included, so the % never
    goes back when one step ends and the next begins. `step 2/2` says which
    step runs; the amounts and the rate are that step's.
  - A folder, or several evidence at once: items done plus the running item's
    share (`12/340 evidence`).
  - One step of known size (full-text, graph): its own fraction.
  - Work of unknown size (the index's tail, compact): a sweep, a count and the
    time it has run, with no %.
- **The ETA** covers the rest of the command. It shows once the last work step
  runs: identify hashes several times faster than store, so an estimate made
  during identify would run short. Until then the step's own row says how
  long it has left (`11s left`). `~` marks an ETA that other running work may
  move.
- **Step rows** read as what they do (`storing`, `indexing`) and keep their
  names once ended. They show the step's %, the new bytes and how much was
  stored already (`31% dedup`), whether the chunks are encrypted, the
  partition, and the file being read, cut from the left so its name stays.
  Steps still to come fold into one `next:` row; when rows do not fit,
  finished ones go first.
- A step that ends OK shows its `✓` in highlight for 300 ms, then in green.
- Lines other code prints go above the display, and a prompt pauses it.
- When the command ends, the scrollback keeps one row per top-level step, by
  name and at full contrast, the steps that never ran on one line
  (`– not run: index, full-text`), and the closing line.
- With `--taskbar`, Windows Terminal and ConEmu also show the progress on the
  tab and in the taskbar (OSC 9;4):
  - after a second, so short commands never flicker it, and at most twice a
    second;
  - paused while a prompt waits or after Ctrl-C, red after a failure;
  - cleared on every way out, a second Ctrl-C and a panic included.
  Leave it off under tmux or screen, which pass it to the terminal they run
  in, and in terminals that read OSC 9 as a notification (iTerm2).

### On a pipe, under `--plain` or `--ci`

Plain lines: one when a phase starts, one when it ends, and, while the
command runs, one heartbeat line. It reports the step in focus, how far the
whole command is when it has several work steps or items, and what runs
beside it.

```
dues: store: start, 4.20 GiB
dues: store: 62%, 2.61 of 4.20 GiB, 148 MiB/s, eta 11s, overall 81% (step 2 of 2); index 121,804 files
dues: store: done, 4.20 GiB in 38.2s, 112 MiB/s
dues: done in 38.2s, read 4.20 GiB, new 2.88 GiB, deduplicated 1.32 GiB (31%)
```

The heartbeat comes every 15 seconds at first, then every tenth of the time
the command has run, never more than 5 minutes apart: a two-hour store prints
about 55, and a CI log never goes silent for long.

### Elsewhere

In the TUI, the server, the FFI library and the compression product: nothing.
`internal/progress` measures the command as a whole (`Tracker`) for any
renderer, so a later TUI can show the same overall row.

### Endings, rates and units

The closing line says how the command ended and what it read, stored new and
found already stored. "new" counts raw bytes before compression, not size on
disk.

A bar shows 100% only for work that finished. A phase that failed or was
interrupted shows where it stopped, and an interrupted store says how to
finish it. A second Ctrl-C ends DUES at once: the display is closed, the
cursor is shown again, and the next command that opens the database for
writing recovers what was left.

Rates are a five-second weighted average. The ETA appears after two seconds
and 1% done. Sizes use IEC units (KiB, MiB, GiB); exact byte counts stay in
the reports.

## Extras

DUES has a few small hidden extras: rare, short animations after a long
command that succeeded, and some hidden commands. Because forensic tools get
validated, this is what they are allowed to do:

- They appear only when stdin, stdout and stderr are all terminals, outside
  CI, after a progress command that succeeded with no warning and ran for at
  least ten seconds, with a 2% chance, at most once per process.
- They last at most 1.5 seconds and draw on stderr only. They erase
  themselves, and Ctrl-C ends one at once.
- Which one plays depends on the command that ran. None follows repair,
  whose output scripts read.
- They never touch evidence processing, output files, logs, stdout or exit
  codes.
- `--no-fun`, `--plain`, `--ci` or a build with
  `funDefault=off` turn them off. The hidden commands still run when asked
  for by name; `dues mew` then prints its cat without animating.

## Accessibility review

- **Colour never alone:** every state has a mark, a word and a colour.
- **Contrast:** AA (4.5:1) with a known background. With an unknown background,
  3:1 on bold tokens and marks, under WCAG 1.4.11. Sentences are always in the
  terminal's own colour.
- **Faint text** is only on the progress display's passing details. It can
  fall below 4.5:1 on some themes, so it never carries a state alone (marks
  and words do), and never holds a result, a warning or an error.
- **Colour blindness:** success and error differ in mark (`✓`/`✗`), word and
  luminance. `--color=ansi` hands colour to the terminal theme, so
  colour-blind and high-contrast themes apply.
- **Screen readers:** `--plain` prints discrete lines, with no
  redraw, no spinner and no box drawing. Screen readers read box-drawing
  characters aloud one by one.
- **Reduced motion:** under `--plain` nothing moves. `--no-fun` removes the
  extras alone.
- **Seizure safety:** frames come at most 12.5 per second, nothing flashes,
  extras last at most 1.5 seconds, and the moving area is a few cells (WCAG
  2.3.1).
- **Copy and paste:** hashes, IDs and paths are single tokens, never split by
  an escape code or cut on stdout.
- **Width:** layout is measured in display columns. The live display's rows
  are cut to the terminal's width so they never wrap; the run header and the
  closing line break at spaces instead. In East Asian locales use `--plain`,
  because their terminals often draw box characters two columns wide.

## Human review

The tests cover what a test can see. Before a release, look at these by hand:
- Windows Terminal, dark and light themes, with `--theme` and without: the
  overall row, the faint step rows, and with `--taskbar` the tab and taskbar
  progress (normal, paused at a prompt, red after a failure, cleared at the
  end).
- The Windows console host (`conhost.exe cmd`) with `--plain`: ASCII marks
  and emblem; without it, boxes where its font has no glyph.
- The VS Code terminal.
- `dues tui` on a dark and a light Windows Terminal profile, without
  `--theme` (it should match the profile) and with `--theme light` and
  `dark`; and over SSH, where a slow answer arrives after the menu shows.
- tmux and screen, and SSH to Linux (`TERM=xterm-256color`).
- A 50-column window: rows cut, the header and the closing line broken at
  spaces.
- `--color=never` (bold and faint only), `--color=ansi`, and `--plain` with a
  screen reader (NVDA).
- Ctrl-C once (the bar holds, the tab shows paused) and twice (the cursor is
  back, the tab cleared).

## Tests

- `internal/ui`:
  - the mode-resolution matrix, including when the taskbar is allowed;
  - palette contrast, and faint per theme and colour depth;
  - glyph widths, and the banner at every width and glyph set;
  - painter, message and header formats;
  - the overall row in each mode and the step rows, at widths 119 to 29;
  - live frames, the final frame and plain heartbeats with a fake clock,
    drawn on `internal/testutil/vtscreen`, a small terminal that rejects any
    escape sequence DUES must not write and any row that wraps;
  - taskbar states, their rate limit, and their clearing on stop, a second
    Ctrl-C and a panic;
  - prompt holding, panic and force-exit cleanup;
  - which extras follow which command;
  - a race test.
- `internal/progress`: classification and concurrency, and the tracker:
  every mode, the fraction that never goes back, the ETA rules, and an
  `Update` that allocates nothing.
- Each instrumented package: tests that assert its phases, its plan and its
  notes.
- `internal/e2e/output_contract_test.go`:
  - pure JSON on stdout from `list`, `micro list`, `repair --verify` and
    `repair --fix --revrel`;
  - the identity line from `version`, `--version` and piped help;
  - colour only when asked for, and no taskbar progress on a pipe.
  `testutil.RunDues` fails any piped run that writes an escape code.
- `internal/e2e/environment_test.go`: store, list, help and version print the
  same with an empty environment and with every variable DUES once read set,
  and nothing lands where those variables point.
- `internal/e2e/pty_test.go` and `pty_faultinject_test.go`: the binary on a
  real pseudo-terminal under WSL (see [TESTING.md](TESTING.md)).
- `tui/styles_test.go`: the TUI's styles in both themes against the same
  reference backgrounds as the palette (`internal/testutil/wcag`), its
  theme switch, and no emoji.
