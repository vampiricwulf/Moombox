# UX Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement 18 UX improvements across TUI, Web UI, and setup process in 4 themed phases.

**Architecture:** Theme-based phases, each independently shippable. Strict parity between Web UI and TUI where both support the feature. No new API endpoints — batch operations use existing per-job endpoints. No database or config schema changes.

**Tech Stack:** Go (Bubble Tea TUI), vanilla JS + Shoelace v2.16 (Web UI), chi/v5 router, modernc/sqlite

**Spec:** `docs/superpowers/specs/2026-03-18-ux-improvements-design.md`

---

## Phase 1: Setup & Onboarding

### Task 1: Mode Selection Guidance

**Files:**
- Modify: `web/public/index.html:1230-1245` (setup mode cards)
- Modify: `internal/tui/setup_wizard.go:1127-1200` (viewModeSelect)

- [ ] **Step 1: Add recommended badge and subtext to Web UI Quick Setup card**

In `web/public/index.html`, replace the Quick Setup button (lines 1232-1236):

```html
<button class="setup-mode-card" id="setup-mode-quick" tabindex="0">
    <sl-icon name="lightning-charge" style="font-size: 2rem; color: var(--sl-color-primary-600);"></sl-icon>
    <h3>Quick Setup <sl-badge variant="primary" pill>Recommended</sl-badge></h3>
    <p>Best for most users — takes ~2 minutes. Set up cookies and add channels.</p>
</button>
```

Update the Advanced Setup button subtext (lines 1237-1241):

```html
<button class="setup-mode-card" id="setup-mode-advanced" tabindex="0">
    <sl-icon name="gear" style="font-size: 2rem; color: var(--sl-color-neutral-500);"></sl-icon>
    <h3>Advanced Setup</h3>
    <p>Full control over every setting — network, paths, downloads, and more.</p>
</button>
```

- [ ] **Step 2: Add recommended label to TUI mode select**

In `internal/tui/setup_wizard.go`, find `viewModeSelect()` (around line 1127). Locate where the Quick Setup option label is rendered (around line 1137-1150). Append " (recommended)" to the Quick Setup label text. Add a dim subtext line below: "Best for most users — takes ~2 minutes". Add a dim subtext below Advanced Setup: "Full control over every setting".

- [ ] **Step 3: Build and verify visually**

```bash
go build ./...
```

Open Web UI setup wizard and verify the badge appears on Quick Setup. Start TUI and verify the "(recommended)" suffix appears.

- [ ] **Step 4: Commit**

```bash
git add web/public/index.html internal/tui/setup_wizard.go
git commit -m "ux: add recommended badge and descriptive subtext to setup mode selection"
```

---

### Task 2: Cookie Timeout Countdown (Web UI)

**Files:**
- Modify: `web/public/modules/setup.js:278-333` (finishCookieSetup)
- Modify: `web/public/modules/settings.js:1823-1869` (finishAutoCookieSetup)

- [ ] **Step 1: Add countdown to setup.js cookie dialog**

In `web/public/modules/setup.js`, find `finishCookieSetup()` (around line 278). The dialog currently has a spinner and "Please sign in" text. Add a countdown display element and interval:

Before the `setTimeout(() => controller.abort(), 60000)` call (line 288), add:

```javascript
// Countdown display
let remaining = 60;
const countdownEl = document.getElementById("auto-cookie-countdown");
if (countdownEl) {
  countdownEl.textContent = `${remaining}s remaining`;
  countdownEl.style.color = "";
}
const countdownInterval = setInterval(() => {
  remaining--;
  if (countdownEl) {
    countdownEl.textContent = `${remaining}s remaining`;
    if (remaining <= 10) {
      countdownEl.style.color = "var(--sl-color-warning-600)";
    }
  }
  if (remaining <= 0) clearInterval(countdownInterval);
}, 1000);
```

In the `finally` block (around line 330), add:

```javascript
clearInterval(countdownInterval);
```

On timeout error (around line 324), replace the error message with retry/skip options:

```javascript
if (e.name === "AbortError") {
  // Show retry/skip instead of vague error
  const resultEl = document.getElementById("auto-cookie-result");
  if (resultEl) {
    resultEl.innerHTML = `
      <sl-alert variant="warning" open>
        <sl-icon slot="icon" name="clock"></sl-icon>
        Cookie extraction timed out. The browser window may still be open.
      </sl-alert>
      <div style="display: flex; gap: 0.5em; margin-top: 0.75em;">
        <sl-button variant="primary" size="small" id="cookie-retry-btn">Try Again</sl-button>
        <sl-button variant="default" size="small" id="cookie-skip-btn">Skip</sl-button>
      </div>`;
    document.getElementById("cookie-retry-btn")?.addEventListener("click", () => {
      resultEl.innerHTML = "";
      this.startCookieSetup(platform);
    });
    document.getElementById("cookie-skip-btn")?.addEventListener("click", () => {
      document.getElementById("auto-cookie-setup-dialog")?.hide();
    });
  }
}
```

- [ ] **Step 2: Add countdown element to auto-cookie dialog HTML**

In `web/public/index.html`, find the auto-cookie setup dialog (around line 1202-1211). Add a countdown element:

```html
<p id="auto-cookie-countdown" style="text-align: center; font-size: var(--sl-font-size-small); margin-top: 0.5em;"></p>
<div id="auto-cookie-result"></div>
```

- [ ] **Step 3: Apply the same countdown pattern to settings.js**

In `web/public/modules/settings.js`, find `finishAutoCookieSetup()` (around line 1823). Apply the identical countdown + retry/skip pattern. The code structure mirrors setup.js — add the same `remaining`, `countdownInterval`, countdown element updates, and timeout error handling.

- [ ] **Step 4: Build and test**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test: Trigger cookie setup, verify countdown appears and decrements. Let it time out — verify retry/skip buttons appear instead of vague error.

- [ ] **Step 5: Commit**

```bash
git add web/public/modules/setup.js web/public/modules/settings.js web/public/index.html
git commit -m "ux: add visible countdown and retry/skip to cookie timeout dialog"
```

---

### Task 3: Cookie Timeout Countdown (TUI)

**Files:**
- Modify: `internal/tui/setup_wizard.go:538-598` (cookie flow + new tea.Tick timer)

- [ ] **Step 1: Add countdown state fields to SetupWizardModel**

In `internal/tui/setup_wizard.go`, find the model fields (around line 160). Add:

```go
cookieCountdown int  // seconds remaining for cookie timeout
cookieTimedOut  bool // true when countdown reaches 0
```

- [ ] **Step 2: Add tea.Tick command for countdown**

Create a new message type and tick command:

```go
type cookieCountdownTickMsg struct{}

func cookieCountdownTick() tea.Cmd {
    return tea.Tick(time.Second, func(t time.Time) tea.Msg {
        return cookieCountdownTickMsg{}
    })
}
```

- [ ] **Step 3: Start countdown when cookie extraction begins**

In the cookie activation code (around lines 571-590), where `m.cookieActive = true` is set, also set:

```go
m.cookieCountdown = 60
m.cookieTimedOut = false
```

Return the tick command alongside the spinner tick:

```go
return m, tea.Batch(m.spinner.Tick, cookieCountdownTick())
```

- [ ] **Step 4: Handle countdown tick in Update()**

In the `Update()` method, add a case for `cookieCountdownTickMsg`:

