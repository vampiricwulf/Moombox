# TUI Charm V2 Audit — Design Spec

**Date:** 2026-03-20
**Scope:** Adopt unused Bubble Tea / Bubbles / Lipgloss V2 features; update skill file

## Context

Moombox upgraded to Charm V2 (bubbletea/v2 v2.0.2, bubbles/v2 v2.0.0, lipgloss/v2 v2.0.2, huh/v2 v2.0.3). All imports and constructors were migrated, but several V2-exclusive features remain unused. The `moombox-charm-suite` skill file still documents V1 patterns.

### Audit Findings

**Already V2-complete:** Declarative `tea.View` on root model, `BackgroundColorMsg` detection with `isDark` propagation, V2 bubbles constructors (functional options, `SetStyles()`, `SetWidth()`), huh forms with `ThemeFunc`/`MapAccessor`/generics, viewport `StyleLineFunc` and `SetHighlights()` in log viewer.

**Assessed and rejected:**
- *Settings huh forms* — Current manual renderer has change indicators, inline editing, toggle/cycle rendering, preview functions that huh can't replicate. Purpose-built and superior.
- *StyleLineFunc on job_details* — StyleLineFunc applies one style per line; job_details needs mixed label/value colors within each line. Current pre-rendering is correct.
- *SoftWrap on job_details* — `wrapText()` intentionally wraps to value-column width for alignment. Viewport SoftWrap would break this.
- *Inline progress bars in task list* — At `Height(1)` items, a progress bar is less information-dense than percentage text.
- *ReportFocus (FocusMsg/BlurMsg)* — Ticks are lightweight enough that pausing them when unfocused provides negligible benefit.
- *Ecosystem libraries (bubbletea-overlay, additional-bubbles, tree-bubble, skeleton)* — None worth adopting; Moombox's custom patterns are tighter and purpose-built.

## Changes

### 1. Skill File V2 Update

**File:** `.claude/skills/moombox-charm-suite/skill.md`

Update the skill to reflect actual V2 APIs and codebase patterns:

**Import paths:** All references must use `charm.land/*/v2` paths:
- `charm.land/bubbletea/v2` (imported as `tea`)
- `charm.land/bubbles/v2/list`, `viewport`, `textinput`, `spinner`, `progress`, `table`, `filepicker`, `key`
- `charm.land/lipgloss/v2`
- `charm.land/huh/v2`

**Update `cursor` in component catalog:** `bubbles/v2/cursor` still exists (used internally by `textinput` and `textarea` for virtual cursor rendering), but the codebase does not import it directly. Remove it from the "used" list. Note that V2 added `tea.Cursor` / `tea.NewCursor(x, y)` on `tea.View` for hardware terminal cursor positioning — this is separate from the virtual cursor in bubbles.

**Fix huh example:** Change `Value(&accessor)` → `Accessor(&accessor)`. Update `moomboxTheme()` to `moomboxTheme(isDark)` to match the current codebase signature (the code is already correct; only the skill file documentation is stale). Document `SubmitCmd = nil` as a critical pattern — Moombox uses it to prevent huh forms from quitting the program on submit.

**Add V2 Features section** documenting:
- Declarative views: root `View()` returns `tea.View`, subcomponents return `string`. `tea.NewView(content)` with `.AltScreen`, `.MouseMode`, `.WindowTitle` fields.
- Terminal detection: `tea.RequestBackgroundColor` in Init(), `tea.BackgroundColorMsg` with `msg.IsDark()`, `tea.ColorProfileMsg` with `msg.Profile`.
- Color adaptation: `lipgloss.LightDark(isDark)` returns a function selecting colors by terminal background. `colorprofile.TrueColor`, `ANSI256`, `ANSI`, `NoTTY`.
- Viewport V2: `viewport.New(viewport.WithWidth(), viewport.WithHeight())`, `StyleLineFunc` for per-line styling, `LeftGutterFunc` for line numbers, `SetHighlights()` for search, `SetContentLines([]string)`, `FillHeight = true`.
- TextInput V2: `Styles()` / `SetStyles(s)`, `DefaultStyles(isDark)`, `SetWidth()`, `SetCursor(pos)`.
- Focus: `tea.FocusMsg` / `tea.BlurMsg` with `v.ReportFocus = true` for terminal window focus detection.
- Mouse: `tea.MouseClickMsg`, `tea.MouseReleaseMsg`, `tea.MouseWheelMsg`, `tea.MouseMotionMsg` (typed messages replacing generic `tea.MouseMsg`).

**Update all code examples** to match actual codebase patterns with correct V2 constructors.

### 2. Viewport FillHeight + SetContentLines

**Files:** `log_viewer.go`, `job_details.go`, `help.go`

