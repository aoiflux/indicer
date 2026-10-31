# DUES in the terminal

How DUES looks and behaves in a terminal: what goes to stdout and stderr, the
colours and marks, the progress display, the switches that turn decoration
off, and the accessibility review behind them. The code is `internal/ui`
(presentation) and `internal/progress` (what long operations report), on top
of `internal/console` (the only writer to the terminal).

## Output contract

1. **Results on stdout, everything else on stderr.** Results are JSON
   (`list`, `micro list`), reports (`store`, `restore`, `reindex`, `stats`),
   help and version text. The run header, progress, notes, warnings, errors
   and the occasional extra go to stderr. `dues list | jq` reads pure JSON.
2. **Piped output is the contract.** On a terminal stdout differs from piped
   stdout only by colour codes; marks, art and layout appear on stderr, or in
   help and version on a terminal. Nothing on a pipe holds an escape code
   unless `--color=always` or `FORCE_COLOR` asks for colour.
3. **Colour is added where text is written, never stored in a report.**
4. **Text from evidence is sanitised before it is styled.** File names,
   partition labels and error text pass through `internal/termsafe`, so a
   hostile name cannot move the cursor or change the terminal's title.
5. **No terminal queries.** DUES never asks the terminal for its background
   colour or capabilities: such queries are slow over SSH and leave stray
   bytes in the shell. It reads flags and environment variables only.
6. **Extras never touch evidence processing, output files, logs, stdout or
   exit codes.**

Messages on stderr read `dues: warning: ...` on a pipe, the form scripts
match, and `▲ warning: ...` on a terminal. A command that finished with a
warning (exit 2) says `warning`, not `error`.

## Switches

| Flag | Environment | Effect |
|---|---|---|
| `--color auto\|always\|never\|ansi` | `DUES_COLOR` | Colour policy for stdout and stderr. `ansi` keeps colour but only the terminal theme's 16 colours. |
| `--plain` | `DUES_PLAIN=1` | Keep the terminal, drop decoration: no colour or escape codes at all, ASCII only, no redraw, no spinner, no art, no extras. The mode for screen readers, reduced motion and copying logs. |
| `--ci` | any of `CI`, `GITHUB_ACTIONS`, `GITLAB_CI`, `BUILDKITE`, `JENKINS_URL`, `TF_BUILD`, `TEAMCITY_VERSION` | Behave as if no terminal is attached: plain output on every stream, and prompts refuse as they do on a pipe. |
| `--no-fun` | `DUES_NO_FUN=1` | No extras; everything else unchanged. |
| | `DUES_THEME=dark\|light` | The terminal's background, for the tuned palettes. |
| | `NO_COLOR` (any value) | No colour; bold stays. |
| | `FORCE_COLOR`, `CLICOLOR_FORCE` | Colour even on a pipe. |
| | `TERM=dumb` | No colour, no motion, ASCII. |

Precedence: flag, then `DUES_*`, then the standard variables, then what the
streams are. A build for an organisation that wants no extras sets
`-ldflags "-X indicer/internal/ui.funDefault=off"`.

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

- The background is read from `DUES_THEME`, then `COLORFGBG`. Otherwise it is
  unknown, which is the common case: macOS Terminal and xterm start light,
  Windows Terminal and VS Code dark.
- `internal/ui/palette_test.go` checks the contrast of every value, and of the
  nearest xterm-256 colour a 256-colour terminal shows instead:
  - the dark and light palettes reach WCAG AA (4.5:1) on six dark and five light
    reference backgrounds;
  - the unknown-background palette reaches 3:1 on all eleven. It is used bold,
    on marks and short tokens.
- Muted text keeps the terminal's own colour when the background is unknown.

## Marks

| Set | Used on | Marks | Spinner | Bar |
|---|---|---|---|---|
| Full | Windows Terminal, VS Code, macOS, Linux UTF-8 locales | `✓ ✗ ▲ ● ■ · – ◆` | braille | `━╸─` |
| Console | the Windows console host without a modern terminal | `+ x ! * # . -` | `\|/-\` | `█░` |
| ASCII | `--plain`, `TERM=dumb`, non-UTF-8 and East Asian locales | `+ x ! * # . -` | `\|/-\` | `#>-` |