```go
case cookieCountdownTickMsg:
    if m.cookieActive && m.cookieCountdown > 0 {
        m.cookieCountdown--
        if m.cookieCountdown <= 0 {
            m.cookieTimedOut = true
            m.cookieActive = false
            if m.OnCancelAutoCookie != nil {
                m.OnCancelAutoCookie()
            }
            return m, nil
        }
        return m, cookieCountdownTick()
    }
    return m, nil
```

- [ ] **Step 5: Update spinner text to show countdown**

In the cookie view rendering (around `viewSimpleCookies()`), where the spinner is shown during `cookieActive`, change the spinner text to include countdown:

```go
spinnerText := fmt.Sprintf("Waiting for login... (%ds remaining)", m.cookieCountdown)
if m.cookieCountdown <= 10 {
    spinnerText = lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
        fmt.Sprintf("Waiting for login... (%ds remaining)", m.cookieCountdown),
    )
}
```

When `cookieTimedOut` is true, show retry/skip options instead of spinner:

```go
if m.cookieTimedOut {
    lines = append(lines, "")
    lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
        "Cookie extraction timed out."))
    lines = append(lines, "")
    lines = append(lines, "  R  Try Again")
    lines = append(lines, "  S  Skip")
}
```

- [ ] **Step 6: Handle retry/skip keys in timed-out state**

In the key handler for the cookie stage, when `cookieTimedOut` is true:

```go
if m.cookieTimedOut {
    switch key {
    case "r":
        m.cookieTimedOut = false
        // restart cookie flow for the same platform
        m.OnStartAutoCookie(m.cookiePlatform)
        m.cookieActive = true
        m.cookieCountdown = 60
        return m, tea.Batch(m.spinner.Tick, cookieCountdownTick())
    case "s":
        m.cookieTimedOut = false
        return m, nil // skip, stay on current step
    }
}
```

- [ ] **Step 7: Build and test**

```bash
go build ./...
```

- [ ] **Step 8: Commit**

```bash
git add internal/tui/setup_wizard.go
git commit -m "ux: add cookie timeout countdown with retry/skip to TUI setup wizard"
```

---

### Task 4: Cookie Success Confirmation (Per-Platform)

**Files:**
- Modify: `web/public/modules/setup.js:297-315` (success feedback)
- Modify: `web/public/modules/settings.js` (settings cookie success)
- Modify: `internal/tui/setup_wizard.go` (TUI cookie success)

- [ ] **Step 1: Add per-platform toast on Web UI cookie success**

In `web/public/modules/setup.js`, find the success handling (around line 297). Replace the generic toast with per-platform messages:

```javascript
if (ytOk) {
  this.app.showToast("YouTube cookies configured", "success");
}
if (twOk) {
  this.app.showToast("Twitch cookies configured", "success");
}
```

Apply the same change in `web/public/modules/settings.js` where cookie success is handled.

- [ ] **Step 2: Add per-platform feedback in TUI**

In `internal/tui/setup_wizard.go`, where `cookieYTDone` / `cookieTWDone` flags are set (in the `setupCookieFinishMsg` handler), add a feedback message:

```go
if platform == "youtube" {
    m.cookieYTDone = true
    m.setFeedback("YouTube cookies configured", feedbackSuccess)
} else if platform == "twitch" {
    m.cookieTWDone = true
    m.setFeedback("Twitch cookies configured", feedbackSuccess)
}
```

Note: `setFeedback` does NOT exist on `SetupWizardModel` — it's on `App`. The wizard should return a typed message (e.g., `setupCookieSuccessMsg{platform string}`) that `App` handles in `app_update.go` by calling `a.setFeedback()`. Follow the existing pattern for `setupCookieFinishMsg`.

- [ ] **Step 3: Build and test**

```bash
go build ./...
```

- [ ] **Step 4: Commit**

```bash
git add web/public/modules/setup.js web/public/modules/settings.js internal/tui/setup_wizard.go
git commit -m "ux: show per-platform cookie success confirmation in both UIs"
```

---

### Task 5: FFmpeg Skip Option + Re-Accessible Installer

**Files:**
- Modify: `internal/tui/ffmpeg_check.go:236-267` (main menu key handling)
- Modify: `internal/tui/ffmpeg_check.go:490-507` (main menu view)
- Modify: `web/public/index.html:1465-1537` (FFmpeg overlay)
- Modify: `web/public/modules/setup.js` (FFmpeg skip handler)
- Modify: `web/public/modules/settings.js` (re-accessible installer)
- Modify: `internal/tui/settings.go` (re-accessible installer from settings)
- Modify: `internal/tui/settings_view.go` (render install hint)

- [ ] **Step 1: Add "Skip for now" to TUI FFmpeg overlay**

In `internal/tui/ffmpeg_check.go`, the main menu has 3 options (Install=0, Custom=1, Quit=2) at `handleMainKey()` (line 236). Add a 4th option: Skip=3.

Update the choice range validation to allow 0-3. Add a case for choice 3 that returns `"skip"`.

In the View() method (around line 490), add the Skip option to the rendered menu:

```go
items := []string{"Install FFmpeg", "Custom FFmpeg path", "Quit Moombox", "Skip for now"}
```

Add a warning below the Skip option:

```go
if m.choice == 3 {
    // Show warning about skipping
    lines = append(lines, "")
    lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("#f1c40f")).Render(
        "⚠ Muxing will fail until FFmpeg is installed."))
    lines = append(lines, DimStyle.Render("You can install it later from Settings → Paths."))
}
```

- [ ] **Step 2: Handle "skip" action in App**

In `internal/tui/app.go` or `app_update.go`, where FFmpeg check results are processed, handle the `"skip"` return from `ffmpegCheck.HandleKey()` by closing the overlay without quitting:

```go
case "skip":
    a.ffmpegCheck.Close()
```

- [ ] **Step 3: Add "Skip for now" to Web UI FFmpeg overlay**

In `web/public/index.html`, find the FFmpeg main view (around line 1472-1483). Add a Skip button between "Custom FFmpeg path" and "Quit Moombox":

```html
<sl-button id="ffmpeg-skip-btn" variant="default" style="width: 100%; margin-bottom: 0.5em;">Skip for now</sl-button>
<sl-alert variant="warning" id="ffmpeg-skip-warning" style="display: none; margin-bottom: 0.75em;">
    <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
    Muxing will fail until FFmpeg is installed. You can install it later from Settings → Paths.
</sl-alert>
```

In `web/public/modules/setup.js`, add a click handler for the skip button:

```javascript
document.getElementById("ffmpeg-skip-btn")?.addEventListener("click", () => {
    document.getElementById("ffmpeg-overlay").style.display = "none";
    this.initializeApp();
});
```

Show the warning on hover or focus of the skip button.

- [ ] **Step 4: Add "Install FFmpeg" button to Web UI Settings → Paths**

In `web/public/index.html`, find the FFmpeg path input in settings (search for `cfg-ffmpeg-path`). Add an install button next to it:

```html
<div style="display: flex; gap: 0.5em; align-items: flex-end;">
    <sl-input id="cfg-ffmpeg-path" label="FFmpeg Path" placeholder="(system PATH)" style="flex: 1;"></sl-input>
    <sl-button id="cfg-ffmpeg-install-btn" variant="default" size="medium">
        <sl-icon slot="prefix" name="download"></sl-icon> Install
    </sl-button>
</div>
```

In `web/public/modules/settings.js`, add handler that shows the FFmpeg overlay from settings context:

```javascript
document.getElementById("cfg-ffmpeg-install-btn")?.addEventListener("click", () => {
    this.app.setup.showFFmpegOverlay();
});
```

- [ ] **Step 5: Add "Install FFmpeg" action to TUI Settings → Paths**

