---
name: moombox-charm-suite
description: Use when building or modifying TUI features — component catalog for bubbletea, bubbles, huh, and lipgloss, plus Moombox overlay, chord, and async patterns
---

# Charm Suite — TUI Development

**Rule: Check [Charm's repos](https://github.com/charmbracelet) for existing components before building custom ones.**

## Component Catalog

### bubbles (pre-built components)
Components currently used in the codebase are marked with *.

| Component | Import | Use For |
|-----------|--------|---------|
| `list` * | `bubbles/list` | Scrollable item lists with filtering |
| `viewport` * | `bubbles/viewport` | Scrollable text content (logs, help) |
| `textinput` * | `bubbles/textinput` | Single-line text entry |
| `spinner` * | `bubbles/spinner` | Loading indicators |
| `progress` * | `bubbles/progress` | Progress bars |
| `table` * | `bubbles/table` | Tabular data display |
| `filepicker` * | `bubbles/filepicker` | File/directory selection (used in files dialog) |
| `cursor` * | `bubbles/cursor` | Cursor rendering modes |
| `key` * | `bubbles/key` | Key binding definitions |
| `textarea` | `bubbles/textarea` | Multi-line text entry (available, not currently used) |
| `paginator` | `bubbles/paginator` | Page navigation (available, not currently used) |
| `stopwatch` | `bubbles/stopwatch` | Elapsed time tracking (available, not currently used) |
| `timer` | `bubbles/timer` | Countdown timer (available, not currently used) |

### huh (form composition)
Used for setup wizard advanced mode. Builds forms from composable pieces:
```go
form := huh.NewForm(
    huh.NewGroup(
        huh.NewInput().Title("Port").Value(&accessor),
        huh.NewSelect[string]().Title("Access").Options(
            huh.NewOption("Localhost", "localhost"),
            huh.NewOption("LAN", "lan"),
        ),
    ),
).WithTheme(moomboxTheme())
```
Key types: `Form`, `Group`, `NewInput`, `NewSelect`, `NewOption`, `NewConfirm`. Form values use `MapAccessor` (implements `huh.Accessor[string]`) for map-backed storage.

### lipgloss (styling)
```go
style := lipgloss.NewStyle().
    Foreground(lipgloss.Color("205")).
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
Single source of truth: `buildMenuItems()` in `app.go` returns `[]ActionMenuItem`:
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
- **Form theming**: `moomboxTheme()` customizes huh form colors, borders, button styles
- **TUI helpers**: `newSpinner()`, `newTextInput()`, `configureTextInput()`, `renderInactiveInput()`, `renderPasswordDots()`

### Panel Layout
Three-panel layout with dynamic sizing based on `FocusPanel` (Tasks, Details, Logs):
- Focused panel gets more space (70/25 height split, 45/55 or 35/65 width split)
- `PanelRegion` provides mouse hit-testing with `Contains()` and `ContentY()` for click detection

### Custom Components
- **Marquee**: Bouncing text scroller for long job titles (resets on timer, pauses at ends)
- **ProgressStore**: Thread-safe map for progress ticks — prevents re-sorting task list on every 100ms update

## Common Mistakes

- Building a custom list when `bubbles/list` exists (with built-in filtering, pagination, and key bindings)
- Calling blocking I/O in `Update()` instead of returning a `tea.Cmd`
- Forgetting `SetSize()` on overlays when window resizes
- Missing `UpdateComponents()` on overlays with interactive sub-components — inputs won't receive keys
- Adding chord logic outside `buildMenuItems()`/`dispatchAction()` — breaks the single source of truth
