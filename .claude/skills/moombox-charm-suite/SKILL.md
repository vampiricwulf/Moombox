---
name: moombox-charm-suite
description: Use when building or modifying TUI features — component catalog for bubbletea, bubbles, huh, and lipgloss, plus Moombox overlay, chord, and async patterns
---

# Charm Suite — TUI Development

**Rule: Check [Charm's repos](https://github.com/charmbracelet) for existing components before building custom ones.**

All imports use V2 paths: `charm.land/bubbletea/v2`, `charm.land/bubbles/v2/...`, `charm.land/huh/v2`, `charm.land/lipgloss/v2`.

## Component Catalog

### bubbles (pre-built components)
Components currently used in the codebase are marked with *.

| Component | Import | Use For |
|-----------|--------|---------|
| `list` * | `charm.land/bubbles/v2/list` | Scrollable item lists with filtering |
| `viewport` * | `charm.land/bubbles/v2/viewport` | Scrollable text content (logs, help) |
| `textinput` * | `charm.land/bubbles/v2/textinput` | Single-line text entry |
| `spinner` * | `charm.land/bubbles/v2/spinner` | Loading indicators |
| `progress` * | `charm.land/bubbles/v2/progress` | Progress bars |
| `table` * | `charm.land/bubbles/v2/table` | Tabular data display |
| `filepicker` * | `charm.land/bubbles/v2/filepicker` | File/directory selection (used in files dialog) |
| `cursor` | `charm.land/bubbles/v2/cursor` | Cursor rendering modes (used internally by textinput/textarea) |
| `key` * | `charm.land/bubbles/v2/key` | Key binding definitions |
| `textarea` | `charm.land/bubbles/v2/textarea` | Multi-line text entry — the `A C` chapter import's paste box (`internal/tui/chapter_import_dialog.go`); its own keys (Tab, Ctrl+S, Esc) are kept from it in `UpdateComponents` |
| `paginator` | `charm.land/bubbles/v2/paginator` | Page navigation (available, not currently used) |
| `stopwatch` | `charm.land/bubbles/v2/stopwatch` | Elapsed time tracking (available, not currently used) |
| `timer` | `charm.land/bubbles/v2/timer` | Countdown timer (available, not currently used) |

### huh (form composition)
Used for setup wizard and FFmpeg check dialog. Builds forms from composable pieces:
```go
form := huh.NewForm(
    huh.NewGroup(
        huh.NewInput().Title("Port").Accessor(&accessor),
        huh.NewSelect[string]().Title("Access").Options(
            huh.NewOption("Localhost", "localhost"),
            huh.NewOption("LAN", "lan"),
        ),
    ),
).WithTheme(moomboxTheme(isDark))
form.SubmitCmd = nil // Prevent huh from quitting on submit
```
Key types: `Form`, `Group`, `NewInput`, `NewSelect`, `NewOption`, `NewConfirm`. Form values use `MapAccessor` (implements `huh.Accessor[string]`) for map-backed storage.

**Critical: `SubmitCmd = nil`** — Moombox sets this on every huh form to prevent the form from sending a quit command when submitted. Without this, form submission exits the entire TUI. The form's completion is detected via `form.State == huh.StateCompleted` in the update loop instead.

### lipgloss (styling)
```go
style := lipgloss.NewStyle().
    Foreground(lipgloss.Color("#cd5cff")).
    Bold(true).Padding(0, 1).
    Border(lipgloss.RoundedBorder())

lipgloss.JoinHorizontal(lipgloss.Top, left, right)
lipgloss.JoinVertical(lipgloss.Left, top, bottom)
```

## Moombox TUI Patterns

### Overlay Pattern
All 16 overlays (dialogs, forms, pickers) follow this interface — embedded in `App`, not standalone `tea.Model`s:
```go
func (m *SomeOverlay) Open()              { m.visible = true; m.reset() }
func (m *SomeOverlay) Close()             { m.visible = false }
func (m *SomeOverlay) IsVisible() bool    { return m.visible }
func (m *SomeOverlay) SetSize(w, h int)   { m.width = w; m.height = h }
func (m *SomeOverlay) Update(msg tea.Msg) tea.Cmd { ... }
func (m *SomeOverlay) View() string       { if !m.visible { return "" }; ... }
```

**Critical: `UpdateComponents(msg tea.Msg) tea.Cmd`** — Overlays with interactive sub-components (textinput, table, viewport, spinner) implement this method. App calls it from `routeComponentMsg()` to delegate focus, key handling, and ticks to embedded Charm components.

**Adding an overlay — the registry is the only place it is routed from (K23).** `handleKey`, `handleMouse`, `View`, `routeComponentMsg` and `hasActiveOverlay` all take the first visible entry of `overlayRegistry` (`internal/tui/overlays.go`); an overlay missing from it never receives a key, a click or a frame. Three steps:
1. An `overlayEntry` in `overlayRegistry` at the priority it should have (`visible`, `view`, `components`, `key`, and `mouse` or nil to swallow the mouse). An overlay with anything to scroll takes the wheel (owner ruling R-C2): three rows a notch for text (`mouseHelp`), one cursor row for a list or table (`wheelAsArrow` in `internal/tui/app_mouse.go`, which delivers the notch as the Up or Down key and takes a predicate saying a list is on screen); clicks stay inert unless the overlay already handles them. State the answer in `overlayWheel` (`internal/tui/overlay_order_test.go`), with a row in `overlayWheelCases` (`internal/tui/overlay_wheel_test.go`) when it scrolls. The 8 ms progress tick idles at 500 ms under an overlay; set `liveProgress` only if the overlay draws progress that tick delivers (the trim dialog), and add its row to `overlayLiveProgress` in `internal/tui/overlay_order_test.go` either way. Give it a `title` (the name the operator reads on the notice that covers it in a terminal smaller than it needs) and state its `bare` answer (see "Bare overlays" below): `nil` when it needs 60x20, or an `overlayBare` with the smallest terminal it opens in. Unless its Esc closes it from every state, give it a `heldEsc` func (`internal/tui/overlay_held.go`) saying what Esc does under that notice right now: closes, steps back, or off (never delivered: an Esc that would quit, or raise or answer a prompt nobody can see). State its answer in `heldEscBase` (`internal/tui/app_compact_test.go`), unless its bare form draws at every compact size, so that it is never held.
2. Its key handler (`func (a *App) keyX(msg tea.KeyPressMsg, key string) (tea.Model, tea.Cmd)`) in `internal/tui/overlay_keys.go` — Ctrl+C is checked ahead of every overlay in `handleKey`, so the handler never sees it.
3. A row in `overlayCases` (`internal/tui/overlay_order_test.go`) at the same position, which opens the overlay and states its bare minimum (`0, 0` for none); the order, view/key and mouse-parity tests, the Ctrl+C census and the draw table (`TestEveryOverlayIsDrawnForItsSize`, `internal/tui/overlay_bare_test.go`) iterate it.

### Chord System
Single source of truth: `buildMenuItems()` in `internal/tui/app_actions.go` returns `[]ActionMenuItem`:
```go
type ActionMenuItem struct {
    Chord        string                   // e.g. "A A", "R C", "F"
    Label        string                   // display name for menu/help
    HintLabel    string                   // short label for feedback bar
    Category     string                   // "Action", "Request", "Open", "Filter", "Other"
    NeedsJob     bool                     // if true, opens job selector first
    NeedsConfirm bool                     // if true, requires 3rd keypress within 3s
    JobFilter    func(*database.Job) bool // conditional visibility based on job state
    Opens        string                   // overlayRegistry name of the overlay it opens, "" for none
}
```
Adding a new chord:
1. Add `ActionMenuItem` entry in `buildMenuItems()`. If the action opens an overlay, name it in `Opens`: in a terminal smaller than that overlay needs, the action is then refused with the size named, instead of opening something that cannot be drawn.
2. Add case in `dispatchAction(chord, job)`
3. Classify it in `chordCompact` (`internal/tui/app_compact_test.go`): "works" (opens no overlay), "bare WxH" (its overlay opens bare from that size) or "refused" (its overlay needs 60x20).

That's it. Hints, help screen, action menu, and chord feedback all derive from `buildMenuItems()` automatically.

**Prefixes:** A (Action), R (Request), O (Open), Q (Quit). **Single keys:** F, M, `, ?.

### Async Operations
Long-running work returns a `tea.Cmd` that sends a custom message:
```go
case "R V":
    return a, func() tea.Msg {
        info, err := a.OnCheckUpdate()
        return updateCheckResultMsg{Info: info, Err: err}
    }
```
Never call blocking operations directly in `Update()`.

### Non-Blocking Channel Sends
TUI receives events via channels made in `cmd/moombox/tui_wiring.go`. Sends are non-blocking. The four job forwarders go through `forwardOrDrop` (`cmd/moombox/tui_wiring.go`), which counts a drop, warns once per streak and replays a full snapshot on the next send that gets through; a new channel that carries job state must use it. Other channels drop without replay (log lines are counted and reported at exit):
```go
select {
case ch <- msg:
default: // dropped while the TUI event loop is busy; no replay, so never for job state
}
```

### Styling Conventions
- **Status colors**: `StatusColor(status)` and `StatusIcon(status)` for consistent job state visualization
- **Borders**: `FocusedBorder` (cyan) vs `UnfocusedBorder` (gray)
- **Form theming**: `moomboxTheme(isDark)` customizes huh form colors, borders, button styles
- **TUI helpers**: `newSpinner()`, `newTextInput()`, `configureTextInput()`, `renderInactiveInput()`, `renderPasswordDots()`

### Panel Layout
Three-panel layout with dynamic sizing based on `FocusPanel` (Tasks, Details, Logs):
- Focused panel gets more space (70/25 height split, 45/55 or 35/65 width split)
- `PanelRegion` provides mouse hit-testing with `Contains()` and `ContentY()` for click detection

### Layouts by Terminal Size
The size alone picks one of three layouts, through one predicate: `layoutModeFor(width, height)` / `App.layoutMode()` in `internal/tui/app_compact.go`. It is derived on every call. Never store the answer, and never write a second size comparison: `View`, `handleKey`, `handleMouse` and the tick gating all ask it.

- **Normal** (60x20 and up): the three panels and every overlay in its bordered form.
- **Compact** (below that, down to 30x6): the task list filling the screen, one line a job (`internal/tui/task_list_compact.go`: status tag, title, figure), then a rule, a one-line status bar (`compactStatusLine`) and the feedback line, which wraps upward over them while its message needs more than one row (`compactFeedbackRows`). It is a second drawing of the same models, with no state of its own. Navigation, selection, the `/` filter and the chords that open no overlay work. An overlay with a bare form is drawn bare; an action whose overlay (`ActionMenuItem.Opens`, or the action menu for `M`) does not open at this size is refused on the feedback line, naming the size it needs (`refuseOverlay`, `refusedForSize`).
- **Too small** (below 30x6): a notice; only Ctrl+C works.

An overlay open in a terminal smaller than it needs is kept under a notice, which names it and the size it needs and says what Esc does; it gets its own Esc only where `App.heldEscFor` says that Esc closes it or steps back. `App.screenHeld()` says a notice has replaced the screen: check it before delivering any new kind of input (keys, mouse and paste already do).

### Bare overlays (below 60x20)
What an overlay does below the normal layout is one derived predicate in `internal/tui/overlay_bare.go`, never stored and never re-derived elsewhere: `App.overlayDrawFor(o)` for an open overlay (`drawNormal`, `drawBare` or `drawHeld`) and `App.overlayOpensHere(name)` for one not yet open (whether it opens, and the size it needs). `View`, the key guard, `handleMouse`, `screenHeld`, the refusals, the prefix hint and Help's compact list all ask it. Do not write a size comparison about an overlay anywhere else.

Every overlay states a bare answer in the `bare` field of its registry entry: `nil` (it needs 60x20; it is refused and held below that), or `&overlayBare{openW, openH, now}`: the smallest terminal it opens in, and `now` when one of its states needs more than its first screen (that state alone is then held, under a notice naming its size). Settings (every page: `internal/tui/settings_bare.go`, `internal/tui/settings_bare_lists.go`), Help, the release notes, the Job Log, Statistics, client tokens, the yt-dlp plugin, the cookie file import, the orphaned items, the action menu, Add Video, Trim and Import Archive are bare from 30x6. Import Chapters is the one with a `now`: 30x10 while editing, 30x6 on its confirm and saving steps (`ChapterImportDialogModel.bareNeed`). The setup wizard and the FFmpeg check have none, by owner ruling R-C10: they are the only overlays still refused and held below 60x20, and the only ones with a `heldEsc` func. Import Chapters can still be held on its editing step below 30x10; it has no func (its Esc closes it) but keeps a `heldEscBase` row.

To give an overlay a bare form:
1. In the model, make the first statement of `View` (after the visibility check) and of `SetSize` branch on `layoutModeFor(m.width, m.height) == layoutCompact` into new functions in a file of its own beside the model (`internal/tui/help_bare.go` is the pattern). Derive it from the model's own size on every call; store no mode. Do not edit the normal renderer: every frame at 60x20 and above must stay byte-identical.
2. Draw with `bareSheet` (`internal/tui/bare_sheet.go`): `left`/`right` header (the right half is never cut), `body` rows, an optional last `keys` row, and `msg` for an error or a confirm, which wraps upward from the bottom and never covers the first `keep` body rows (the focused input, the armed row). No border, no `centerBox`. The view must be exactly width x height cells at every compact size at or above the minimum, 200x8 and 30x50 included; `App.View`'s `fitBlock` is only a safety net.
   A list overlay branches its delegate's `Render` the same way and gives the list its bare size in its own `SetSize`; the patterns are `internal/tui/client_tokens_bare.go` and `internal/tui/files_dialog_bare.go`. Keep a one-cell cursor mark: colour alone must not tell the selected row from the armed one. Derive an armed confirm in the bare view from the model's confirm fields, never from a pre-worded message, so it survives a resize. `bareWrap`, `bareHeadCut` and `bareMiddleCut` (same file) wrap an error and cut a path or an address without losing its end. An error is wrapped, not cut to one row; where it shares the screen with a list, the list gives up rows to it and keeps the cursor's row.
3. Anything the bare branch sets on a shared component (a viewport or input width, a placeholder, SoftWrap) must be set back by the normal branch: `TestOverlayFramesSurviveAShrinkAndBack` compares a shrunk-and-regrown 60x20 frame with one never shrunk. A `textinput` draws its width plus one cell, and `SetWidth` alone never lays its value out again (the input only does that on an edit, and only when the cursor has left the window it drew before): `bareInputWidth` (`internal/tui/bare_sheet.go`) sets a width and re-lays the value whatever the cursor's place. Where the normal form gives the input a width (Add Video, the cookie path, the Job Log search, Settings), draw the bare input from a copy (`bareInput`, `internal/tui/bare_sheet.go`) and set nothing on the model's own, so a shrink and a regrow with nothing typed leaves the normal frame as it was wherever the cursor is (`TestSharedInputIsUntouchedByAShrinkAndBack`, `internal/tui/overlay_bare_relay_test.go`); a value written while the terminal was small is laid out with `bareInputWidth` for the normal form when it grows back (a `bareHeldInput` in `SetSize` says whether it was; `SettingsModel.bareLeave` for Settings; `TestSharedInputIsLaidOutAgainAcrossForms`). Where the normal form gives the input no width (Trim, Import Archive), set the bare width with `bareInputWidth` and take it off again on leaving.
4. Set `bare` on the registry entry, the bare minimum in its `overlayCases` row, and the class of its chord in `chordCompact` ("bare WxH"). Delete its `heldEsc` func and its `heldEscBase` row once every state draws at 30x6. Secrets stay hidden in the bare form as in the normal one.
5. Help's compact list (`internal/tui/help_bare.go`) follows by itself for a chord; a static row there that belongs to an overlay names it in `opens`. The bare action menu dims an action whose overlay does not open at its size by the same predicate (`ActionMenuModel.sizeRefusal`).
6. Errors and confirms (owner ruling R-C11): an error or a confirm is shown whole or wraps upward, and never covers the focused input. `bareSheet.errorUnder` puts one under the rows it may not cover (the title gives way first, then the keys row); a message that reaches the header stays in one block, the kept rows moving up over the header; a list hands a long error to the sheet as `msg` with `keep` on the cursor's row; a prompt that sits on the keys row (Settings' `Save changes?`) is never given up to a message; `noteUnder` adds a hint only where it fits. A confirm that names what it deletes and cannot be shown whole is not armed: the bare action menu refuses it with the size that shows it, worked out on every draw and never stored as text (`bareRefuseConfirm`, `bareRefusal`, `internal/tui/action_menu_bare.go`), and checks again on the confirming Enter, since the terminal may have shrunk since.
7. A page with a focus (a field list, an editor) derives its window of rows from the focus on every draw and stores no scroll state: `bareWindow` and `SettingsModel.barePage` (`internal/tui/settings_bare.go`), which also make the rows give way to a message down to the focused item. A key the bare form cannot honour (one that reaches something not drawn, or opens an overlay that needs 60x20) is answered in the model's bare key path before the normal handler sees it (`SettingsModel.bareKey`).
8. Do not resize a shared `table`, and do not let a `filepicker` keep a stale window. The bare Add Video draws its format rows by hand from `table.Rows()` and pages itself (`internal/tui/add_video_bare.go`), so the normal form finds the tables as it left them. A `filepicker` follows a new height only on a `tea.WindowSizeMsg` and never moves its window to the cursor (`ImportDialogModel.bareSetSize`); back in the normal layout its window is laid out from the folder's top again (`pickerAnchor`).

When you add something the task list draws, draw it (or decide to leave it out) in `renderCompactJob` too. When you add a warning to the status bar, check it reaches `StatusBarModel.alerts()`, which the compact status line shows. `internal/tui/app_compact_test.go` has the size table, the golden frames at 40x10 and 30x6, and the cached-against-fresh frame comparison.

### Panel Render Caches
The task list, job details, log viewer and status bar memoise `View()`: each returns its stored `renderCache` while a freshly built key struct (`taskListKey`, `jobDetailsKey`, `logViewerKey`, `statusBarKey`) equals the one the frame was drawn for. The frame is asked for ~120 times a second and most messages change nothing a panel draws.

**The rule:** anything `View()` reads must be a field of that panel's key struct, or change only through the panel's content funnel — `rebuildVirtualList` (task list, bumps `rebuildSeq`) or `updateViewportContent` (details and log viewer, bumps `contentSeq`). The status bar has no funnel: every input is a key field. `invalidate()` on the task list and log viewer is for the rare change that moves neither. Miss this and the panel shows a stale frame, and a test of the new feature that renders once will not notice.

When you add state a panel draws (a header badge, a row tag, a new status-bar segment):
1. Add it to the key struct, or route its writer through the funnel.
2. Add a row to the matching table in `internal/tui/render_cache_test.go` — `TestTaskListCachedFramesMatchFreshFrames`, `TestJobDetailsCachedFramesMatchFreshFrames`, or `TestStatusBarCacheKeyCoversEveryInput` (one row per key field). Each compares the cached frame with a fresh one after the mutation.
3. State that `App.View` itself reads on its panel path (not inside a panel: a banner, the feedback line) goes in `frameKey` (`internal/tui/frame_compose.go`), which memoises the whole composed screen, with a row in `TestFrameMemoFollowsEveryInput` (`internal/tui/frame_compose_test.go`). The composer's output must stay byte-identical to the lipgloss joins; the tests in that file compare them.
4. Run `go test -run 'Cache|FrameCost|Compose|FrameMemo' ./internal/tui/`. `TestFrameCostAtLogCap` (`internal/tui/frame_cost_test.go`) pins the allocations and bytes of an unchanged frame; `go test -run '^$' -bench 'AtLogCap' -benchmem ./internal/tui/` shows the timings.

Details: `docs/spec/user-interfaces.md` § Panel Render Caches.

### Custom Components
- **Marquee**: Bouncing text scroller for long job titles (resets on timer, pauses at ends)
- **ProgressStore**: Thread-safe map for progress ticks — prevents re-sorting task list on every 100ms update

## V2 Features

### Declarative Views
Root `View()` returns `tea.View`, subcomponents return `string`. Use `tea.NewView(content)` to create a view with terminal mode settings:
```go
func (a *App) viewWithMode(content string) tea.View {
    v := tea.NewView(content)
    v.AltScreen = true
    v.MouseMode = tea.MouseModeCellMotion
    v.WindowTitle = a.windowTitle
    return v
}
```
Fields on `tea.View`: `AltScreen`, `MouseMode`, `WindowTitle`, `Cursor`, `ReportFocus`.

### Terminal Detection
Request background color in `Init()` via `tea.RequestBackgroundColor`. The runtime sends two messages on startup:

- **`tea.BackgroundColorMsg`** — use `msg.IsDark()` to determine dark/light terminal. Moombox stores `isDark` on App and propagates to components that need it (huh themes, styles).
- **`tea.ColorProfileMsg`** — `msg.Profile` provides terminal color capability (`colorprofile.TrueColor`, `ANSI256`, `ANSI`, `ASCII`, `NoTTY`). Sent automatically; no explicit request needed.

### Color Adaptation (available, not yet used)
`lipgloss.LightDark(isDark)` returns a function that selects between two colors based on terminal background. Useful for adaptive theming without maintaining separate style sets.

### Viewport V2
Functional options constructor: `viewport.New(viewport.WithWidth(w), viewport.WithHeight(h))`.

Key features used in Moombox:
- `StyleLineFunc` — per-line styling callback (used in `internal/tui/log_viewer.go` to color log lines without embedding ANSI in content, which would break search highlight byte offsets)
- `LeftGutterFunc` — callback for rendering line numbers in the gutter
- `SetHighlights([]viewport.Highlight)` — search result highlighting with byte offsets
- `SetContentLines([]string)` — set content from a string slice directly (avoids joining/splitting)
- `FillHeight = true` — viewport fills remaining terminal height
- `SetWidth(w)` / `SetHeight(h)` — resize after construction

### TextInput V2
Style management via accessors:
- `Styles()` / `SetStyles(s)` — get and set the styles struct
- `SetWidth(w)` — set input width (field assignment replaced by method)
- `SetCursor(pos)` — position the cursor programmatically

### Focus Detection (available, not yet used)
`tea.FocusMsg` / `tea.BlurMsg` are sent when the terminal window gains or loses focus. Enable with `v.ReportFocus = true` on the `tea.View`. Useful for pausing updates or dimming the UI when the terminal is not in focus.

### Mouse Messages
V2 uses typed mouse messages instead of V1's generic `tea.MouseMsg`:
- `tea.MouseClickMsg` — mouse button pressed (used for panel clicks, button clicks)
- `tea.MouseReleaseMsg` — mouse button released
- `tea.MouseWheelMsg` — scroll wheel (used for viewport scrolling)
- `tea.MouseMotionMsg` — mouse movement

Each carries `.X`, `.Y` coordinates and button information.

## Common Mistakes

- Building a custom list when `charm.land/bubbles/v2/list` exists (with built-in filtering, pagination, and key bindings)
- Calling blocking I/O in `Update()` instead of returning a `tea.Cmd`
- Forgetting `SetSize()` on overlays when window resizes
- Missing `UpdateComponents()` on overlays with interactive sub-components — inputs won't receive keys
- Adding chord logic outside `buildMenuItems()`/`dispatchAction()` — breaks the single source of truth
- Forgetting `form.SubmitCmd = nil` on huh forms — causes the TUI to quit on form submission
- Using V1 `tea.MouseMsg` instead of the typed V2 variants (`tea.MouseClickMsg`, etc.)