In `internal/tui/settings.go`, find the FFmpeg path field definition (line 71 in `sections`). This is the `fieldText` type field.

In `internal/tui/settings_view.go`, in the hint rendering for the Paths section, add a conditional hint when the FFmpeg path field is focused:

```go
if sec.name == "Paths" && field.key == "ffmpeg_path" {
    hint += "  I: Install FFmpeg"
}
```

In the settings key handler, add the "i" key when on the FFmpeg field to open the FFmpeg check overlay:

```go
if key == "i" && sec.name == "Paths" && field.key == "ffmpeg_path" {
    // Signal to app to open FFmpeg installer
    return m, func() tea.Msg { return openFFmpegInstallerMsg{} }
}
```

Handle `openFFmpegInstallerMsg` in `app_update.go` to open the FFmpeg check overlay.

- [ ] **Step 6: Build and test**

```bash
go build ./...
```

Test: Verify Skip works in both UIs. Verify Install button in Settings → Paths opens the FFmpeg installer overlay.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/ffmpeg_check.go internal/tui/app.go internal/tui/app_update.go internal/tui/settings.go internal/tui/settings_view.go web/public/index.html web/public/modules/setup.js web/public/modules/settings.js
git commit -m "ux: add FFmpeg skip option and re-accessible installer from settings"
```

---

### Task 6: Restart Progress Feedback (Web UI)

**Files:**
- Modify: `web/public/modules/setup.js:699-808` (pollForRestart)

- [ ] **Step 1: Add phased progress display to pollForRestart()**

In `web/public/modules/setup.js`, find `pollForRestart()` (line 701). Replace the static "Restarting Moombox..." message (lines 746-758) with a phased progress display:

```javascript
// Phase tracking
const startTime = Date.now();
const phaseEl = document.createElement("p");
phaseEl.style.cssText = "margin-top: 0.75em; font-size: var(--sl-font-size-small); color: var(--sl-color-neutral-500);";
phaseEl.textContent = "Saving configuration...";

const elapsedEl = document.createElement("p");
elapsedEl.style.cssText = "font-size: var(--sl-font-size-x-small); color: var(--sl-color-neutral-400); margin-top: 0.25em;";

const elapsedInterval = setInterval(() => {
    const elapsed = Math.floor((Date.now() - startTime) / 1000);
    elapsedEl.textContent = `${elapsed}s elapsed`;
    if (elapsed >= 15) {
        phaseEl.textContent = "Reconnecting... This is taking longer than usual. The server may be installing plugins.";
    }
}, 1000);
```

Update the `msg` element to use `phaseEl` and add `elapsedEl` to the inner container.

At the start of the `poll` function, update the phase text:

```javascript
if (attempts === 1) {
    phaseEl.textContent = "Restarting server...";
}
if (attempts >= 2) {
    phaseEl.textContent = "Reconnecting...";
}
```

On success (around line 771), update to "Connected!" and clear the interval:

```javascript
phaseEl.textContent = "Connected!";
clearInterval(elapsedInterval);
```

On failure (around line 786), clear the interval:

```javascript
clearInterval(elapsedInterval);
```

- [ ] **Step 2: Build and test**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test: Complete setup wizard, observe phased progress messages during restart.

- [ ] **Step 3: Commit**

```bash
git add web/public/modules/setup.js
git commit -m "ux: show phased progress and elapsed time during setup restart"
```

---

### Task 7: Post-Setup Onboarding Nudge

**Files:**
- Modify: `web/public/app.js:940-964` (renderJobs empty state)
- Modify: `web/public/modules/setup.js` (set sessionStorage flag)
- Modify: `internal/tui/task_list.go:468-487` (View empty state)
- Modify: `internal/tui/app.go` (justCompletedSetup flag)

- [ ] **Step 1: Set sessionStorage flag on Web UI setup completion**

In `web/public/modules/setup.js`, in the `submitSetup()` or `pollForRestart()` success path (around line 771-779 where `initializeApp()` is called), add:

```javascript
sessionStorage.setItem("justCompletedSetup", "1");
```

- [ ] **Step 2: Show welcome empty state in Web UI**

In `web/public/app.js`, find `renderJobs()` (around line 940). In the `jobs.length === 0` branch (lines 951-964), check for the setup flag:

```javascript
if (this.jobs.length === 0) {
    container.innerHTML = "";
    emptyState.style.display = "";
    const justSetup = sessionStorage.getItem("justCompletedSetup");

    const icon = emptyState.querySelector("sl-icon");
    const msg = emptyState.querySelector("p");
    const subtext = emptyState.querySelector(".empty-state-subtext");
    const cta = emptyState.querySelector(".empty-state-cta");

    if (justSetup) {
        sessionStorage.removeItem("justCompletedSetup");
        if (icon) icon.name = "check-circle";
        if (msg) msg.textContent = "Setup complete!";
        if (subtext) subtext.innerHTML = 'Add channels to auto-monitor streams, or add a video URL to start archiving now.';
        // Replace single CTA with two buttons
        if (cta) {
            cta.outerHTML = `
                <div class="empty-state-cta" style="display: flex; gap: 0.5em; flex-wrap: wrap; justify-content: center;">
                    <sl-button variant="default" size="small" onclick="document.querySelector('sl-tab[panel=settings]')?.click(); setTimeout(() => document.querySelector('[data-section=channels]')?.click(), 100);">
                        <sl-icon slot="prefix" name="broadcast"></sl-icon> Add Channels
                    </sl-button>
                    <sl-button variant="primary" size="small" id="empty-state-add-btn">
                        <sl-icon slot="prefix" name="plus"></sl-icon> Add Video
                    </sl-button>
                </div>`;
            document.getElementById("empty-state-add-btn")?.addEventListener("click", addVideoBtnClick);
        }
    } else {
        if (icon) icon.name = "inbox";
        if (msg) msg.textContent = "No jobs yet";
        if (subtext) subtext.textContent = "Add a YouTube or Twitch URL to start archiving";
        if (cta) cta.style.display = "";
    }
    if (filterCount) { filterCount.style.display = "none"; }
    return;
}
```

Note: `addVideoBtnClick` must be accessible — it's defined at the module top level in the existing code.

- [ ] **Step 3: Add justCompletedSetup flag to TUI**

In `internal/tui/app.go`, add a field to the App struct:

```go
justCompletedSetup bool
```

In the setup wizard completion handler (where `setupWiz.Close()` is called and config is saved), set:

```go
a.justCompletedSetup = true
```

- [ ] **Step 4: Show welcome empty state in TUI**

In `internal/tui/task_list.go`, find the empty list view (line 474-475). The `TaskListModel` needs access to the `justCompletedSetup` flag. Add a field:

```go
JustCompletedSetup bool
```

Update the empty view:

```go
if len(m.list.Items()) == 0 {
    if m.JustCompletedSetup {
        m.JustCompletedSetup = false
        listContent = lipgloss.NewStyle().Foreground(lipgloss.Color("#2ecc71")).Render("Setup complete!") + "\n\n" +
            DimStyle.Render("Press ` to open Settings and add channels,") + "\n" +
            DimStyle.Render("or A A to add a video.")
    } else {
        listContent = DimStyle.Render("No tasks. Press A to add, or use Web UI.")
    }
}
```

Set the flag on the task list model when setup completes in `app.go`.

- [ ] **Step 5: Build and test**

```bash
go build ./...
```

- [ ] **Step 6: Commit**

```bash
git add web/public/app.js web/public/modules/setup.js internal/tui/task_list.go internal/tui/app.go
git commit -m "ux: show contextual onboarding nudge after setup completion"
```

---

## Phase 2: Feedback & Indicators

### Task 8: Log Auto-Scroll Indicator (TUI)

**Files:**
- Modify: `internal/tui/log_viewer.go:257-301` (View method)
- Modify: `internal/tui/log_viewer.go:111-132` (scroll handlers for End key)

- [ ] **Step 1: Add auto-scroll paused indicator to View()**

In `internal/tui/log_viewer.go`, find the `View()` method (line 257). After the viewport content is rendered but before the final border/style is applied, add a paused indicator when auto-scroll is off:

```go
if m.focused && !m.autoScroll {
    pauseHint := DimStyle.Render("↓ Auto-scroll paused (End to resume)")
    content = content + "\n" + pauseHint
}
```

This appends the hint as the last visible line in the log viewport.

- [ ] **Step 2: Ensure End key re-enables auto-scroll**

Check if End key is already handled. If not, add handling in the key processing section. The viewport keymap should allow End. In the key handler:

```go
case key.Matches(msg, key.NewBinding(key.WithKeys("end"))):
    m.viewport.GotoBottom()
    m.autoScroll = true
