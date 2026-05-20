# DUES TUI Implementation (Bubble Tea v2)

## Overview

DUES TUI is implemented on Bubble Tea v2 and organized as a state-driven app with one focused model per operation.

Current module versions in `go.mod`:

- `charm.land/bubbletea/v2`
- `charm.land/bubbles/v2`
- `charm.land/lipgloss/v2`

## Entry and Integration

- CLI command: `dues tui`
- Entry path: `main.go` -> `cli.TUICmd(...)` -> `tui.RunTUI(...)`
- `cli.TUICmd` creates action callbacks and opens a shared DB session.
- Operational stdout/stderr noise is suppressed via `runQuiet(...)` so the TUI remains clean.

## Bubble Tea v2 Architecture

### Why v2 matters here

- Import path is `charm.land/.../v2` for Tea, Bubbles, and Lip Gloss.
- Views are returned through `tea.View` and wrapped with `tea.NewView(...)` where needed.
- The app runs in alternate screen mode (`AltScreen = true`) for full-screen TUI behavior.
- Message-driven async commands are used for long-running operations to keep UI responsive.

### Root model

`tui/app.go` defines a root `Model` with:

- Window size tracking (`tea.WindowSizeMsg` handling).
- App states (`StateMenu`, `StateStore`, `StateList`, `StateSearch`, `StateRestore`, `StateNear`, `StateReset`).
- One submodel instance per workflow.
- Responsive layout via `applyWindowSize()`.

The root `View()` returns `tea.View` and sets `AltScreen = true`.

### Submodels

- `tui/store.go`: path input, sync/no-index toggles, "store another?" loop.
- `tui/list.go`: async load with spinner, selection, restore shortcut, clipboard copy.
- `tui/search.go`: query input and async search trigger.
- `tui/restore.go`: hash/path input and "restore another?" loop.
- `tui/near.go`: `in/out` mode selection, input, deep toggle.
- `tui/reset.go`: destructive confirmation with completion state.

All models use `Update(msg tea.Msg)` with command-returned completion messages (for example `storeCompletedMsg`, `searchCompletedMsg`) to keep long operations non-blocking.

## Input and Navigation Behavior

Global behavior in root model:

- `ctrl+c`: quit immediately.
- `q`: quit from menu; acts like back/escape on most child screens.
- `esc`: return to menu and reinitialize submodels.
- `enter` on menu: open selected flow.

Per-screen highlights:

- List screen supports `up/down` selection, `c` copy hash, `r`/`enter` restore selected item.
- NeAR screen supports `d` deep toggle while focused on input step.
- Store and Restore screens support continue-in-loop prompts after success.

## Styling System

`tui/styles.go` centralizes Lip Gloss styles:

- Shared border/title/help/input/button styles.
- Consistent color palette across screens.
- `CenterBox` and `Section` helpers for composition.

## Data/Action Wiring

`tui.Actions` (in `tui/app.go`) decouples UI from implementation:

- `Store`
- `Search`
- `Restore`
- `NearIn`
- `NearOut`
- `Reset`

Each function is injected from `cli/cmdtui.go` and delegates to existing core logic (`store`, `search`, `near`, DB reset), so CLI and TUI share the same backend behavior.

## Operational Notes

- TUI inherits global flags (`--dbpath`, `--password`, `--chonksize`, `--low`, `--quick`, `--container`, `--hierarchical`).
- Reset in TUI clears DB content and recreates the blobs directory.
- Search from TUI runs the same search backend and emits `report.json` in working directory.
- NeAR flows emit graph/report artifacts consistent with CLI usage.

## Current Behavior Guarantees

- Pressing `esc` from an active flow returns to the main menu and resets that flow model state.
- The list screen only shows completed evidence entries.
- The list screen restore action writes to auto-generated `restored_<hash>.bin` by default.
- Reset flow recreates blob storage directory after DB drop, so the app can continue in the same session.

## File Map

- `tui/app.go`: root state machine and routing.
- `tui/styles.go`: shared style primitives.
- `tui/store.go`: store form and execution loop.
- `tui/list.go`: DB list, hash copy, and restore shortcut.
- `tui/search.go`: search form and execution.
- `tui/restore.go`: restore form and execution loop.
- `tui/near.go`: in/out mode and deep scan toggle.
- `tui/reset.go`: reset confirmation flow.
