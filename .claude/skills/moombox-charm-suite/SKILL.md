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
| `textarea` | `charm.land/bubbles/v2/textarea` | Multi-line text entry (available, not currently used) |
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
All 10 overlays (dialogs, forms, pickers) follow this interface — embedded in `App`, not standalone `tea.Model`s:
```go
func (m *SomeOverlay) Open()              { m.visible = true; m.reset() }
func (m *SomeOverlay) Close()             { m.visible = false }
func (m *SomeOverlay) IsVisible() bool    { return m.visible }
func (m *SomeOverlay) SetSize(w, h int)   { m.width = w; m.height = h }
func (m *SomeOverlay) Update(msg tea.Msg) tea.Cmd { ... }
func (m *SomeOverlay) View() string       { if !m.visible { return "" }; ... }
```

**Critical: `UpdateComponents(msg tea.Msg) tea.Cmd`** — Overlays with interactive sub-components (textinput, table, viewport, spinner) implement this method. App calls it from `routeComponentMsg()` to delegate focus, key handling, and ticks to embedded Charm components.

### Chord System
Single source of truth: `buildMenuItems()` in `app_actions.go` returns `[]ActionMenuItem`:
```go
type ActionMenuItem struct {
    Chord        string                   // e.g. "A A", "R C", "F"
    Label        string                   // display name for menu/help
    HintLabel    string                   // short label for feedback bar
    Category     string                   // "Action", "Request", "Open", "Filter", "Other"
    NeedsJob     bool                     // if true, opens job selector first
    NeedsConfirm bool                     // if true, requires 3rd keypress within 3s
    JobFilter    func(*database.Job) bool // conditional visibility based on job state
}
```
Adding a new chord:
1. Add `ActionMenuItem` entry in `buildMenuItems()`
2. Add case in `dispatchAction(chord, job)`

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
TUI receives events via channels from `main.go`. Sends are non-blocking — messages are silently dropped if the channel is full (no counters):
```go
select {
case ch <- msg:
default: // silently dropped if TUI event loop is busy
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
- `StyleLineFunc` — per-line styling callback (used in `log_viewer.go` to color log lines without embedding ANSI in content, which would break search highlight byte offsets)
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