```

If End is already handled by the viewport and `UpdateViewport()` catches it (since `AtBottom()` would be true after End), this is already working. Verify by checking the `UpdateViewport()` method (lines 141-146) — it syncs `autoScroll` with `viewport.AtBottom()`.

- [ ] **Step 3: Build and test**

```bash
go build ./...
```

Test: Focus log panel, scroll up — verify "↓ Auto-scroll paused (End to resume)" appears. Press End — verify it disappears and auto-scroll resumes.

- [ ] **Step 4: Commit**

```bash
git add internal/tui/log_viewer.go
git commit -m "ux: show auto-scroll paused indicator in TUI log viewer"
```

---

### Task 9: Log Auto-Scroll Indicator (Web UI)

**Files:**
- Modify: `web/public/index.html:381-400` (logs panel)
- Modify: `web/public/app.js:212-219` (log scroll handler)
- Modify: `web/public/moombox.css` (pill styling)

- [ ] **Step 1: Add auto-scroll pill element to HTML**

In `web/public/index.html`, find the logs panel (around line 381-400). Inside the log viewer container, add a pill element:

```html
<div id="log-autoscroll-pill" style="display: none;">
    <sl-icon name="arrow-down"></sl-icon> Resume auto-scroll
</div>
```

- [ ] **Step 2: Add CSS styling for the pill**

In `web/public/moombox.css`, add styling:

```css
#log-autoscroll-pill {
    position: sticky;
    bottom: 0.5em;
    margin: 0 auto;
    width: fit-content;
    padding: 0.25em 0.75em;
    background: var(--sl-color-primary-600);
    color: white;
    border-radius: var(--sl-border-radius-pill);
    font-size: var(--sl-font-size-small);
    cursor: pointer;
    opacity: 0;
    animation: fadeIn 0.2s ease forwards;
    z-index: 10;
    display: flex;
    align-items: center;
    gap: 0.25em;
}

@keyframes fadeIn {
    to { opacity: 1; }
}
```

- [ ] **Step 3: Toggle pill visibility based on auto-scroll state**

In `web/public/app.js`, find the log scroll handler (around line 212). After `_logAutoScroll` is updated:

```javascript
const pill = document.getElementById("log-autoscroll-pill");
if (pill) {
    pill.style.display = this._logAutoScroll ? "none" : "";
}
```

Add a click handler for the pill (in the init section):

```javascript
document.getElementById("log-autoscroll-pill")?.addEventListener("click", () => {
    const logsViewer = document.getElementById("logs-viewer");
    if (logsViewer) {
        logsViewer.scrollTop = logsViewer.scrollHeight;
        this._logAutoScroll = true;
    }
    document.getElementById("log-autoscroll-pill").style.display = "none";
});
```

- [ ] **Step 4: Build and test**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test: Navigate to Logs tab, scroll up — verify pill appears. Click it — verify it disappears and scrolls to bottom.

- [ ] **Step 5: Commit**

```bash
git add web/public/index.html web/public/app.js web/public/moombox.css
git commit -m "ux: show auto-scroll resume pill in Web UI log viewer"
```

---

### Task 10: Action Menu Disabled Explanations (TUI)

**Files:**
- Modify: `internal/tui/action_menu.go:24-32` (ActionMenuItem struct)
- Modify: `internal/tui/action_menu.go:82-84` (rendering "(none)")
- Modify: `internal/tui/app_actions.go:307-368` (buildMenuItems)

- [ ] **Step 1: Add DisabledReason field to ActionMenuItem**

In `internal/tui/action_menu.go`, find the `ActionMenuItem` struct (line 24). Add:

```go
DisabledReason string // shown when NeedsJob && no matching jobs
```

- [ ] **Step 2: Update rendering to show reason instead of "(none)"**

In `internal/tui/action_menu.go`, find the rendering code (around line 82-84) where `noJobs` triggers `" (none)"`. Replace with:

```go
if mi.noJobs {
    reason := " (none)"
    if mi.item.DisabledReason != "" {
        reason = " · " + mi.item.DisabledReason
    }
    label += DimStyle.Render(reason)
}
```

- [ ] **Step 3: Add DisabledReason to each filtered action in buildMenuItems()**

In `internal/tui/app_actions.go`, find `buildMenuItems()` (around line 307). Add `DisabledReason` to each action that has a `JobFilter`:

```go
// Retry action
DisabledReason: "no failed jobs",

// Cancel action
DisabledReason: "no active jobs",

// Delete action
DisabledReason: "no deletable jobs",

// Trim action
DisabledReason: "no finished jobs with files",
```

- [ ] **Step 4: Build and test**

```bash
go build ./...
```

Test: Open action menu (M) with no jobs — verify "Retry · no failed jobs" etc. instead of "(none)".

- [ ] **Step 5: Commit**

```bash
git add internal/tui/action_menu.go internal/tui/app_actions.go
git commit -m "ux: show context-specific reasons for disabled actions in TUI menu"
```

---

### Task 11: Error Message Truncation Fix (Web UI)

**Files:**
- Modify: `web/public/app.js:1202` (error truncation)
- Modify: `web/public/moombox.css` (expandable error styling)

- [ ] **Step 1: Make error text expandable on desktop, full on mobile**

In `web/public/app.js`, find where error messages are truncated (around line 1202). Replace the truncation logic with a class-based approach:

```javascript
const errorText = job.error || "Error";
const truncated = errorText.length > 50 ? errorText.substring(0, 50) + "…" : errorText;
return `<span class="job-error-text" title="${this.escapeHtml(errorText)}"
    data-full="${this.escapeHtml(errorText)}"
    data-short="${this.escapeHtml(truncated)}">${this.escapeHtml(truncated)}</span>`;
```

- [ ] **Step 2: Add CSS for expandable error text**

In `web/public/moombox.css`, add:

```css
.job-error-text {
    cursor: pointer;
    text-decoration-style: dotted;
    text-decoration-line: underline;
    text-underline-offset: 2px;
}

.job-error-text.expanded {
    white-space: normal;
    word-break: break-word;
}

@media (max-width: 768px) {
    .job-error-text {
        white-space: normal;
        word-break: break-word;
        cursor: default;
        text-decoration: none;
    }
}
```

- [ ] **Step 3: Add click handler for expand/collapse**

In `web/public/app.js`, add event delegation for the error text toggle on the jobs container:

```javascript
container.addEventListener("click", (e) => {
    const errorText = e.target.closest(".job-error-text");
    if (errorText) {
        e.stopPropagation();
        const isExpanded = errorText.classList.contains("expanded");
        errorText.textContent = isExpanded ? errorText.dataset.short : errorText.dataset.full;
    }
});
```

Note: This may already be in a delegated click handler on `#jobs-container`. Integrate with the existing event delegation pattern.