**FillHeight:** Set `vp.FillHeight = true` on all three viewports in their constructors (`NewLogViewerModel`, `NewJobDetailsModel`, `NewHelpModel`). This fills remaining viewport height with empty lines, preventing the panel border from collapsing when content is shorter than the panel.

**SetContentLines:** Replace `strings.Join` + `SetContent` with `SetContentLines` where content is already built as `[]string`:
- `job_details.go:updateViewportContent()` — lines are built in a `[]string` slice then joined. Change to `m.viewport.SetContentLines(lines)`.
- `log_viewer.go:updateViewportContent()` — `m.filtered` is already `[]string`. Change to `m.viewport.SetContentLines(slices.Clone(m.filtered))` (clone to avoid shared backing array — `SetContentLines` stores the slice reference, and `m.filtered` may be mutated by `rebuildFiltered()`).

**Not changed:** `help.go` builds content as a single string with `strings.Builder`, so `SetContent` remains appropriate there.

### 3. ColorProfileMsg Detection

**File:** `app_update.go`, `app.go`

Add `colorProfile` field to `App` struct (type `colorprofile.Profile`, default `colorprofile.TrueColor`).

Handle `tea.ColorProfileMsg` in the Update switch. Note: `ColorProfileMsg` is sent automatically by bubbletea on program startup (no request needed, unlike `tea.RequestBackgroundColor`):
```go
case tea.ColorProfileMsg:
    a.colorProfile = msg.Profile
```

This provides the plumbing for future adaptive color decisions. No immediate visual changes — the profile is stored and available for style functions that may later need it.

**Import:** `github.com/charmbracelet/colorprofile` v0.4.2 (already an indirect dependency). Profile constants: `colorprofile.TrueColor`, `colorprofile.ANSI256`, `colorprofile.ANSI`, `colorprofile.Ascii`, `colorprofile.NoTTY`.

### 4. FFmpeg Huh Selects

**File:** `ffmpeg_check.go`

Replace manual keyboard-driven mode selection with huh select forms for cleaner UX:

**Main menu** (currently modes `main` with keybinds I/C/S/Q):
```go
huh.NewSelect[string]().
    Title("FFmpeg Setup").
    Options(
        huh.NewOption("Install FFmpeg (Chocolatey/Winget)", "install"),
        huh.NewOption("Set custom FFmpeg path", "custom"),
        huh.NewOption("Skip (FFmpeg not available)", "skip"),
        huh.NewOption("Quit Moombox", "quit"),
    ).
    Accessor(&MapAccessor{M: m.values, Key: "action"})
```

**Install submenu** (currently mode `install` with keybinds C/W):
```go
huh.NewSelect[string]().
    Title("Install Method").
    Options(
        huh.NewOption("Chocolatey (choco install ffmpeg)", "choco"),
        huh.NewOption("Winget (winget install ffmpeg)", "winget"),
    ).
    Accessor(&MapAccessor{M: m.values, Key: "method"})
```

**What stays the same:**
- Custom path textinput (step `custom`) — huh input doesn't add value over current textinput
- Script review viewport (step `review`) — read-only display
- Manual install instructions (step `manual`) — static text
- Spinner during installation — unchanged

**State machine changes:**
- Remove `mode` string field, replace with huh form state
- Add `values map[string]string` field to `FFmpegCheckModel` for form-backed storage
- Form initialization: create and call `.Init()` on each form when entering `main` or `install` mode (same pattern as `setup_wizard.go` line 366)
- Form update routing: delegate messages to `m.form.Update(msg)` in `UpdateComponents()`, same as setup wizard routes to `m.advancedForm`
- Form completion: check `m.form.State == huh.StateCompleted`, then read `m.values["action"]` or `m.values["method"]` to determine next step. Set `SubmitCmd = nil` to prevent huh from quitting the program.
- Esc from form returns to previous step (or closes dialog)
- **Dynamic install options:** `buildInstallOptions()` currently checks choco/winget availability. Build huh Select options dynamically at form creation time, only including available package managers.

**Theme:** Use `moomboxTheme(m.isDark)` for consistent styling. Requires propagating `isDark` to FFmpegCheckModel (currently available on App).

## Non-Goals

- No color palette overhaul (adaptive colors, LightDark) — deferred to future work
- No structural refactoring (shared dialog base, delegate standardization)
- No new bubbles components (textarea, paginator, stopwatch, timer)
- No external ecosystem dependencies

## Testing

- `go build ./...` — verify compilation
- `go vet ./...` — static analysis
- Manual TUI testing:
  - FillHeight: verify panel borders don't collapse when content is shorter than panel height; viewport padding should fill to the bottom
  - FFmpeg dialog: verify main menu selection, install submenu (only available package managers shown), custom path input, Esc navigation between steps
  - ColorProfileMsg: verify no panics on startup (the handler is passive)
  - No regressions in existing behavior (log viewer search, job details scrolling, help overlay)
