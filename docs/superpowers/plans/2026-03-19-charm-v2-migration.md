# Charm V2 Migration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Migrate the TUI package from Charm V1 (bubbletea v1.3.10, bubbles v1.0.0, lipgloss v1.1.0, huh v0.8.0) to Charm V2 (charm.land/bubbletea/v2, charm.land/bubbles/v2, charm.land/lipgloss/v2, charm.land/huh/v2).

**Architecture:** All 26 Go files in `internal/tui/` import Charm libraries. Go's module system requires all imports to change atomically — you cannot mix V1 and V2 in the same binary. The migration touches import paths, type signatures, API calls, and constructor patterns. Changes are mechanical (find-and-replace + targeted refactors), not architectural.

**Tech Stack:** Go 1.25, charm.land/bubbletea/v2, charm.land/bubbles/v2, charm.land/lipgloss/v2, charm.land/huh/v2, image/color (stdlib)

**Critical constraint:** All tasks must be completed before the code will compile. No intermediate compilation is possible because V1 and V2 module paths are incompatible. Verification happens only after all tasks are done.

---

## Reference: Upgrade Guides

- [Bubble Tea V2 Upgrade Guide](https://github.com/charmbracelet/bubbletea/blob/main/UPGRADE_GUIDE_V2.md)
- [Bubbles V2 Upgrade Guide](https://github.com/charmbracelet/bubbles/blob/main/UPGRADE_GUIDE_V2.md)
- [Lip Gloss V2 Upgrade Guide](https://github.com/charmbracelet/lipgloss/blob/main/UPGRADE_GUIDE_V2.md)
- [Huh V2 Upgrade Guide](https://github.com/charmbracelet/huh/blob/main/UPGRADE_GUIDE_V2.md)

---

### Task 1: Update go.mod Dependencies

**Files:**
- Modify: `go.mod`
- Modify: `go.sum` (auto-updated by `go get`)

- [ ] **Step 1: Fetch V2 modules**

```bash
cd D:/Git/Moombox
go get charm.land/bubbletea/v2@latest
go get charm.land/bubbles/v2@latest
go get charm.land/lipgloss/v2@latest
go get charm.land/huh/v2@latest
```

- [ ] **Step 2: Verify modules resolve**

Run: `go list -m charm.land/...`
Expected: All four V2 modules listed

---

### Task 2: Update All Import Paths

**Files (all 26 in `internal/tui/`):**
- Modify: Every `.go` file in `internal/tui/`

Apply these exact find-and-replace operations (order matters — do subpackages first):

- [ ] **Step 1: Replace bubbles subpackage imports**

Find-and-replace across all files in `internal/tui/`:

| Find | Replace |
|------|---------|
| `"github.com/charmbracelet/bubbles/cursor"` | `"charm.land/bubbles/v2/cursor"` |
| `"github.com/charmbracelet/bubbles/filepicker"` | `"charm.land/bubbles/v2/filepicker"` |
| `"github.com/charmbracelet/bubbles/key"` | `"charm.land/bubbles/v2/key"` |
| `"github.com/charmbracelet/bubbles/list"` | `"charm.land/bubbles/v2/list"` |
| `"github.com/charmbracelet/bubbles/progress"` | `"charm.land/bubbles/v2/progress"` |
| `"github.com/charmbracelet/bubbles/spinner"` | `"charm.land/bubbles/v2/spinner"` |
| `"github.com/charmbracelet/bubbles/table"` | `"charm.land/bubbles/v2/table"` |
| `"github.com/charmbracelet/bubbles/textinput"` | `"charm.land/bubbles/v2/textinput"` |
| `"github.com/charmbracelet/bubbles/viewport"` | `"charm.land/bubbles/v2/viewport"` |

- [ ] **Step 2: Replace core library imports**

| Find | Replace |
|------|---------|
| `"github.com/charmbracelet/bubbletea"` | `"charm.land/bubbletea/v2"` |
| `"github.com/charmbracelet/lipgloss"` | `"charm.land/lipgloss/v2"` |
| `"github.com/charmbracelet/huh"` | `"charm.land/huh/v2"` |

Note: The `tea` alias (`tea "..."`) is preserved — only the path changes.

- [ ] **Step 3: Add `image/color` import where needed**

Files that use `lipgloss.Color` as a **type** (not a function call) need `"image/color"` added to their imports. These files declare variables, struct fields, or function signatures with `lipgloss.Color` as the type:

- `internal/tui/styles.go` — `StatusColor()` return type, color var declarations
- `internal/tui/app_layout.go` — `feedbackColor()` return type
- `internal/tui/text_input.go` — `renderInactiveInput()` param, `renderTimeInputPair()` param
- `internal/tui/settings.go` — `secMessageColor` field
- `internal/tui/job_details.go` — `detailRow.color` field, `chatStatusColor()` return, `addFieldColor()` param
- `internal/tui/add_video.go` — `var borderColor lipgloss.Color`
- `internal/tui/log_viewer.go` — `logLineColor()` return type
- `internal/tui/settings_view.go` — inline `func() lipgloss.Color` closure

- [ ] **Step 4: Remove old V1 modules**

```bash
go mod tidy
```

This removes the old `github.com/charmbracelet/*` entries from `go.mod` and `go.sum`.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/tui/
git commit -m "refactor(tui): update Charm import paths to V2 (charm.land/*)"
```

---

### Task 3: Migrate `lipgloss.Color` Type Usage

In V1, `lipgloss.Color` is a type (`type Color string`). In V2, it's a function (`func Color(string) color.Color`). The function call syntax `lipgloss.Color("#ff00ff")` stays the same, but all **type annotations** must change from `lipgloss.Color` to `color.Color`.

**Files:**
- Modify: `internal/tui/styles.go`
- Modify: `internal/tui/app_layout.go`
- Modify: `internal/tui/text_input.go`
- Modify: `internal/tui/settings.go`
- Modify: `internal/tui/job_details.go`
- Modify: `internal/tui/add_video.go`
- Modify: `internal/tui/log_viewer.go`
- Modify: `internal/tui/settings_view.go`

- [ ] **Step 1: Update `styles.go` color variable declarations and return type**

The `var` block at lines 12-35 uses inferred types from `lipgloss.Color(...)` calls. In V2 these will infer to `color.Color` automatically — no change needed for the declarations themselves.

Change the `StatusColor` function return type:

```go
// Before (line 116)
func StatusColor(status string) lipgloss.Color {

// After
func StatusColor(status string) color.Color {
```

- [ ] **Step 2: Update `app_layout.go` feedbackColor return type**

```go
// Before (line 135)
func feedbackColor(msg string) lipgloss.Color {

// After
func feedbackColor(msg string) color.Color {
```

- [ ] **Step 3: Update `text_input.go` parameter types**

```go
// Before (line 62)
func renderInactiveInput(value string, w int, color lipgloss.Color) string {

// After
func renderInactiveInput(value string, w int, c color.Color) string {
```

Note: Also rename the parameter from `color` to `c` to avoid shadowing the `color` import. Update usage on line 67: `.Foreground(color)` → `.Foreground(c)`.

```go
// Before (line 137)
	accentColor lipgloss.Color,

// After
	accentColor color.Color,
```

- [ ] **Step 4: Update `settings.go` struct field type**

```go
// Before (line 259)
	secMessageColor  lipgloss.Color

// After
	secMessageColor  color.Color
```

- [ ] **Step 5: Update `job_details.go` types**

```go
// Before (line 56)
	color lipgloss.Color

// After
	color color.Color
```

```go
// Before (line 590)
func (m *JobDetailsModel) chatStatusColor(status string) lipgloss.Color {

// After
func (m *JobDetailsModel) chatStatusColor(status string) color.Color {
```

```go
// Before (line 608)
func (m *JobDetailsModel) addFieldColor(label, value string, color lipgloss.Color) {

// After
func (m *JobDetailsModel) addFieldColor(label, value string, c color.Color) {
```

Note: Rename `color` param to `c` to avoid shadowing. Update usages of `color` inside the function body.

- [ ] **Step 6: Update `add_video.go` variable type**

```go
// Before (line 584)
	var borderColor lipgloss.Color

// After
	var borderColor color.Color
```

- [ ] **Step 7: Update `log_viewer.go` return type**

```go
// Before (line 242)
func logLineColor(line string) lipgloss.Color {

// After
func logLineColor(line string) color.Color {
```

- [ ] **Step 8: Update `settings_view.go` inline closure**

```go
// Before (line 533)
			lines = append(lines, lipgloss.NewStyle().Foreground(func() lipgloss.Color {

// After
			lines = append(lines, lipgloss.NewStyle().Foreground(func() color.Color {
```

- [ ] **Step 9: Commit**

```bash
git add internal/tui/
git commit -m "refactor(tui): migrate lipgloss.Color type annotations to color.Color"
```

---

### Task 4: Migrate View() Signatures and Program Options

In V2, `View()` returns `tea.View` instead of `string`. Program options like `tea.WithAltScreen()` and `tea.WithMouseCellMotion()` move to View fields. The command `tea.SetWindowTitle()` also becomes a View field.

**Files:**
- Modify: `internal/tui/app_layout.go` — Main `App.View()`, the only one that sets AltScreen/Mouse/Title
- Modify: `internal/tui/app_commands.go` — `Run()` function, remove program options
- Modify: `internal/tui/app.go` — `updateTerminalTitle()` return type change
- Modify: All 15 other files with `View() string` methods

**Key design decision:** Only `App.View()` in `app_layout.go` is the top-level tea.Model view. All other `View()` methods are on sub-components that return strings which get composed into the main view. These sub-component Views should return `string` (they are NOT tea.Model implementations themselves). Only `App.View()` needs to return `tea.View`.

- [ ] **Step 1: Update `App.View()` in `app_layout.go` to return `tea.View`**

```go
// Before (line 57)
func (a *App) View() string {
	if a.width == 0 || a.height == 0 {
		return "Initializing..."
	}
	// ... (entire body remains the same, building a `content` string)
	return content
}

// After
func (a *App) View() tea.View {
	if a.width == 0 || a.height == 0 {
		return tea.NewView("Initializing...")
	}
	// ... (entire body remains the same, building a `content` string)
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = a.windowTitle
	return v
}
```

- [ ] **Step 2: Remove program options from `Run()` in `app_commands.go`**

```go
// Before (lines 462-465)
	p := tea.NewProgram(app,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

// After
	p := tea.NewProgram(app)
```

- [ ] **Step 3: Convert `updateTerminalTitle()` to set a field instead of returning a command**

The `tea.SetWindowTitle()` command no longer exists in V2. Instead, window title is a View field. Change `updateTerminalTitle()` to store the title in a field on `App`:

Add a `windowTitle` field to the App struct (in `app.go`):

```go
// Add to App struct fields
	windowTitle string
```

Then change the function:

```go
// Before (app.go lines 545-565)
func (a *App) updateTerminalTitle() tea.Cmd {
	// ... builds title string ...
	return tea.SetWindowTitle(title)
}

// After
func (a *App) updateTerminalTitle() {
	// ... builds title string (same logic) ...
	a.windowTitle = title
}
```

Update all call sites of `updateTerminalTitle()`:

In `app_update.go` line 38:
```go
// Before
return a, tea.Batch(a.updateTerminalTitle(), a.tick())
// After
a.updateTerminalTitle()
return a, a.tick()
```

In `app_update.go` line 92-93 (`handleJobUpdate` returns a `tea.Cmd`):
```go
// Before
func (a *App) handleJobUpdate(job *database.Job) tea.Cmd {
	// ...
	return a.updateTerminalTitle()
}
// (call site at line 93)
titleCmd := a.handleJobUpdate(msg.Job)
return a, tea.Batch(titleCmd, a.listenForUpdates())

// After
func (a *App) handleJobUpdate(job *database.Job) {
	// ...
	a.updateTerminalTitle()
}
// (call site)
a.handleJobUpdate(msg.Job)
return a, a.listenForUpdates()
```

Check for **all** call sites of `handleJobUpdate` and `updateTerminalTitle` and update them similarly — the function no longer returns a `tea.Cmd`. Known additional call sites:

- `app_update.go` ~line 127: `return a, tea.Batch(a.updateTerminalTitle(), a.listenForUpdates())` → `a.updateTerminalTitle(); return a, a.listenForUpdates()`
- Any `return a.updateTerminalTitle()` inside `handleJobUpdate` → `a.updateTerminalTitle()` (void, no return)

Grep for `updateTerminalTitle` and `handleJobUpdate` to find all call sites.

- [ ] **Step 4: Verify sub-component View() methods**

The other 15 `View() string` methods (ActionMenuModel, AddVideoModel, etc.) are NOT tea.Model implementations — they're helper methods that return string content composed into the main App.View(). **Leave them as `View() string`** — they don't need to change.

Verify this by checking that `App` is the only type passed to `tea.NewProgram()`. It should be the sole `tea.Model`.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "refactor(tui): migrate View() to tea.View, move program options to view fields"
```

---

### Task 5: Migrate Key and Mouse Handling

**Files:**
- Modify: `internal/tui/app_update.go` — `tea.KeyMsg` → `tea.KeyPressMsg`, `tea.MouseMsg` interface
- Modify: `internal/tui/app_keys.go` — `handleKey(msg tea.KeyMsg)` parameter type
- Modify: `internal/tui/app_mouse.go` — `handleMouse()` parameter type
- Modify: `internal/tui/mouse.go` — Helper functions parameter types and mouse button constants
- Modify: `internal/tui/files_dialog.go` — `HandleKey(msg tea.KeyMsg)` parameter type
- Modify: `internal/tui/client_tokens_dialog.go` — `HandleKey(msg tea.KeyMsg)` parameter type
- Modify: `internal/tui/settings_components.go` — `msg.(tea.KeyMsg)` type assertion
- Modify: `internal/tui/ffmpeg_check.go` — `msg.(tea.KeyMsg)` type assertion

- [ ] **Step 1: Update `app_update.go` key/mouse type switches**

```go
// Before (line 453)
	case tea.KeyMsg:

// After
	case tea.KeyPressMsg:
```

```go
// Before (line 464)
	case tea.MouseMsg:

// After — tea.MouseMsg is now an interface; still works for type switch
// but handleMouse needs the concrete type
	case tea.MouseClickMsg:
		return a.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		return a.handleMouseWheel(msg)
```

Wait — the current `handleMouse` does both clicks and scrolls. In V2, mouse messages are split into concrete types. We need to handle this refactor carefully.

**Approach:** Keep `tea.MouseMsg` in the type switch (it's still an interface in V2, so it matches all mouse events). Update `handleMouse` and the helper functions to use the V2 interface methods.

```go
// Before (line 464)
	case tea.MouseMsg:
		return a.handleMouse(msg)

// After — tea.MouseMsg is an interface in V2, still matches all mouse events
	case tea.MouseMsg:
		return a.handleMouse(msg)
```

- [ ] **Step 2: Update `mouse.go` helper functions**

```go
// Before (lines 23-35)
func isScrollUp(msg tea.MouseMsg) bool {
	return msg.Button == tea.MouseButtonWheelUp
}
func isScrollDown(msg tea.MouseMsg) bool {
	return msg.Button == tea.MouseButtonWheelDown
}
func isLeftClick(msg tea.MouseMsg) bool {
	return msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress
}

// After — use V2 concrete types for type-based dispatch
func isScrollUp(msg tea.MouseMsg) bool {
	if wm, ok := msg.(tea.MouseWheelMsg); ok {
		return wm.Button == tea.MouseWheelUp
	}
	return false
}
func isScrollDown(msg tea.MouseMsg) bool {
	if wm, ok := msg.(tea.MouseWheelMsg); ok {
		return wm.Button == tea.MouseWheelDown
	}
	return false
}
func isLeftClick(msg tea.MouseMsg) bool {
	if cm, ok := msg.(tea.MouseClickMsg); ok {
		return cm.Button == tea.MouseLeft
	}
	return false
}
```

- [ ] **Step 3: Update `app_mouse.go` coordinate access**

In V2, mouse coordinates are accessed via `msg.Mouse()` method instead of direct fields:

```go
// Before (line 16)
	x, y := msg.X, msg.Y

// After
	m := msg.Mouse()
	x, y := m.X, m.Y
```

The function signature stays `handleMouse(msg tea.MouseMsg)` — the interface is the same name, just an interface now.

- [ ] **Step 4: Update all `tea.KeyMsg` function signatures and type assertions**

These function signatures take `tea.KeyMsg` as a parameter type — all must become `tea.KeyPressMsg`:

```go
// app_keys.go line 14
// Before: func (a *App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
// After:
func (a *App) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {

// files_dialog.go line 238 — change tea.KeyMsg param to tea.KeyPressMsg
// (keep original return type — verify exact signature in the file)
func (m *FilesDialogModel) HandleKey(msg tea.KeyPressMsg) ...

// client_tokens_dialog.go line 224 — change tea.KeyMsg param to tea.KeyPressMsg
func (m *ClientTokensDialogModel) HandleKey(msg tea.KeyPressMsg) ...
```

These type assertions also need updating:

```go
// settings_components.go line 83
// Before: if keyMsg, ok := msg.(tea.KeyMsg); ok {
// After:
if keyMsg, ok := msg.(tea.KeyPressMsg); ok {

// ffmpeg_check.go line 166
// Before: if keyMsg, ok := msg.(tea.KeyMsg); ok {
// After:
if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
```

Grep for any remaining `tea.KeyMsg` across all TUI files and update. Every instance must become `tea.KeyPressMsg`.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "refactor(tui): migrate key and mouse handling to V2 types"
```

---

### Task 6: Migrate Viewport Constructor and Field Access

In V2, `viewport.New(w, h)` → `viewport.New(viewport.WithWidth(w), viewport.WithHeight(h))`. Width/Height/YOffset fields become getter/setter methods.

**Files:**
- Modify: `internal/tui/job_details.go`
- Modify: `internal/tui/log_viewer.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/ffmpeg_check.go`

- [ ] **Step 1: Update viewport constructors**

```go
// job_details.go line 61
// Before: vp := viewport.New(0, 1)
// After:
vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(1))

// log_viewer.go line 56
// Before: vp := viewport.New(0, 1)
// After:
vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(1))

// help.go line 102
// Before: vp := viewport.New(0, 0)
// After:
vp := viewport.New(viewport.WithWidth(0), viewport.WithHeight(0))

// ffmpeg_check.go line 385
// Before: m.reviewViewport = viewport.New(vpW, 6)
// After:
m.reviewViewport = viewport.New(viewport.WithWidth(vpW), viewport.WithHeight(6))
```

- [ ] **Step 2: Update viewport Width/Height field writes → SetWidth/SetHeight**

```go
// log_viewer.go lines 97-98
// Before:
m.viewport.Width = w - 2
m.viewport.Height = contentH
// After:
m.viewport.SetWidth(w - 2)
m.viewport.SetHeight(contentH)

// job_details.go lines 127-128
// Before:
m.viewport.Width = w - 2
m.viewport.Height = contentH
// After:
m.viewport.SetWidth(w - 2)
m.viewport.SetHeight(contentH)

// ffmpeg_check.go lines 137-138
// Before:
m.reviewViewport.Width = contentW - 4
m.reviewViewport.Height = ...
// After:
m.reviewViewport.SetWidth(contentW - 4)
m.reviewViewport.SetHeight(...)
```

Also check ffmpeg_check.go line 575 for additional Width assignments.

```go
// help.go lines 164-165
// Before:
m.viewport.Width = w
m.viewport.Height = h - 1
// After:
m.viewport.SetWidth(w)
m.viewport.SetHeight(h - 1)
```

- [ ] **Step 3: Update viewport field reads (Width, Height, YOffset)**

Search for `m.viewport.Width`, `m.viewport.Height`, or `m.viewport.YOffset` used as reads (not assignments). These become method calls:

```go
// job_details.go line 115 (and any other YOffset reads)
// Before: m.viewport.YOffset
// After: m.viewport.YOffset()

// Any Width/Height reads:
// Before: m.viewport.Width
// After: m.viewport.Width()
```

- [ ] **Step 4: Verify `SetContent` and `SetYOffset` still work**

`viewport.SetContent(string)` is unchanged in V2.
`viewport.SetYOffset(int)` is unchanged in V2 (was already a setter in V1).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "refactor(tui): migrate viewport to V2 constructors and setter methods"
```

---

### Task 7: Migrate TextInput Width and Styles

In V2, `textinput.Width` field → `SetWidth()`/`Width()` methods. Style fields move to `Styles` struct. Cursor access changes.

**Files:**
- Modify: `internal/tui/text_input.go`
- Modify: `internal/tui/add_video.go`
- Modify: `internal/tui/ffmpeg_check.go`
- Modify: `internal/tui/settings_view.go` (5 Width assignments)
- Modify: `internal/tui/setup_wizard.go` (1 Width assignment)
- Modify: `internal/tui/settings_components.go` (check for textinput usage)

- [ ] **Step 1: Update `text_input.go` newTextInput() function**

```go
// Before (lines 24-31)
func newTextInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Cursor.SetMode(cursor.CursorStatic)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(ColorCyan)
	ti.TextStyle = lipgloss.NewStyle().Foreground(ColorCyan)
	return ti
}

// After — Cursor is now a method returning *tea.Cursor, style fields moved
func newTextInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	// V2: cursor is managed via tea.Cursor on the View, or via VirtualCursor
	// Set virtual cursor mode (closest to V1 static cursor)
	ti.SetVirtualCursor(true)
	s := ti.Styles()
	s.Focused.Text = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Focused.Cursor = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Blurred.Text = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Blurred.Cursor = lipgloss.NewStyle().Foreground(ColorCyan)
	ti.SetStyles(s)
	return ti
}
```

Note: The `cursor` subpackage import can be removed from `text_input.go` since `cursor.CursorStatic` is no longer used. V2 uses `VirtualCursor` instead.

- [ ] **Step 2: Update textinput Width assignments**

```go
// text_input.go line 159
// Before: ti.Width = w - 12
// After:
ti.SetWidth(w - 12)

// add_video.go line 632
// Before: m.textInput.Width = w - 2
// After:
m.textInput.SetWidth(w - 2)

// ffmpeg_check.go line 148
// Before: m.textInput.Width = contentW - 2
// After:
m.textInput.SetWidth(contentW - 2)
```

**settings_view.go** (5 occurrences — grep for `textInput.Width =` or `ti.Width =`):
```go
// settings_view.go lines ~191, ~402, ~492, ~659, ~701
// Before: m.textInput.Width = contentW - N
// After:
m.textInput.SetWidth(contentW - N)
```

**setup_wizard.go** (1 occurrence):
```go
// setup_wizard.go line ~494
// Before: m.textInput.Width = contentW - N
// After:
m.textInput.SetWidth(contentW - N)
```

- [ ] **Step 3: Update textinput TextStyle/Cursor.Style in `add_video.go`**

```go
// add_video.go lines 630-631
// Before:
m.textInput.TextStyle = lipgloss.NewStyle().Foreground(color)
m.textInput.Cursor.Style = lipgloss.NewStyle().Foreground(color)
// After:
s := m.textInput.Styles()
s.Focused.Text = lipgloss.NewStyle().Foreground(c)
s.Focused.Cursor = lipgloss.NewStyle().Foreground(c)
m.textInput.SetStyles(s)
```

- [ ] **Step 4: Update textinput style assignments in `text_input.go` renderTimeInputPair**

```go
// Before (lines 157-158)
	ti.TextStyle = lipgloss.NewStyle().Foreground(ColorCyan)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(ColorCyan)

// After
	s := ti.Styles()
	s.Focused.Text = lipgloss.NewStyle().Foreground(ColorCyan)
	s.Focused.Cursor = lipgloss.NewStyle().Foreground(ColorCyan)
	ti.SetStyles(s)
```

- [ ] **Step 5: Check `settings_components.go` for textinput patterns**

Read the file and update any textinput width/style assignments using the same patterns.

- [ ] **Step 6: Verify EchoMode and Prompt field access**

The codebase sets `ti.EchoMode = textinput.EchoPassword` and `ti.Prompt = ""` directly. Verify these fields still exist as direct assignments in V2. If they've become setter methods, update all occurrences (7+ across `settings_components.go`, `setup_wizard.go`, `text_input.go`).

- [ ] **Step 7: Commit**

```bash
git add internal/tui/
git commit -m "refactor(tui): migrate textinput to V2 styles and setter methods"
```

---

### Task 8: Migrate Spinner, Progress, List, and Table

**Files:**
- Modify: `internal/tui/text_input.go` — spinner (newSpinner)
- Modify: `internal/tui/app_keys.go` — `spinner.Tick` references (6 occurrences)
- Modify: `internal/tui/add_video.go` — spinner.Tick
- Modify: `internal/tui/import_dialog.go` — spinner.Tick
- Modify: `internal/tui/files_dialog.go` — spinner.Tick
- Modify: `internal/tui/client_tokens_dialog.go` — spinner.Tick
- Modify: `internal/tui/job_details.go` — progress Width
- Modify: `internal/tui/trim_dialog.go` — progress Width
- Modify: `internal/tui/task_list.go` — list construction
- Modify: `internal/tui/action_menu.go` — list construction
- Modify: `internal/tui/styles.go` — table styles, huh theme

- [ ] **Step 1: spinner.Tick — method is now on model, not package**

In V2, `spinner.Tick` (package-level function) → `model.Tick()` (method). Our code already uses `m.spinner.Tick` (accessing the field on the model struct), which in V1 was actually the `Tick` *field* on the spinner.Model. In V2, `Tick()` is a method.

Check if V1 `spinner.Tick` is a function reference (used as `tea.Cmd`) or a method. In our codebase, usage looks like `a.ffmpegCheck.spinner.Tick` — this is referencing the `Tick` cmd field. In V2, call it as a method:

```go
// Before — referencing spinner.Tick field as a tea.Cmd value
a.ffmpegCheck.spinner.Tick

// After — calling Tick() method which returns tea.Cmd
a.ffmpegCheck.spinner.Tick()
```

Update all 10 occurrences in `app_keys.go` and 4 occurrences in dialog SpinnerInit methods:

**app_keys.go** (6 occurrences):
- Line 68: `a.ffmpegCheck.spinner.Tick` → `a.ffmpegCheck.spinner.Tick()`
- Line 71: `a.ffmpegCheck.spinner.Tick` → `a.ffmpegCheck.spinner.Tick()`
- Line 86: `a.ffmpegCheck.spinner.Tick` → `a.ffmpegCheck.spinner.Tick()`
- Line 152: `a.setupWiz.spinner.Tick` → `a.setupWiz.spinner.Tick()`
- Line 218: `a.trimDlg.spinner.Tick` → `a.trimDlg.spinner.Tick()`
- Line 228: `a.trimDlg.spinner.Tick` → `a.trimDlg.spinner.Tick()`

**Dialog SpinnerInit methods** (4 occurrences):
- `add_video.go:568`: `return m.spinner.Tick` → `return m.spinner.Tick()`
- `import_dialog.go:189`: `return m.spinner.Tick` → `return m.spinner.Tick()`
- `files_dialog.go:234`: `return m.spinner.Tick` → `return m.spinner.Tick()`
- `client_tokens_dialog.go:221`: `return m.spinner.Tick` → `return m.spinner.Tick()`

- [ ] **Step 2: Progress bar Width → SetWidth**

```go
// trim_dialog.go line 511
// Before: m.progressBar.Width = barWidth
// After:
m.progressBar.SetWidth(barWidth)

// job_details.go line 697
// Before: m.progress.Width = barW
// After:
m.progress.SetWidth(barW)
```

- [ ] **Step 3: List construction — no API change needed for basic usage**

`list.New(items, delegate, width, height)` constructor is the same in V2. However, `list.DefaultStyles()` now requires an `isDark bool` parameter:

```go
// If any file calls list.DefaultStyles(), add isDark:
// Before: list.DefaultStyles()
// After: list.DefaultStyles(true)  // Moombox always uses dark theme
```

Check if our code calls `list.DefaultStyles()` or `list.NewDefaultItemStyles()` anywhere. If so, add `true` as the parameter. Our task_list.go and action_menu.go construct custom delegates — verify whether they call these functions.

- [ ] **Step 4: Table DefaultStyles — check for changes**

```go
// styles.go line 100
// Before: s := table.DefaultStyles()
// After: s := table.DefaultStyles()  // unchanged in V2
```

Table `DefaultStyles()` does NOT take an `isDark` parameter in V2. No change needed.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/
git commit -m "refactor(tui): migrate spinner, progress, list, and table to V2 APIs"
```

---

### Task 9: Migrate huh Theme

In V2, `huh.ThemeBase()` → `huh.ThemeBase(isDark bool)` and returns `*huh.Styles` (same type, but function signature changed). Theme struct fields may have changed names.

**Files:**
- Modify: `internal/tui/styles.go` — `moomboxTheme()` function
- Modify: `internal/tui/setup_wizard.go` — any direct huh form/theme usage

- [ ] **Step 1: Update moomboxTheme() in styles.go**

```go
// Before (line 62)
	t := huh.ThemeBase()

// After
	t := huh.ThemeBase(true) // true = dark background (Moombox always dark)
```

After this change, verify that all the field paths on `t` still exist in V2:
- `t.Focused.Base`, `t.Focused.Title`, `t.Focused.Description` — check V2 Styles struct
- `t.Focused.TextInput.Cursor`, `t.Focused.TextInput.CursorText`, etc.
- `t.Focused.FocusedButton`, `t.Focused.BlurredButton`
- `t.Blurred` (copy of Focused)
- `t.Group.Title`, `t.Group.Description`

If any fields have been renamed or restructured in V2, update accordingly. The V2 upgrade guide notes that `Theme` became a `ThemeFunc` type (`func(isDark bool) *Styles`), but `ThemeBase()` already returns `*Styles`, so `moomboxTheme()` can keep returning `*huh.Theme` or whatever the V2 type is.

- [ ] **Step 2: Check setup_wizard.go huh form usage**

Read setup_wizard.go for any `.WithTheme()` calls and update them if they pass a theme function:

```go
// Before
.WithTheme(moomboxTheme())

// After — if WithTheme now expects ThemeFunc:
.WithTheme(func(isDark bool) *huh.Styles { return moomboxTheme() })
// OR if it still accepts *Styles:
.WithTheme(moomboxTheme())
```

The exact change depends on V2's `WithTheme` signature. Check by attempting compilation.

- [ ] **Step 3: Commit**

```bash
git add internal/tui/
git commit -m "refactor(tui): migrate huh theme to V2 API"
```

---

### Task 10: Handle Remaining Bubbles API Changes

Catch-all task for any remaining V2 API changes discovered during compilation.

**Files:**
- Modify: Any files flagged by compiler errors

- [ ] **Step 1: Remove `cursor` import if no longer needed**

If `cursor.CursorStatic` was the only usage of `charm.land/bubbles/v2/cursor`, remove the import from `text_input.go`.

- [ ] **Step 2: Check `key.NewBinding` patterns**

`key.NewBinding()` API is unchanged in V2. The `key` package bindings should work as-is. Verify `help.go` viewport key map compiles.

- [ ] **Step 3: Check list filter style fields**

If any code accesses `list.Styles.FilterPrompt` or `list.Styles.FilterCursor`, these moved to `list.Styles.Filter.Focused.Prompt` / `list.Styles.Filter.Cursor` in V2. Grep for these patterns.

- [ ] **Step 4: Verify Paginator field access**

`action_menu.go` and `task_list.go` access `m.list.Paginator.PerPage` and `m.list.Paginator.Page` directly (8+ occurrences). Verify these fields still exist in V2 — if they became getter/setter methods, update all occurrences.

- [ ] **Step 5: Verify filepicker API**

`import_dialog.go` uses `filepicker.New()` and `filepicker.Model`. Verify the constructor and Height field access patterns still compile in V2. Height likely became `SetHeight()`/`Height()`.

- [ ] **Step 6: Handle any other compilation errors**

Run `go build ./internal/tui/...` and fix any remaining type mismatches, renamed APIs, or missing methods not covered by previous tasks.

---

### Task 11: Build Verification and Testing

- [ ] **Step 1: Attempt build**

```bash
cd D:/Git/Moombox
go build ./...
```

Fix any compilation errors that arise. Common issues:
- Missing `"image/color"` imports
- Remaining `lipgloss.Color` type usages not yet converted
- Changed method signatures on bubbles components
- Renamed constants or functions

- [ ] **Step 2: Run tests**

```bash
go test ./...
```

All existing tests should pass unchanged. The TUI tests (if any) may need updates for changed mock types.

- [ ] **Step 3: Run vet**

```bash
go vet ./...
```

- [ ] **Step 4: Manual smoke test**

Build and run the binary to verify:
- TUI launches in alt screen
- Mouse clicks and scrolling work
- Keyboard chords work
- Window title updates
- All dialogs render correctly
- Spinner animations work
- Progress bars display
- Colors look correct

```bash
go build -o moombox.exe ./cmd/moombox
./moombox.exe
```

- [ ] **Step 5: Final commit**

```bash
git add .
git commit -m "refactor(tui): complete Charm V2 migration — bubbletea, bubbles, lipgloss, huh"
```

---

## Appendix: Complete Migration Reference

### Import Path Mapping

| V1 | V2 |
|----|-----|
| `github.com/charmbracelet/bubbletea` | `charm.land/bubbletea/v2` |
| `github.com/charmbracelet/lipgloss` | `charm.land/lipgloss/v2` |
| `github.com/charmbracelet/huh` | `charm.land/huh/v2` |
| `github.com/charmbracelet/bubbles/*` | `charm.land/bubbles/v2/*` |

### Type Changes

| V1 Type | V2 Type |
|---------|---------|
| `lipgloss.Color` (type) | `color.Color` (from `image/color`) |
| `lipgloss.TerminalColor` | `color.Color` |
| `tea.KeyMsg` (struct) | `tea.KeyPressMsg` (for key presses) |
| `tea.MouseMsg` (struct) | `tea.MouseMsg` (interface) |
| `tea.MouseEvent` | `tea.Mouse` |

### Button Constant Renames

| V1 | V2 |
|----|-----|
| `tea.MouseButtonLeft` | `tea.MouseLeft` |
| `tea.MouseButtonRight` | `tea.MouseRight` |
| `tea.MouseButtonMiddle` | `tea.MouseMiddle` |
| `tea.MouseButtonWheelUp` | `tea.MouseWheelUp` |
| `tea.MouseButtonWheelDown` | `tea.MouseWheelDown` |

### Removed APIs → Replacements

| Removed | Replacement |
|---------|-------------|
| `tea.WithAltScreen()` | `view.AltScreen = true` |
| `tea.WithMouseCellMotion()` | `view.MouseMode = tea.MouseModeCellMotion` |
| `tea.SetWindowTitle(s)` | `view.WindowTitle = s` |
| `tea.EnterAltScreen` | `view.AltScreen = true` |
| `tea.MouseActionPress` | Use `tea.MouseClickMsg` type instead |
| `viewport.New(w, h)` | `viewport.New(viewport.WithWidth(w), viewport.WithHeight(h))` |
| `vp.Width = x` | `vp.SetWidth(x)` |
| `vp.Height = y` | `vp.SetHeight(y)` |
| `ti.Width = x` | `ti.SetWidth(x)` |
| `ti.TextStyle = s` | `styles.Focused.Text = s; ti.SetStyles(styles)` |
| `ti.Cursor.Style = s` | `styles.Focused.Cursor = s; ti.SetStyles(styles)` |
| `ti.Cursor.SetMode(...)` | `ti.SetVirtualCursor(true)` |
| `spinner.Tick` (field) | `spinner.Tick()` (method call) |
| `progress.Width = x` | `progress.SetWidth(x)` |
| `huh.ThemeBase()` | `huh.ThemeBase(isDark)` |

### Files Impact Summary

| File | Changes |
|------|---------|
| `styles.go` | Imports, color type, StatusColor return, huh theme, table styles |
| `app_layout.go` | Import, View() → tea.View, feedbackColor return type |
| `app_update.go` | Import, KeyMsg → KeyPressMsg |
| `app_commands.go` | Import, remove program options from Run() |
| `app.go` | Import, add windowTitle field, change updateTerminalTitle |
| `app_keys.go` | Import, spinner.Tick → Tick() (6 places) |
| `app_mouse.go` | Import, mouse coordinate access |
| `mouse.go` | Import, mouse helper functions rewrite |
| `text_input.go` | Import, newTextInput cursor/style, Width setter, color param type |
| `job_details.go` | Import, viewport, progress, color types |
| `log_viewer.go` | Import, viewport, color return type |
| `help.go` | Import, viewport constructor |
| `ffmpeg_check.go` | Import, viewport, textinput Width |
| `add_video.go` | Import, spinner.Tick, textinput Width, color type |
| `trim_dialog.go` | Import, progress Width, spinner.Tick |
| `setup_wizard.go` | Import, huh theme, spinner.Tick |
| `settings.go` | Import, color field type |
| `settings_view.go` | Import, color closure type |
| `settings_components.go` | Import, textinput patterns |
| `action_menu.go` | Import, list construction |
| `task_list.go` | Import, list construction |
| `files_dialog.go` | Import, spinner.Tick, list |
| `client_tokens_dialog.go` | Import, spinner.Tick, list |
| `import_dialog.go` | Import, spinner.Tick |
| `status_bar.go` | Import |
| `marquee.go` | Import (if applicable) |