- [ ] **Step 4: Build and test**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test desktop: Click truncated error — verify it expands. Click again — verify it collapses. Test mobile viewport: verify error shows full text without truncation.

- [ ] **Step 5: Commit**

```bash
git add web/public/app.js web/public/moombox.css
git commit -m "ux: make error messages expandable on desktop, full-width on mobile"
```

---

### Task 12: Settings Unsaved Changes Banner (Web UI)

**Files:**
- Modify: `web/public/index.html` (banner element in settings panel)
- Modify: `web/public/modules/settings.js:62,713-729` (dirty tracking + banner)
- Modify: `web/public/moombox.css` (banner styling)
- Modify: `web/public/app.js` (tab switch interception)

- [ ] **Step 1: Add banner element to settings panel HTML**

In `web/public/index.html`, find the settings panel content area (around line 460). Add a banner at the top of the settings content:

```html
<div id="settings-unsaved-banner" style="display: none;">
    <sl-icon name="exclamation-triangle"></sl-icon>
    <span>You have unsaved changes</span>
    <div style="margin-left: auto; display: flex; gap: 0.5em;">
        <sl-button id="settings-banner-discard" variant="default" size="small">Discard</sl-button>
        <sl-button id="settings-banner-save" variant="primary" size="small">Save</sl-button>
    </div>
</div>
```

- [ ] **Step 2: Add banner CSS styling**

In `web/public/moombox.css`:

```css
#settings-unsaved-banner {
    position: sticky;
    top: 0;
    z-index: 10;
    display: flex;
    align-items: center;
    gap: var(--sl-spacing-small);
    padding: var(--sl-spacing-small) var(--sl-spacing-medium);
    background: var(--sl-color-warning-100);
    border-bottom: 1px solid var(--sl-color-warning-300);
    color: var(--sl-color-warning-700);
    font-size: var(--sl-font-size-small);
}
```

- [ ] **Step 3: Wire banner to dirty state in settings.js**

In `web/public/modules/settings.js`, update `_markDirty()` (around line 713) and `_updateUnsavedIndicator()` (around line 717) to also show/hide the banner:

```javascript
_markDirty() {
    this._dirty = true;
    this._updateUnsavedIndicator();
    const banner = document.getElementById("settings-unsaved-banner");
    if (banner) banner.style.display = "";
}
```

After save or discard, hide the banner:

```javascript
const banner = document.getElementById("settings-unsaved-banner");
if (banner) banner.style.display = "none";
```

Add click handlers for the banner buttons:

```javascript
document.getElementById("settings-banner-discard")?.addEventListener("click", () => {
    this.loadConfig();
    this._dirty = false;
    this._updateUnsavedIndicator();
    document.getElementById("settings-unsaved-banner").style.display = "none";
});

document.getElementById("settings-banner-save")?.addEventListener("click", () => {
    this.saveConfig();
});
```

- [ ] **Step 4: Intercept main tab switches when dirty**

Shoelace's `sl-tab-show` does NOT support `preventDefault()` — it fires after the tab switch. Instead, intercept clicks on tab elements before the switch happens:

```javascript
// Intercept tab clicks when settings are dirty
document.querySelectorAll("sl-tab[slot='nav']").forEach(tab => {
    tab.addEventListener("click", (e) => {
        if (tab.panel !== "settings" && this.settings?._dirty) {
            e.preventDefault();
            e.stopImmediatePropagation();
            this.showConfirm("You have unsaved settings changes. Discard and leave?", {
                title: "Unsaved Changes",
                okLabel: "Leave Without Saving",
                okVariant: "warning"
            }).then(confirmed => {
                if (confirmed) {
                    this.settings.loadConfig();
                    this.settings._dirty = false;
                    this.settings._updateUnsavedIndicator();
                    document.getElementById("settings-unsaved-banner").style.display = "none";
                    tab.click();
                }
            });
        }
    }, true); // capture phase to intercept before Shoelace handles it
});
```

- [ ] **Step 5: Build and test**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test: Change a setting — verify banner appears. Click Discard — verify banner disappears and field reverts. Change a setting and try switching tabs — verify confirmation dialog.

- [ ] **Step 6: Commit**

```bash
git add web/public/index.html web/public/modules/settings.js web/public/moombox.css web/public/app.js
git commit -m "ux: add unsaved changes banner with discard/save to Web UI settings"
```

---

## Phase 3: Discoverability & Labels

### Task 13: Chord System Hints (TUI)

**Files:**
- Modify: `internal/tui/app.go` (seenChordHint flag)
- Modify: `internal/tui/status_bar.go:84-138` (render hint in status bar)
- Modify: `internal/tui/app_keys.go:403-410` (dismiss on chord use)

- [ ] **Step 1: Add seenChordHint flag to App**

In `internal/tui/app.go`, add to the App struct:

```go
seenChordHint bool
```

- [ ] **Step 2: Pass hint state to status bar**

The `StatusBarModel` needs to know whether to show the hint. Add a field:

```go
ShowChordHint bool
```

In `App`'s update loop, sync this before rendering:

```go
a.statusBar.ShowChordHint = !a.seenChordHint
```

- [ ] **Step 3: Render chord hint in status bar**

In `internal/tui/status_bar.go`, find the `View()` method (line 84). When `m.ShowChordHint` is true, append a dim hint to the status bar's right side:

```go
if m.ShowChordHint {
    hint := DimStyle.Render("Press ? for help · M for menu · A for actions")
    // Append to the right section of the status bar
}
```

Integrate this with the existing `renderControls()` method — when `ShowChordHint` is true, show the newcomer hint instead of the standard chord reference.

- [ ] **Step 4: Dismiss hint on chord use**

In `internal/tui/app_keys.go`, find where chord prefixes are handled (around line 403-410). After any chord prefix, M, or ? is pressed:

```go
a.seenChordHint = true
```

- [ ] **Step 5: Build and test**

```bash
go build ./...
```

