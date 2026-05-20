# Quick Start: DUES TUI (Bubble Tea v2)

## Launch

```bash
dues tui
```

Optional flags:

```bash
dues tui --dbpath ./case-db --password mysecret --chonksize 512 --container --hierarchical
```

## Main Menu Flows

1. Store File
2. List Files
3. Search
4. Restore File
5. NeAR Analysis
6. Reset Database

## When To Use TUI vs CLI

- Use TUI when you want guided, keyboard-driven operations in one persistent session.
- Use CLI commands for scripting, automation, or batch pipelines.
- Both paths call the same backend logic, so resulting data/artifacts are consistent.

## Global Keys

- `↑/↓`: move in menus/lists.
- `Enter`: confirm/select.
- `Tab`: move between fields/toggles.
- `esc`: back to main menu.
- `q`: quit from menu (also acts as back on most screens).
- `ctrl+c`: immediate exit.

## Workflow Details

### Store File

1. Enter file path.
2. Toggle options:
   - `Sync Index`
   - `Skip Index`
3. Select `Store`.
4. After success, choose whether to store another file.

### List Files

- Loads files asynchronously.
- Use `↑/↓` (or `k/j`) to move selection.
- `c`: copy selected hash to clipboard.
- `r` or `Enter`: restore selected item to auto path `restored_<hash>.bin`.

### Search

1. Enter query (minimum 2 characters).
2. Press `Enter` to run search.
3. Search backend writes `report.json` in current working directory.

### Restore File

1. Enter hash.
2. Enter restore path (default `restored`).
3. Confirm restore.
4. After success, choose whether to restore another file.

### NeAR Analysis

1. Choose mode:
   - `Find in Database` (`near in` equivalent)
   - `Find from File` (`near out` equivalent)
2. Enter hash or file path.
3. Optional: press `d` to toggle deep scan.
4. Run analysis.

### Reset Database

- Confirm destructive operation in the reset dialog.
- On completion, press `Enter`/`esc` to return to menu.

## Performance Flags You Can Combine

```bash
# Lower resource usage
dues tui --low

# Throughput-oriented mode
dues tui --quick

# Container + hierarchical indexing
dues tui --container --hierarchical
```

## Notes

- TUI uses the same backend as CLI commands, so outputs and behavior are consistent.
- If `--hierarchical` is passed without `--container`, container mode is auto-enabled.
- Search produces `report.json`; NeAR analysis produces graph/report artifacts used by the same tooling as CLI runs.