No emoji anywhere. Every mark is one column wide.

## The banner

Bare `dues`, `dues help` and `dues version` open with the wordmark when
stdout is a terminal:

```
┌─╮ ╷ ╷ ┌─╴ ╭─╴   Deduplicated Unified Evidence Store
│ │ │ │ ├─  ╰─╮   v0.39.0 "<pineapple> spacebar"
└─╯ ╰─╯ └─╴ ╶─╯   42e2ded · windows/amd64 · go1.27.3
```

The console and ASCII sets use an ASCII wordmark. Below 60 columns only the
wordmark shows. Piped help and version text has no art. Long operations open
with a run header on stderr instead: what runs, on what, into which
database, and the settings in effect.

## Progress

Long operations (store, restore, reindex, enrich, NeAR, micro extract,
search, and repair with `--verify`, `--upgrade` or `--fix`) show their phases.

- **On a terminal:** a live display redrawn ten times a second:
  - a bar, percent, amounts, rate and ETA for work of known size;
  - a spinner, count and rate for work of unknown size.
  Phases that run together (storage and indexing) show together. Lines other
  code prints go above the display, and a prompt pauses it.
- **On a pipe, under `--plain` or `--ci`:** plain lines. One when a phase
  starts, one when it ends, and one every 15 seconds while it runs:

  ```
  dues: store: start, 4.20 GiB
  dues: store: 31%, 1.30 of 4.20 GiB, 110 MiB/s, eta 27s
  dues: store: done, 4.20 GiB in 38.2s, 112 MiB/s
  dues: done in 38.2s, read 4.20 GiB, new 2.88 GiB, deduplicated 1.32 GiB (31%)
  ```

- **In the TUI, the server, the FFI library and the compression product:**
  nothing.

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
- They never touch evidence processing, output files, logs, stdout or exit
  codes.
- `--no-fun`, `DUES_NO_FUN=1`, `--plain`, `--ci` or a build with
  `funDefault=off` turn them off.

## Accessibility review

- **Colour never alone:** every state has a mark, a word and a colour.
- **Contrast:** AA (4.5:1) with a known background. With an unknown background,
  3:1 on bold tokens and marks, under WCAG 1.4.11. Sentences are always in the
  terminal's own colour.
- **Colour blindness:** success and error differ in mark (`✓`/`✗`), word and
  luminance. `--color=ansi` hands colour to the terminal theme, so
  colour-blind and high-contrast themes apply.
- **Screen readers:** `--plain` and `TERM=dumb` print discrete lines, with no
  redraw, no spinner and no box drawing. Screen readers read box-drawing
  characters aloud one by one.
- **Reduced motion:** under `--plain` nothing moves. `--no-fun` removes the
  extras alone.
- **Seizure safety:** frames come at most 12.5 per second, nothing flashes,
  extras last at most 1.5 seconds, and the moving area is a few cells (WCAG
  2.3.1).
- **Copy and paste:** hashes, IDs and paths are single tokens, never split by
  an escape code or cut on stdout.
- **Width:** layout is measured in display columns. Rows are cut to the
  terminal's width so they never wrap. East Asian locales get the ASCII set,
  because their terminals often draw box characters two columns wide.

## Tests

- `internal/ui`:
  - the mode-resolution matrix;
  - palette contrast;
  - glyph widths;
  - painter, message and header formats;
  - renderer frames at several widths, against a small terminal emulator
    (`vtscreen_test.go`) that also rejects any escape sequence DUES must not
    write;
  - prompt holding, panic and force-exit cleanup;
  - a race test.
- `internal/progress`: classification and concurrency.
- Each instrumented package: tests that assert its phases.
- `internal/e2e/output_contract_test.go`:
  - pure JSON on stdout;
  - colour only when asked for;
  - `NO_COLOR` and `--color=never` beat `FORCE_COLOR`.
  `testutil.RunDuesEnv` fails any piped run that writes an escape code.