Test: Start TUI fresh — verify hint appears in status bar. Press M — verify hint disappears. Restart TUI — verify hint reappears (not persisted).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/app.go internal/tui/status_bar.go internal/tui/app_keys.go
git commit -m "ux: show newcomer chord hints in TUI status bar until first use"
```

---

### Task 14: Label Clarity — "Include Non-Live Content"

**Files:**
- Modify: `web/public/index.html:1164` (channel dialog checkbox)
- Modify: `web/public/index.html:1548` (setup wizard checkbox)
- Modify: `internal/tui/settings.go:194` (channel field def)
- Modify: `internal/tui/setup_wizard.go` (channel field in setup)

- [ ] **Step 1: Update Web UI labels**

In `web/public/index.html`, find line 1164:

```html
<sl-checkbox id="channel-include-vods">Include non-live content (VODs, premieres)</sl-checkbox>
```

Replace with:

```html
<sl-checkbox id="channel-include-vods">Also archive uploads & premieres (YouTube only)</sl-checkbox>
```

Find line 1548 (setup wizard version) and apply the same label change.

- [ ] **Step 2: Update TUI label**

In `internal/tui/settings.go`, find line 194:

```go
{"include_non_live", "Include non-live", fieldToggle, []string{"No", "Yes"}, "", "youtube"},
```

Replace with:

```go
{"include_non_live", "Archive uploads & premieres (YouTube only)", fieldToggle, []string{"No", "Yes"}, "also capture uploads and premieres, not just live streams", "youtube"},
```

Apply the same label change in `internal/tui/setup_wizard.go` if the channel fields are defined there separately.

- [ ] **Step 3: Build and test**

```bash
go build ./...
```

- [ ] **Step 4: Commit**

```bash
git add web/public/index.html internal/tui/settings.go internal/tui/setup_wizard.go
git commit -m "ux: rename 'include non-live' to 'archive uploads & premieres (YouTube only)'"
```

---

### Task 15: Output Template Live Preview

**Files:**
- Modify: `web/public/index.html:710,1344` (template inputs in settings + setup)
- Modify: `web/public/modules/settings.js` (preview handler)
- Modify: `web/public/modules/setup.js` (preview handler)
- Modify: `internal/tui/settings.go:25-32` (fieldDef struct — add previewFn)
- Modify: `internal/tui/settings_view.go:138-237` (renderFields — render preview)
- Modify: `internal/tui/setup_wizard.go` (setup template preview)

- [ ] **Step 1: Add preview div to Web UI settings template input**

In `web/public/index.html`, find the settings template input (around line 710). Add a preview element below it:

```html
<div id="template-preview" class="template-preview" style="display: none;"></div>
```

Do the same for the setup wizard template input (around line 1344).

- [ ] **Step 2: Add preview CSS**

In `web/public/moombox.css`:

```css
.template-preview {
    font-size: var(--sl-font-size-small);
    color: var(--sl-color-neutral-500);
    font-family: var(--sl-font-mono);
    padding: 0.25em 0;
    word-break: break-all;
}
```

- [ ] **Step 3: Add template preview JS function**

Create a shared helper function (can go in `web/public/modules/utils.js` or inline):

```javascript
function renderTemplatePreview(template) {
    const now = new Date();
    const vars = {
        channel: "Miko Ch",
        title: "Singing Stream",
        id: "dQw4w9WgXcQ",
        start_date: now.toISOString().split("T")[0],
        start_time: "20-00-00",
    };
    let result = template || "";
    for (const [key, val] of Object.entries(vars)) {
        result = result.replaceAll("${" + key + "}", val);
    }
    return result ? "Example: " + result + ".mkv" : "";
}
```

- [ ] **Step 4: Wire preview to settings template input**

In `web/public/modules/settings.js`, after the config form is populated, add an input listener:

```javascript
const templateInput = document.getElementById("cfg-output-template");
const templatePreview = document.getElementById("template-preview");
if (templateInput && templatePreview) {
    const updatePreview = () => {
        const val = templateInput.value || templateInput.placeholder;
        templatePreview.textContent = renderTemplatePreview(val);
        templatePreview.style.display = val ? "" : "none";
    };
    templateInput.addEventListener("sl-input", updatePreview);
    updatePreview(); // initial render
}
```

Apply the same pattern in `web/public/modules/setup.js` for the setup template input.

- [ ] **Step 5: Add previewFn to TUI fieldDef**

In `internal/tui/settings.go`, update the `fieldDef` struct (line 25):

```go
type fieldDef struct {
    key       string
    label     string
    ftype     fieldType
    options   []string
    help      string
    previewFn func(value string) string // optional live preview
}
```

Add the preview function to the output template field (around line 95):

```go
{"output_template", "Output template", fieldText, nil, "${title} ${id} ${channel} ${start_date} ${start_time}",
    func(value string) string {
        if value == "" {
            value = "${channel}/${start_date} ${title} [${id}]"
        }
        now := time.Now().Format("2006-01-02")
        r := strings.NewReplacer(
            "${channel}", "Miko Ch",
            "${title}", "Singing Stream",
            "${id}", "dQw4w9WgXcQ",
            "${start_date}", now,
            "${start_time}", "20-00-00",
        )
        return "Example: " + r.Replace(value) + ".mkv"
    },
},
```

Note: This changes the field initializer from 5 fields to 6. There are approximately 30 `fieldDef` positional initializers in the `sections` variable (lines 53-136 of `settings.go`). Each one needs a trailing `, nil` appended for the new `previewFn` field. The `channelFieldDef` struct (line 179) is a separate type and is NOT affected. Use find-and-replace on the closing `}` pattern to add `, nil}` efficiently.

- [ ] **Step 6: Render preview in renderFields()**

In `internal/tui/settings_view.go`, find `renderFields()` (line 138). After rendering each field line, check for previewFn:

```go
if field.previewFn != nil {
    preview := field.previewFn(m.values[field.key])
    if preview != "" {
        lines = append(lines, "  "+DimStyle.Render(preview))
    }
}
```

- [ ] **Step 7: Add preview to TUI setup wizard template field**

In `internal/tui/setup_wizard.go`, find the output template field in `advancedSetupSteps` (line 80). The `setupFieldDef` struct doesn't have `previewFn`. Add a similar preview rendering approach — either add `previewFn` to `setupFieldDef` or hardcode the preview for the template field in the advanced step view.

- [ ] **Step 8: Build and test**

```bash
go build ./...
```

Test: Type in the template field in both UIs — verify live preview updates. Clear the field — verify preview shows default template example.

- [ ] **Step 9: Commit**

```bash
git add web/public/index.html web/public/moombox.css web/public/modules/settings.js web/public/modules/setup.js internal/tui/settings.go internal/tui/settings_view.go internal/tui/setup_wizard.go
git commit -m "ux: add live output template preview to settings and setup wizard"
```

---

### Task 16: Parallel Downloads Guidance

**Files:**
- Modify: `web/public/index.html:725,1392` (parallel downloads inputs)
- Modify: `internal/tui/settings.go:97` (settings field)
- Modify: `internal/tui/setup_wizard.go` (setup field)

- [ ] **Step 1: Add help text to Web UI inputs**

In `web/public/index.html`, find the settings parallel downloads input (around line 725). Add or update the `help-text` attribute:

```html
<sl-input id="cfg-parallel-downloads" type="number" min="1" label="Parallel Downloads" help-text="Streams to download simultaneously. 2–4 recommended. Higher values use more CPU and network.">
```

Find the setup wizard parallel downloads input (around line 1392) and add the same help text.

- [ ] **Step 2: Update TUI help text**

In `internal/tui/settings.go`, find the parallel downloads field (line 97):

```go
{"num_parallel_downloads", "Parallel downloads", fieldNumber, nil, "concurrent download jobs"},
```

Replace help text:

```go
{"num_parallel_downloads", "Parallel downloads", fieldNumber, nil, "2-4 recommended, higher uses more CPU/network"},
```

In `internal/tui/setup_wizard.go`, find the parallel downloads field in `advancedSetupSteps` and update its help text similarly.

- [ ] **Step 3: Build and test**

```bash
go build ./...
```

- [ ] **Step 4: Commit**

```bash
git add web/public/index.html internal/tui/settings.go internal/tui/setup_wizard.go
git commit -m "ux: add guidance text to parallel downloads setting"
```

---

## Phase 4: Power User Features

### Task 17a: Batch Job Selection UI (Web UI)

**Files:**
- Modify: `web/public/app.js` (selection state, renderJobs checkbox)
- Modify: `web/public/index.html` (action bar element)
- Modify: `web/public/moombox.css` (checkbox, action bar, selection styling)

- [ ] **Step 1: Add selection state to App**

In `web/public/app.js`, add selection state in the constructor:

```javascript
this._selectedJobs = new Set();
```

- [ ] **Step 2: Add batch action bar HTML**

In `web/public/index.html`, add the floating action bar after the jobs container (inside the tasks tab panel):

```html
<div id="batch-action-bar" style="display: none;">
    <span id="batch-count"></span>
    <div class="batch-actions">
        <sl-button id="batch-cancel" variant="warning" size="small">
            <sl-icon slot="prefix" name="x-circle"></sl-icon> Cancel
        </sl-button>
        <sl-button id="batch-retry" variant="primary" size="small">
            <sl-icon slot="prefix" name="arrow-repeat"></sl-icon> Retry
        </sl-button>
        <sl-button id="batch-delete" variant="danger" size="small">
            <sl-icon slot="prefix" name="trash"></sl-icon> Delete
        </sl-button>
    </div>
    <sl-icon-button id="batch-clear" name="x-lg" label="Clear selection"></sl-icon-button>
