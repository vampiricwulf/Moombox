# TUI V2 Improvements Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Leverage Charm V2's new features to improve the Moombox TUI experience with 8 enhancements.

**Architecture:** Each enhancement is independent and can be implemented/tested separately. All build on the V2 migration already completed.

**Tech Stack:** Go 1.25, charm.land/bubbletea/v2, charm.land/bubbles/v2, charm.land/lipgloss/v2

---

### Task 1: Trivial Enhancements (Sync Output, Real Cursor, Dark Detection)

**Files:**
- Modify: `internal/tui/app_layout.go` — add synchronized output
- Modify: `internal/tui/text_input.go` — remove `SetVirtualCursor(true)` for native cursor
- Modify: `internal/tui/app.go` — add `isDark bool` field, request background color in Init()
- Modify: `internal/tui/app_update.go` — handle `tea.BackgroundColorMsg`
- Modify: `internal/tui/styles.go` — pass isDark to components that need it (huh theme)

- [ ] **Step 1: Enable synchronized output**

In `app_layout.go`, add to the `viewWithMode()` helper:
```go
func (a *App) viewWithMode(content string) tea.View {
    v := tea.NewView(content)
    v.AltScreen = true
    v.MouseMode = tea.MouseModeCellMotion
    v.WindowTitle = a.windowTitle
    // TODO: Check if tea.View has a SyncOutput or similar field for Mode 2026
    return v
}
```
Check the V2 API for the exact field name. It may be automatically enabled by the cursed renderer.

- [ ] **Step 2: Enable real cursor in text inputs**

In `text_input.go`, remove or comment out `ti.SetVirtualCursor(true)` from `newTextInput()`. Real cursor is the V2 default.

- [ ] **Step 3: Detect dark/light background**

In `app.go`, add `isDark bool` field to App (default `true`).

In `Init()`, add `tea.RequestBackgroundColor` to the init commands.

In `app_update.go`, handle the response:
```go
case tea.BackgroundColorMsg:
    a.isDark = msg.IsDark()
```

- [ ] **Step 4: Pass isDark to huh theme**

Update `moomboxTheme()` in `styles.go` to accept `isDark bool` parameter and pass it through to `huh.ThemeBase(isDark)`. Update the call site.

- [ ] **Step 5: Build and test**

```bash
go build ./... && go test ./...
```

- [ ] **Step 6: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): enable sync output, real cursor, and dark/light detection"
```

---

### Task 2: Hyperlinks for URLs and Paths

**Files:**
- Modify: `internal/tui/job_details.go` — add hyperlinks to URL and path fields

- [ ] **Step 1: Identify URL/path fields in job details rendering**

Read `job_details.go` and find where stream URL, channel URL, and output path fields are rendered. These are likely in the `addField`/`addFieldColor` methods or the main render loop.

- [ ] **Step 2: Add hyperlinks using lipgloss V2**

For URL fields (stream URL, channel URL), wrap the rendered value with a hyperlink:
```go
lipgloss.NewStyle().Foreground(someColor).Hyperlink(url).Render(displayText)
```

For file paths (output directory, file name), use `file://` URLs:
```go
lipgloss.NewStyle().Hyperlink("file:///" + filepath.ToSlash(path)).Render(displayText)
```

Non-supporting terminals will show plain text (graceful degradation).

- [ ] **Step 3: Build and test**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): add clickable hyperlinks for URLs and file paths in job details"
```

---

### Task 3: Clipboard Copy Chord (O C)

**Files:**
- Modify: `internal/tui/app.go` — add chord to `buildMenuItems()`
- Modify: `internal/tui/app_actions.go` — add case to `dispatchAction()`

- [ ] **Step 1: Add chord entry in buildMenuItems()**

Follow the existing chord pattern. Add an entry with prefix "O" (Open), action "C" (Copy):
```go
{prefix: "O", action: "C", label: "Copy URL", hint: "Copy stream URL"}
```

Read `app.go` to find `buildMenuItems()` and follow the exact pattern used by other entries.

- [ ] **Step 2: Add dispatch case in dispatchAction()**

In `app_actions.go`, add a case for the "OC" chord:
```go
case "OC":
    if sel := a.taskList.SelectedJob(); sel != nil && sel.URL != "" {
        return a, tea.SetClipboard(sel.URL, "c")
        // "c" = clipboard selection (vs "p" = primary selection)
    }
    a.setFeedback("No URL to copy")