</div>
```

- [ ] **Step 3: Add action bar CSS**

In `web/public/moombox.css`:

```css
#batch-action-bar {
    position: sticky;
    bottom: 0;
    background: var(--sl-color-neutral-100);
    border-top: 1px solid var(--sl-color-neutral-300);
    padding: var(--sl-spacing-small) var(--sl-spacing-medium);
    display: flex;
    align-items: center;
    gap: var(--sl-spacing-medium);
    z-index: 10;
}

.batch-actions {
    display: flex;
    gap: var(--sl-spacing-x-small);
}

.video-item .job-checkbox {
    opacity: 0;
    transition: opacity 0.15s;
    flex-shrink: 0;
}

.video-item:hover .job-checkbox,
.video-item .job-checkbox:checked,
.video-item.selected .job-checkbox {
    opacity: 1;
}

.video-item.selected {
    background: var(--sl-color-primary-50);
}

@media (max-width: 768px) {
    .video-item .job-checkbox {
        opacity: 1;
    }
}
```

- [ ] **Step 4: Add checkbox to job card rendering**

In `web/public/app.js`, find `renderJobCard()` or the job card template (around line 1061-1107). Add a checkbox as the first element in each job card:

```javascript
const isSelected = this._selectedJobs.has(job.id);
const checkbox = `<input type="checkbox" class="job-checkbox" data-job-id="${job.id}" ${isSelected ? "checked" : ""}>`;
```

Insert `checkbox` before the thumbnail in the card HTML.

- [ ] **Step 5: Add checkbox event delegation**

In `web/public/app.js`, add to the existing jobs container event delegation:

```javascript
// Checkbox selection
const checkbox = e.target.closest(".job-checkbox");
if (checkbox) {
    e.stopPropagation();
    const jobId = checkbox.dataset.jobId;
    if (checkbox.checked) {
        this._selectedJobs.add(jobId);
    } else {
        this._selectedJobs.delete(jobId);
    }
    checkbox.closest(".video-item")?.classList.toggle("selected", checkbox.checked);
    this.updateBatchActionBar();
    return;
}
```

- [ ] **Step 6: Build and test selection UI**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test: Click checkboxes on job cards — verify visual selection state. Verify action bar element exists (even if buttons don't work yet).

- [ ] **Step 7: Commit selection UI**

```bash
git add web/public/app.js web/public/index.html web/public/moombox.css
git commit -m "ux: add checkbox selection to Web UI job cards"
```

---

### Task 17b: Batch Job Actions (Web UI)

**Files:**
- Modify: `web/public/app.js` (batch action logic, action bar, Esc handler)

- [ ] **Step 1: Implement updateBatchActionBar()**

```javascript
updateBatchActionBar() {
    const bar = document.getElementById("batch-action-bar");
    if (!bar) return;

    const count = this._selectedJobs.size;
    if (count === 0) {
        bar.style.display = "none";
        return;
    }
    bar.style.display = "";
    document.getElementById("batch-count").textContent = `${count} selected`;

    // Show/hide action buttons based on selected job statuses
    const CANCEL_STATUSES = ["Downloading", "Live", "Upcoming", "Muxing", "COOKIES?"];
    const RETRY_STATUSES = ["Error", "Cancelled", "COOKIES?"];
    const DELETE_STATUSES = ["Finished", "Error", "Cancelled", "COOKIES?"];

    const selectedJobs = this.jobs.filter(j => this._selectedJobs.has(j.id));
    const canCancel = selectedJobs.some(j => CANCEL_STATUSES.includes(j.status));
    const canRetry = selectedJobs.some(j => RETRY_STATUSES.includes(j.status));
    const canDelete = selectedJobs.some(j => DELETE_STATUSES.includes(j.status));

    document.getElementById("batch-cancel").style.display = canCancel ? "" : "none";
    document.getElementById("batch-retry").style.display = canRetry ? "" : "none";
    document.getElementById("batch-delete").style.display = canDelete ? "" : "none";
}
```

- [ ] **Step 2: Implement batch action handlers**

```javascript
async batchAction(action) {
    const selectedJobs = this.jobs.filter(j => this._selectedJobs.has(j.id));
    let targets;
    let confirmMsg;
    let apiCall;

    switch (action) {
        case "cancel":
            targets = selectedJobs.filter(j => ["Downloading","Live","Upcoming","Muxing","COOKIES?"].includes(j.status));
            confirmMsg = `Cancel ${targets.length} job${targets.length > 1 ? "s" : ""}?`;
            apiCall = (id) => fetch(`/api/jobs/${id}/cancel`, { method: "POST" });
            break;
        case "retry":
            targets = selectedJobs.filter(j => ["Error","Cancelled","COOKIES?"].includes(j.status));
            confirmMsg = `Retry ${targets.length} job${targets.length > 1 ? "s" : ""}?`;
            apiCall = (id) => fetch(`/api/jobs/${id}/retry`, { method: "POST" });
            break;
        case "delete":
            targets = selectedJobs.filter(j => ["Finished","Error","Cancelled","COOKIES?"].includes(j.status));
            confirmMsg = `Delete ${targets.length} job${targets.length > 1 ? "s" : ""}?`;
            apiCall = (id) => fetch(`/api/jobs/${id}`, { method: "DELETE" });
            break;
    }

    if (!targets || targets.length === 0) return;

    const confirmed = await this.showConfirm(confirmMsg, {
        title: "Batch " + action.charAt(0).toUpperCase() + action.slice(1),
        okLabel: action.charAt(0).toUpperCase() + action.slice(1),
        okVariant: action === "delete" ? "danger" : action === "cancel" ? "warning" : "primary"
    });
    if (!confirmed) return;

    const results = await Promise.allSettled(targets.map(j => apiCall(j.id)));
    const succeeded = results.filter(r => r.status === "fulfilled" && r.value.ok).length;
    const failed = targets.length - succeeded;

    this._selectedJobs.clear();
    this.updateBatchActionBar();

    if (failed === 0) {
        this.showToast(`${action === "delete" ? "Deleted" : action === "cancel" ? "Cancelled" : "Retried"} ${succeeded} job${succeeded > 1 ? "s" : ""}`, "success");
    } else {
        this.showToast(`${succeeded} of ${targets.length} jobs ${action}ed. ${failed} failed.`, "warning");
    }
}
```

Wire the batch buttons:

```javascript
document.getElementById("batch-cancel")?.addEventListener("click", () => this.batchAction("cancel"));
document.getElementById("batch-retry")?.addEventListener("click", () => this.batchAction("retry"));
document.getElementById("batch-delete")?.addEventListener("click", () => this.batchAction("delete"));
document.getElementById("batch-clear")?.addEventListener("click", () => {
    this._selectedJobs.clear();
    document.querySelectorAll(".job-checkbox").forEach(cb => { cb.checked = false; });
    document.querySelectorAll(".video-item.selected").forEach(el => el.classList.remove("selected"));
    this.updateBatchActionBar();
});
```

- [ ] **Step 3: Clear selection on Esc**

In the keyboard handler, add:

```javascript
if (e.key === "Escape" && this._selectedJobs.size > 0) {
    this._selectedJobs.clear();
    this.updateBatchActionBar();
    this.renderJobs();
    return;
}
```

- [ ] **Step 4: Preserve selection across re-renders**

In `renderJobs()`, after rendering job cards, re-apply selection state:

```javascript
// Re-apply selection state
this._selectedJobs.forEach(id => {
    const card = container.querySelector(`[data-job-id="${id}"]`);
    if (card) {
        card.classList.add("selected");
        const cb = card.querySelector(".job-checkbox");
        if (cb) cb.checked = true;
    }
});
this.updateBatchActionBar();
```

- [ ] **Step 5: Build and test**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test: Check multiple jobs, verify action bar appears with correct buttons. Click Cancel/Delete/Retry — verify batch confirmation and execution. Press Esc — verify selection clears.

- [ ] **Step 6: Commit**

```bash
git add web/public/app.js
git commit -m "ux: add batch action handlers with confirmation to Web UI"
```

---

### Task 18: Batch Job Operations (TUI)

**Files:**
- Modify: `internal/tui/task_list.go:92-113` (struct + selection state)
- Modify: `internal/tui/task_list.go:649-733` (renderJob — selection markers)
- Modify: `internal/tui/app_keys.go` (chord dispatch for batch)
- Modify: `internal/tui/app.go` (batch action callbacks)

- [ ] **Step 1: Add selection state to TaskListModel**

In `internal/tui/task_list.go`, add to the struct (around line 92):

```go
selected map[string]bool // selected job IDs for batch operations
```

Initialize in the constructor:

```go
selected: make(map[string]bool),
```

Add helper methods:

```go
func (m *TaskListModel) ToggleSelection(jobID string) {
    if m.selected[jobID] {
        delete(m.selected, jobID)
    } else {
        m.selected[jobID] = true
    }
}

func (m *TaskListModel) ClearSelection() {
    m.selected = make(map[string]bool)
}

func (m *TaskListModel) SelectedCount() int {
    return len(m.selected)
}

func (m *TaskListModel) SelectedIDs() []string {
    ids := make([]string, 0, len(m.selected))
    for id := range m.selected {
        ids = append(ids, id)
    }
    return ids
}
```

- [ ] **Step 2: Handle Space key for selection toggle**

In `internal/tui/task_list.go`, find the key handling section. Add Space key handling:

```go
case key.Matches(msg, key.NewBinding(key.WithKeys(" "))):
    // Toggle selection on current job
    if job := m.CurrentJob(); job != nil {
        m.ToggleSelection(job.ID)
    }
```

- [ ] **Step 3: Render selection markers on job items**

In `internal/tui/task_list.go`, find `renderJob()` (around line 649). Add a selection marker prefix:

```go
// At the start of the rendered job line
selMark := "  "
if m.selected[job.ID] {
    selMark = lipgloss.NewStyle().Foreground(lipgloss.Color("#2ecc71")).Render("✓ ")
}
```

Prepend `selMark` to the job line.

- [ ] **Step 4: Show selection count in status bar**

Pass `m.SelectedCount()` to the status bar for display. In the status bar's View(), when count > 0:

```go
if m.SelectedCount > 0 {
    // Show "X selected" in the status bar
    selectedText := fmt.Sprintf("%d selected", m.SelectedCount)
    // Render with primary color
}
```

- [ ] **Step 5: Modify chord dispatch for batch actions**

In `internal/tui/app_keys.go`, find where chord actions are dispatched (Cancel, Delete, Retry). When `taskList.SelectedCount() > 0`, operate on all selected jobs instead of just the current job:

```go
if a.tasks.SelectedCount() > 0 {
    // Batch mode: operate on all selected jobs
    ids := a.tasks.SelectedIDs()
    // Filter to valid IDs for this action, then execute
    // Show single confirmation: "Cancel 3 jobs?"
} else {
    // Single mode: operate on current job (existing behavior)
}
```

- [ ] **Step 6: Handle Esc to clear selection**

In `internal/tui/app_keys.go`, in the Esc handler, add:

```go
if a.tasks.SelectedCount() > 0 {
    a.tasks.ClearSelection()
    return a, nil // consume the Esc, don't propagate
}
```

- [ ] **Step 7: Build and test**

```bash
go build ./...
```

Test: Navigate job list, press Space on jobs — verify checkmarks appear. Press A C — verify batch cancel confirmation. Press Esc — verify selection clears.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/task_list.go internal/tui/app_keys.go internal/tui/app.go
git commit -m "ux: add batch job operations with Space-to-select to TUI"
```

---

### Task 19: Player Chat Offset Reset (Web UI only)

**Files:**
- Modify: `web/public/index.html:273` (offset input area)
- Modify: `web/public/modules/player.js:162-177` (offset handler)

- [ ] **Step 1: Add reset button next to offset input**

In `web/public/index.html`, find the chat offset input (around line 273). Wrap it with a reset button:

```html
<div style="display: flex; align-items: center; gap: 0.25em;">
    <input type="text" id="player-chat-offset" inputmode="decimal" placeholder="-0.0s offset" />
    <sl-icon-button id="player-chat-offset-reset" name="x-circle" label="Reset offset" style="display: none; font-size: 1rem;"></sl-icon-button>
</div>
```

- [ ] **Step 2: Wire reset button in player.js**

In `web/public/modules/player.js`, find the offset input handler (around line 162). After the offset is updated, toggle reset button visibility:

```javascript
const resetBtn = document.getElementById("player-chat-offset-reset");
if (resetBtn) {
    resetBtn.style.display = this.playerCustomOffsetMs !== 0 ? "" : "none";
}
```

Add click handler for the reset button:

```javascript
document.getElementById("player-chat-offset-reset")?.addEventListener("click", () => {
    const offsetInput = document.getElementById("player-chat-offset");
    if (offsetInput) offsetInput.value = "";
    this.playerCustomOffsetMs = 0;
    this.resetSidebarToTime(this.getCurrentTimeMs());
    document.getElementById("player-chat-offset-reset").style.display = "none";
});
```

- [ ] **Step 3: Build and test**

```bash
go build -o moombox.exe ./cmd/moombox
```

Test: Set a chat offset — verify reset button appears. Click it — verify offset clears and button hides.

- [ ] **Step 4: Commit**

```bash
git add web/public/index.html web/public/modules/player.js
git commit -m "ux: add chat offset reset button to player"
```

---

## Final Verification

### Task 20: Full Build and Smoke Test

- [ ] **Step 1: Full build**

```bash
go build ./...
go vet ./...
```

- [ ] **Step 2: Run tests**

```bash
go test ./...
```

- [ ] **Step 3: Visual smoke test**

Start Moombox and verify:
- Setup wizard: mode selection badges, cookie countdown, cookie success, FFmpeg skip
- Settings: unsaved banner, FFmpeg install button, template preview, parallel downloads help
- TUI: chord hints, auto-scroll indicator, action menu reasons, batch selection
- Web UI: log auto-scroll pill, error expand, batch operations, offset reset

- [ ] **Step 4: Final commit if any fixes needed**

```bash
git add -A
git commit -m "ux: final fixes from smoke testing"
```