```

Check the exact `tea.SetClipboard` API. The first arg is the content, second may be the clipboard type.

After successful copy, show feedback: `a.setFeedback("Copied: " + sel.URL)`

- [ ] **Step 3: Build and test**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): add O C chord to copy stream URL to clipboard"
```

---

### Task 4: Curly Underlines for Error States

**Files:**
- Modify: `internal/tui/task_list.go` — add curly underline to Error/COOKIES? status items
- Modify: `internal/tui/styles.go` — add error underline style

- [ ] **Step 1: Add error underline style to styles.go**

```go
var ErrorUnderline = lipgloss.NewStyle().
    UnderlineStyle(lipgloss.UnderlineCurly).
    UnderlineColor(ColorError)
```

- [ ] **Step 2: Apply to error status items in task_list.go**

In the task list rendering, when a job has Error or COOKIES? status, apply the curly underline style to the status text or the entire row. Read the rendering code to find the right place.

- [ ] **Step 3: Build and test**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): add curly underlines for error status indicators"
```

---

### Task 5: Soft-Wrapped Log Lines

**Files:**
- Modify: `internal/tui/log_viewer.go` — enable viewport soft wrapping

- [ ] **Step 1: Enable soft wrapping on viewport**

In `log_viewer.go`, after creating or configuring the viewport, enable soft wrapping:
```go
m.viewport.SoftWrap = true
```

Check the V2 viewport API for the exact field/method name. It may be `SetSoftWrap(true)` or a constructor option.

- [ ] **Step 2: Adjust auto-scroll and line count logic**

With soft wrapping, the visible line count may differ from the content line count. Check if the auto-scroll logic (scrolling to bottom when new logs arrive) still works correctly. The viewport should handle this internally, but verify.

- [ ] **Step 3: Build and test**

```bash
go build ./... && go test ./...
```

- [ ] **Step 4: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): enable soft wrapping for log viewer lines"
```

---

### Task 6: Log Search with Viewport Highlighting

**Files:**
- Modify: `internal/tui/log_viewer.go` — add search mode, highlighting, navigation
- Modify: `internal/tui/app_update.go` — route keys to log search when active

UX: Press `/` when log panel is focused → shows search input at top of log panel. Type query, press Enter to search. `n` jumps to next match, `N` (shift+n) jumps to previous. `Esc` clears search. Matches are highlighted in the viewport using V2's regex highlighting.

- [ ] **Step 1: Add search state to LogViewerModel**

```go
type LogViewerModel struct {
    // existing fields...
    searching    bool
    searchInput  textinput.Model
    searchQuery  string
    matchCount   int
}
```

- [ ] **Step 2: Handle `/` key to enter search mode**

When the log viewer is focused and `/` is pressed, set `searching = true`, focus the search input.

- [ ] **Step 3: Handle Enter to execute search**

On Enter, take the search input value, set it as the `searchQuery`, and use the viewport's highlight API:
```go
m.viewport.SetHighlights(regexp.MustCompile(regexp.QuoteMeta(query)))
```

Check the exact V2 viewport highlighting API.

- [ ] **Step 4: Handle n/N for navigation**

```go
// n = next match
m.viewport.HighlightNext()
// N = previous match
m.viewport.HighlightPrevious()
```

- [ ] **Step 5: Handle Esc to clear search**

Clear the search query, remove highlights, hide the search input.

- [ ] **Step 6: Render search bar in View()**

When `searching` is true, render a search input at the top of the log panel:
```
╭─ Logs ──────────────────────────────────╮
│ /search query here                      │
│ log line 1                              │
│ log line 2 with [highlighted] match     │
```

When search has results, show match count in the border or as a suffix.

- [ ] **Step 7: Build and test**

```bash
go build ./... && go test ./...
```

- [ ] **Step 8: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): add vim-style log search with viewport highlighting"
```

---

## Implementation Notes

- All 6 tasks are independent and can be implemented in any order.
- Tasks 1-5 are trivial/small (5-15 min each). Task 6 is medium (~30 min).
- Check V2 API docs (`go doc charm.land/...`) when unsure about exact method names.
- The proposals about "synchronized output" may be automatically handled by BubbleTea V2's cursed renderer — check if there's an explicit opt-in needed.
