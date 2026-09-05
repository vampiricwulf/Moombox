# Arc F — Small Parity Features Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close eight Web↔TUI parity gaps: two Web buttons that already have server routes (re-scan feed history, copy stream URL) and six TUI actions the Web already offers (cookie-file import, toggle watched, orphans delete-all, skip an update, clear the log view, yt-dlp plugin status/install).

**Architecture:** Every server operation already exists; this arc adds UI. Web: two buttons in existing panels plus one pure helper in `utils.js`. TUI: three new chords (`R I`, `A W`, `R Y`) added to `buildMenuItems()`/`dispatchAction()` (the single source of truth), two new full-screen dialogs modelled on `client_tokens_dialog.go` (async: spinner → result), one new key in two existing overlays (`A` in the files dialog, `S` in the release-notes overlay) and one in the log panel (`c`). Two handler bodies move into exported `routes` functions so the TUI and the Web call the same code: `routes.DismissUpdate` and `routes.YtdlpPluginStatus`.

**Tech Stack:** Go 1.27.1, Bubble Tea v2 (`tea.KeyPressMsg`, bubbles `textinput`/`spinner`/`viewport`/`list`), vanilla ES modules + `node:test`, chi routes.

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §8 (Arc F) and §1 ruling Q9 (TUI cookie import = new chord `R I`, path input). Anchors below were read at main `e932633`/`88bcfbc`; Arc E (merged since) touched only `internal/engine` and `docs/spec/architecture.md`, so they hold — re-verify with `grep -n` before editing.

## Global Constraints

- Worktree `D:/Git/Moombox/.worktrees/improvement-f-parity`, branch `improvement-f-parity`, cut from `main`. Never push. Never `cd` to the main checkout. Git is read-only beyond `git add`/`git commit` (no stash/checkout/reset/clean); never edit a file you are not tasked with.
- Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. Implementers run only the packages their task names; the controller runs the one full `go test -count=1 ./...`.
- `gofmt -l ./cmd ./internal ./tools ./web` prints nothing; `go vet ./...` silent; `staticcheck ./...` (2026.2.1) prints nothing (hard CI gate). Go files stay LF (`perl -0777 -ne 'print tr/\r//' <file>` → 0).
- **Chord rules (CLAUDE.md):** a chord = one `ActionMenuItem` in `buildMenuItems()` (`internal/tui/app_actions.go:565`) + one `case` in `dispatchAction()`. A chord gated on a callback is appended only `if a.<Callback> != nil` (pattern: `R L` at `app_actions.go:626-628`), so a nil callback deletes it from dispatch, menu and help alike — every gated chord gets a symmetric wired/unwired test like `internal/tui/cookie_login_chord_test.go`. `TestHelpCoversEveryChord` (`help_coverage_test.go`) must stay green: categories are `Action`/`Request`/`Open`; single keys go in `help.go`'s static sections.
- **Every new full-screen dialog needs four coordinated additions:** (1) a field + constructor call on `App` (`app.go` struct ~:322, `NewApp()` ~:581); (2) a branch in `hasActiveOverlay()` (`app.go:909-922`); (3) a branch in `App.View()`'s overlay chain (`app_layout.go:94-127`, same order as key interception); (4) a branch in the `app_keys.go` dialog-intercept chain (~:227-233, before the `PanelLogs`/`PanelTasks` search intercepts at ~:311-334) AND in `routeComponentMsg` (`app_update.go:1186-1187`) for textinput/spinner routing.
- **JS:** `settings.js` has exactly one `import { ... } from "./utils.js";` block (`:4-17`); the goja harnesses in `internal/tui/settings_js_vm_test.go` and `internal/web/routes/cookies_lasterror_panel_test.go` strip only that block — a new helper used by `settings.js` goes INTO that block; never add a second import statement to `settings.js`. `app.js` may import from `./modules/utils.js` freely (its block is `:4-13`). Never log or toast cookie contents.
- Docs cited by `internal/docs/citations_test.go` (the six `docs/spec/*.md`) must keep every backticked path/symbol resolvable.
- Every commit ends with both trailers, exactly:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
  Write the message to a file and `git commit -F <file>` (never `-F -`).

---

### Task 1: Web — "Re-scan feed history" button and "Copy stream URL" button

**Files:**
- Modify: `web/public/index.html:1368-1369` (channels panel), `web/public/modules/settings.js:207-225` (button wiring) and its import block `:4-17`
- Modify: `web/public/app.js:2632-2636` (details dialog Video ID row) and the import block `:4-13`
- Modify: `web/public/modules/utils.js` (new export after `canResumeJob`, ~`:838`)
- Test: `web/tests/utils.test.mjs`
- Modify: `docs/spec/user-interfaces.md` Backfill table row `:564-568` (mention the button); `README.md` — no change (Web features are not tabled there)

**Interfaces:**
- Consumes: `POST /api/backfill/rescan` (`internal/web/routes/backfill.go:37`; responses `{"success":true}` / `{"success":false,"debounced":true,"retryAfterMs":N}` / 503 when no rescan is wired), `this.app.showToast(msg, variant)`, the details dialog's delegated `[data-copy]` click handler (`app.js:297-303`) and `copyTextToClipboard`.
- Produces: `export function streamUrl(job)` in `utils.js`.

- [ ] **Step 1: Write the failing test** — add to `web/tests/utils.test.mjs` (import `streamUrl` in the existing import list):

```js
// streamUrl mirrors internal/tui/app_actions.go streamURL (the TUI's O C
// chord): an explicit url wins; else YouTube watch URL; Twitch VOD strips the
// tw_v prefix; Twitch live needs a channel name.
test("streamUrl: explicit url wins over derivation", () => {
  assert.equal(streamUrl({ url: "https://example/x", videoId: "abc", platform: "youtube" }), "https://example/x");
});
test("streamUrl: youtube derives the watch URL", () => {
  assert.equal(streamUrl({ videoId: "dQw4w9WgXcQ", platform: "youtube" }), "https://www.youtube.com/watch?v=dQw4w9WgXcQ");
});
test("streamUrl: twitch VOD strips the tw_v prefix", () => {
  assert.equal(streamUrl({ videoId: "tw_v123456", platform: "twitch", isVod: true }), "https://www.twitch.tv/videos/123456");
});
test("streamUrl: twitch live is the channel page, empty without a channel", () => {
  assert.equal(streamUrl({ videoId: "live1", platform: "twitch", channelName: "somestreamer" }), "https://www.twitch.tv/somestreamer");
  assert.equal(streamUrl({ videoId: "live1", platform: "twitch" }), "");
  assert.equal(streamUrl({ platform: "youtube" }), "");
  assert.equal(streamUrl(null), "");
});
```

- [ ] **Step 2: Run** — `node --test web/tests/utils.test.mjs` → FAIL (`streamUrl` is not exported).

- [ ] **Step 3: `web/public/modules/utils.js`** — after `canResumeJob` add:

```js
/**
 * streamUrl is the JS twin of the TUI's streamURL (internal/tui/app_actions.go,
 * the O C chord): the job's own url when it has one, else derived from the
 * platform. Twitch VOD ids carry a "tw_v" prefix on the wire; Twitch live has
 * no id-addressable page, only the channel's. Empty string = nothing to copy.
 * @param {{url?: string, videoId?: string, platform?: string, isVod?: boolean, channelName?: string}|null} job
 * @returns {string}
 */
export function streamUrl(job) {
  if (!job) return "";
  if (job.url) return job.url;
  if (!job.videoId) return "";
  if (job.platform === "twitch") {
    if (job.isVod) return "https://www.twitch.tv/videos/" + job.videoId.replace(/^tw_v/, "");
    return job.channelName ? "https://www.twitch.tv/" + job.channelName : "";
  }
  return "https://www.youtube.com/watch?v=" + job.videoId;
}
```

- [ ] **Step 4: Run** — the four tests PASS.

- [ ] **Step 5: Copy stream URL button** — `web/public/app.js:2632-2636`: directly after the Video ID row's closing `</div>` (read the surrounding template to find the row boundary) add a sibling row, using the same classes:

```js
      ${streamUrl(job) ? `
      <div class="details-row">
        <span class="details-label">Stream URL:</span>
        <span class="details-value"><code>${this.escapeHtml(streamUrl(job))}</code><sl-icon-button class="details-copy-btn" name="clipboard" label="Copy stream URL" data-copy="${this.escapeHtml(streamUrl(job))}"></sl-icon-button></span>
      </div>` : ""}
```
(Match the exact wrapper element/class the Video ID row uses — if it is not `details-row`, use what it uses.) Add `streamUrl` to the `./modules/utils.js` import block at `app.js:4-13`. The existing `[data-copy]` delegate (`:297-303`) services the click; no new handler.

- [ ] **Step 6: Re-scan button** — `web/public/index.html:1369`: directly after the `add-channel-btn` `<sl-button …>…</sl-button>` element add:
```html
            <sl-button id="rescan-feeds-btn" title="Force a full-catalog backfill re-scan of every configured YouTube channel (same as the TUI's R B)">Re-scan Feed History</sl-button>
```
`web/public/modules/settings.js` — beside the `add-channel-btn` wiring (`:207-210`) add:
```js
    const rescanBtn = document.getElementById("rescan-feeds-btn");
    if (rescanBtn) {
      rescanBtn.addEventListener("click", () => this.rescanFeedHistory());
    }
```
and add the method to `SettingsController` (near the other channel actions):
```js
  /**
   * Force a full-catalog backfill re-scan — the Web twin of the TUI's R B.
   * Same fire-and-forget shape as app.checkMonitorsNow(): the server debounces
   * (30 s) and answers {debounced, retryAfterMs}; nothing is throttled here.
   */
  async rescanFeedHistory() {
    try {
      const resp = await fetch("/api/backfill/rescan", { method: "POST" });
      const data = await resp.json().catch(() => ({}));
      if (data.debounced) {
        this.app.showToast(`Just re-scanned — try again in ${Math.ceil((data.retryAfterMs || 0) / 1000)}s`, "primary");
      } else if (resp.ok && data.success) {
        this.app.showToast("Re-scanning feed history…", "success");
      } else {
        this.app.showToast("Re-scan failed", "danger");
      }
    } catch {
      this.app.showToast("Re-scan failed: could not reach server", "danger");
    }
  }
```
(`this.app.showToast` is how `settings.js` toasts elsewhere — grep `this.app.showToast` to confirm; if the controller uses a different accessor, match it.)

- [ ] **Step 7: Source-pin test** — create `internal/web/routes/parity_buttons_test.go`:

```go
package routes

import (
	"strings"
	"testing"
)

// TestParityButtonsArePinned pins the two Arc F web buttons the way the
// logout icon is pinned (logout_wiring_test.go): the markup exists, the
// handler is wired, and the server route each one POSTs to is the one that
// exists. A mutant that drops the button or its listener passes node
// 100 %; this is what fails.
func TestParityButtonsArePinned(t *testing.T) {
	html := readEmbeddedModule(t, "public/index.html")
	settingsJS := readEmbeddedModule(t, "public/modules/settings.js")
	appJS := readEmbeddedModule(t, "public/app.js")

	if !strings.Contains(html, `id="rescan-feeds-btn"`) {
		t.Error("index.html lacks the rescan-feeds-btn")
	}
	if !strings.Contains(settingsJS, `document.getElementById("rescan-feeds-btn")`) {
		t.Error("settings.js does not wire rescan-feeds-btn")
	}
	if body := jsMethodBody(t, settingsJS, "rescanFeedHistory"); !strings.Contains(body, `fetch("/api/backfill/rescan", { method: "POST" })`) {
		t.Error("rescanFeedHistory does not POST /api/backfill/rescan")
	}
	if !strings.Contains(appJS, `import {`) || !strings.Contains(appJS, "streamUrl") {
		t.Error("app.js does not import streamUrl")
	}
	if !strings.Contains(appJS, `data-copy="${this.escapeHtml(streamUrl(job))}"`) {
		t.Error("the details dialog has no Copy stream URL button")
	}
}
```
(`readEmbeddedModule` — `cookies_setup_threestate_test.go:25`; `jsMethodBody` — `cookies_indicator_test.go:519`, zero-arg headers; `rescanFeedHistory()` is zero-arg.)

- [ ] **Step 8: Docs** — `docs/spec/user-interfaces.md` Backfill table (`:564-568`): the `POST /api/backfill/rescan` row's description gains ` Web: the "Re-scan Feed History" button in the Settings → Channels panel (\`rescan-feeds-btn\`, \`settings.js\` \`rescanFeedHistory\`).` In the job-details description (grep `Copy` near the details dialog prose, or the `details-copy-btn`) add one sentence: `A "Stream URL" row with its own copy button (\`streamUrl\` in \`web/public/modules/utils.js\`, the twin of the TUI's \`O C\`) appears whenever the job has or can derive a page URL.`

- [ ] **Step 9: Gates**
```bash
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
(cd web/tests && node --test *.test.mjs 2>&1 | grep -E '^ℹ (tests|pass|fail)')
go test -count=1 ./internal/web/routes/ ./internal/tui/ ./internal/docs/
```
Expected: `tests 113 / pass 113 / fail 0` (109 + 4); three `ok` (tui runs its goja harness over settings.js — proves the import block is still the only import).

- [ ] **Step 10: Commit**
```
feat(web): Re-scan Feed History button and a Copy stream URL row

Both twins of TUI chords (R B, O C) against routes that already existed.
streamUrl mirrors streamURL case for case; a routes test pins the wiring.
```

---

### Task 2: TUI — `R I` Import Cookie File dialog

**Files:**
- Create: `internal/tui/cookie_import_dialog.go`, `internal/tui/cookie_import_dialog_test.go`
- Modify: `internal/tui/app.go` (field `cookieImportDlg *CookieImportDialogModel`, callback `OnImportCookieFile func(path string) (cookies.ImportResult, error)`, `NewApp()`, `hasActiveOverlay()`), `app_layout.go` (View chain), `app_keys.go` (intercept), `app_update.go` (result msg + `routeComponentMsg`), `app_actions.go` (`buildMenuItems` + `dispatchAction`), `app_commands.go` (the cmd)
- Modify: `cmd/moombox/tui_wiring.go` (wire the callback beside the cookie callbacks)
- Modify: `CLAUDE.md:109`, `README.md` Request table `:420-429`, `docs/spec/user-interfaces.md` Request table `:233-245` + Module/Overlay table `:906-914`

**Interfaces:**
- Consumes: `(*cookies.AutoCookieService).ImportCookies(ctx, netscape string) (cookies.ImportResult, error)` (`internal/cookies/cookie_import.go:332`); `cookies.ImportResult{YouTube, Twitch RefreshVerdict; YouTubeAccepted, TwitchAccepted bool; YouTubeOutcome, TwitchOutcome ImportOutcome; RollbackProtected, Wrote bool}` (`:250-294`); `ImportOutcome.String()` = `unknown|unchanged|imported|rolled-back|rejected`; `RefreshVerdict.String()`.
- Produces: `App.OnImportCookieFile`, `CookieImportDialogModel`, `cookieImportResultMsg`.

- [ ] **Step 1: Write the failing test** — `internal/tui/cookie_import_dialog_test.go`:

```go
package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

func typeInto(m *CookieImportDialogModel, s string) {
	for _, r := range s {
		m.HandleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// TestCookieImportDialogValidatesThePath: a missing file is refused inline
// (no callback), "~" expands to the home directory, a directory is refused.
func TestCookieImportDialogValidatesThePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(file, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewCookieImportDialogModel()
	m.Open()
	typeInto(m, filepath.Join(dir, "missing.txt"))
	if action, _ := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter}); action != "" {
		t.Fatalf("missing file must not import, got action %q", action)
	}
	if !strings.Contains(m.View(), "does not exist") {
		t.Errorf("no inline error for a missing file:\n%s", m.View())
	}
	m.Open()
	typeInto(m, dir)
	if action, _ := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter}); action != "" {
		t.Fatalf("a directory must not import, got %q", action)
	}
	m.Open()
	typeInto(m, file)
	action, path := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if action != "import" || path != file {
		t.Fatalf("existing file: action=%q path=%q", action, path)
	}
	home, _ := os.UserHomeDir()
	if got := expandHome("~/x/cookies.txt"); got != filepath.Join(home, "x", "cookies.txt") {
		t.Errorf("expandHome = %q", got)
	}
	if got := expandHome("relative/cookies.txt"); got != "relative/cookies.txt" {
		t.Errorf("expandHome must leave non-~ paths alone, got %q", got)
	}
}

// TestCookieImportDialogRendersEveryOutcome: the result is worded per platform
// off ImportOutcome.String() and the verdict — never flattened to pass/fail —
// and never includes cookie content.
func TestCookieImportDialogRendersEveryOutcome(t *testing.T) {
	m := NewCookieImportDialogModel()
	m.Open()
	m.SetImporting()
	if !strings.Contains(m.View(), "Importing") {
		t.Errorf("importing step not rendered:\n%s", m.View())
	}
	m.SetResult(cookies.ImportResult{
		YouTubeOutcome: cookies.ImportInstalled, YouTubeAccepted: true,
		TwitchOutcome: cookies.ImportRolledBack, RollbackProtected: true,
	}, nil)
	v := m.View()
	for _, want := range []string{"YouTube", "imported", "Twitch", "rolled-back"} {
		if !strings.Contains(v, want) {
			t.Errorf("result view lacks %q:\n%s", want, v)
		}
	}
	m.SetResult(cookies.ImportResult{}, errors.New("cookies.txt: not a Netscape cookie file"))
	if !strings.Contains(m.View(), "not a Netscape cookie file") {
		t.Errorf("error not rendered:\n%s", m.View())
	}
	if action, _ := m.HandleKey(tea.KeyPressMsg{Code: tea.KeyEscape}); action != "close" || m.IsVisible() {
		t.Fatalf("Esc must close the dialog (action %q, visible %v)", action, m.IsVisible())
	}
}

// TestImportCookieChordExistsOnlyWhenWired: nil callback ⇒ no R I in
// dispatch, menu or help; wired ⇒ all three, and dispatch opens the dialog.
func TestImportCookieChordExistsOnlyWhenWired(t *testing.T) {
	app := NewApp()
	app.OnImportCookieFile = nil
	if riOffered(app) {
		t.Fatal("R I offered with no callback")
	}
	app.OnImportCookieFile = func(string) (cookies.ImportResult, error) { return cookies.ImportResult{}, nil }
	if !riOffered(app) {
		t.Fatal("R I missing with the callback wired")
	}
	app.dispatchAction("R I", nil)
	if !app.cookieImportDlg.IsVisible() {
		t.Fatal("dispatching R I did not open the dialog")
	}
}

func riOffered(app *App) bool {
	for _, it := range app.buildMenuItems() {
		if it.Chord == "R I" {
			return true
		}
	}
	return false
}
```
Check the Bubble Tea import path and key constants the package already uses (`grep -n 'charm.land/bubbletea\|tea.KeyEnter\|tea.KeyEscape\|keyEnter\|keyEsc' internal/tui/files_dialog_test.go internal/tui/task_search_test.go`) and use exactly those forms; `NewApp()` is how `cookie_forcerefresh_chord_test.go` builds the app.

- [ ] **Step 2: Run** — `go test -count=1 -run 'CookieImport|ImportCookieChord' ./internal/tui/` → FAIL to compile.

- [ ] **Step 3: `internal/tui/cookie_import_dialog.go`** — model the shape on `client_tokens_dialog.go` (Open/loading/spinner/View three-way branch) plus a `textinput`:

```go
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/vampiricwulf/Moombox/internal/cookies"
)

// CookieImportDialogModel is the R I overlay: a path prompt, then the import
// runs through App.OnImportCookieFile, then the per-platform outcome is shown
// in place. It never sees cookie content — the path is the only thing typed
// or displayed, and the result is worded off ImportOutcome/RefreshVerdict.
type CookieImportDialogModel struct {
	visible       bool
	width, height int
	step          int // 0 = path input, 1 = importing, 2 = result
	input         textinput.Model
	errorMsg      string
	spinner       spinner.Model
	result        cookies.ImportResult
	resultErr     error
}

func NewCookieImportDialogModel() *CookieImportDialogModel {
	ti := textinput.New()
	ti.Placeholder = "~/Downloads/cookies.txt"
	ti.Prompt = "> "
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	return &CookieImportDialogModel{input: ti, spinner: sp}
}

func (m *CookieImportDialogModel) IsVisible() bool { return m.visible }

func (m *CookieImportDialogModel) SetSize(w, h int) {
	m.width, m.height = w, h
	m.input.SetWidth(max(min(w-8, 100), 20))
}

// Open resets to the path prompt.
func (m *CookieImportDialogModel) Open() tea.Cmd {
	m.visible = true
	m.step = 0
	m.errorMsg = ""
	m.result = cookies.ImportResult{}
	m.resultErr = nil
	m.input.SetValue("")
	return m.input.Focus()
}

func (m *CookieImportDialogModel) Close() { m.visible = false; m.input.Blur() }

// SetImporting moves to the spinner step (the App fires the cmd).
func (m *CookieImportDialogModel) SetImporting() { m.step = 1; m.errorMsg = "" }

// SpinnerInit starts the spinner ticking while importing.
func (m *CookieImportDialogModel) SpinnerInit() tea.Cmd { return m.spinner.Tick }

// SetResult renders the outcome (or the error) in place; Esc closes.
func (m *CookieImportDialogModel) SetResult(r cookies.ImportResult, err error) {
	m.step = 2
	m.result, m.resultErr = r, err
}

// expandHome turns a leading "~" (alone, "~/…" or "~\…") into the home
// directory; anything else is returned unchanged. Nothing else in the tree
// expands "~", and bubbles' filepicker does not either.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// HandleKey returns ("import", path) when a valid file was confirmed,
// ("close", "") when the overlay closed, ("", "") otherwise.
func (m *CookieImportDialogModel) HandleKey(msg tea.KeyPressMsg) (string, string) {
	key := msg.String()
	if key == "esc" || (m.step == 2 && (key == "q" || key == "enter")) {
		m.Close()
		return "close", ""
	}
	switch m.step {
	case 0:
		if key == "enter" {
			path := expandHome(strings.TrimSpace(m.input.Value()))
			if path == "" {
				m.errorMsg = "Enter the path to a Netscape cookies.txt"
				return "", ""
			}
			info, err := os.Stat(path)
			switch {
			case err != nil:
				m.errorMsg = fmt.Sprintf("%s does not exist", path)
				return "", ""
			case info.IsDir():
				m.errorMsg = fmt.Sprintf("%s is a directory, not a cookie file", path)
				return "", ""
			}
			m.errorMsg = ""
			return "import", path
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		_ = cmd
	case 1:
		// importing — only Esc (handled above) does anything
	}
	return "", ""
}

// UpdateComponents routes spinner/textinput ticks.
func (m *CookieImportDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if m.step == 1 {
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}
	m.input, cmd = m.input.Update(msg)
	return cmd
}

func (m *CookieImportDialogModel) View() string {
	var b strings.Builder
	b.WriteString(TitleStyle.Render("Import Cookie File") + "\n\n")
	switch m.step {
	case 0:
		b.WriteString("Path to a Netscape-format cookies.txt (exported from your browser):\n")
		b.WriteString(m.input.View() + "\n")
		if m.errorMsg != "" {
			b.WriteString("\n" + ErrorStyle.Render(m.errorMsg) + "\n")
		}
		b.WriteString("\n" + HelpStyle.Render("Enter: Import  Esc: Cancel"))
	case 1:
		b.WriteString(m.spinner.View() + " Importing and verifying each platform...\n")
		b.WriteString("\n" + HelpStyle.Render("Esc: Close (the import keeps running)"))
	case 2:
		if m.resultErr != nil {
			b.WriteString(ErrorStyle.Render("Import failed: "+m.resultErr.Error()) + "\n")
		} else {
			b.WriteString(platformOutcomeLine("YouTube", m.result.YouTubeOutcome, m.result.YouTube, m.result.YouTubeAccepted) + "\n")
			b.WriteString(platformOutcomeLine("Twitch", m.result.TwitchOutcome, m.result.Twitch, m.result.TwitchAccepted) + "\n")
			if m.result.RollbackProtected {
				b.WriteString(HelpStyle.Render("A platform that stopped authenticating kept its previous cookies (rolled back).") + "\n")
			}
		}
		b.WriteString("\n" + HelpStyle.Render("Esc/Enter: Close"))
	}
	return dialogBox(b.String(), m.width, m.height)
}

// platformOutcomeLine words one platform's result off the two facts the
// import reports — what happened to the rows (outcome) and whether the
// result authenticates (verdict/accepted) — never a single pass/fail.
func platformOutcomeLine(name string, outcome cookies.ImportOutcome, verdict cookies.RefreshVerdict, accepted bool) string {
	auth := verdict.String()
	if accepted {
		auth = "authenticates"
	}
	return fmt.Sprintf("  %-8s %s (%s)", name+":", outcome.String(), auth)
}
```
Adapt to the package's real names: `TitleStyle`/`ErrorStyle`/`HelpStyle` and the box helper the other dialogs use (`grep -n 'func dialogBox\|func renderDialog\|lipgloss.Place' internal/tui/client_tokens_dialog.go internal/tui/files_dialog.go`) — use those; if `RefreshVerdict` has no `String()`, grep `func (v RefreshVerdict) String` and use whatever it exposes. Keep every identifier the test references (`NewCookieImportDialogModel`, `Open`, `SetImporting`, `SetResult`, `HandleKey`, `IsVisible`, `View`, `expandHome`).

- [ ] **Step 4: App wiring** (the four coordinated additions + chord):
1. `app.go`: field `cookieImportDlg *CookieImportDialogModel` beside `importDlg` (~`:322-325`); callback beside the cookie callbacks (~`:489`):
   ```go
   	// OnImportCookieFile imports a Netscape cookies.txt from disk through
   	// AutoCookieService.ImportCookies (the R I chord). Nil deletes the chord.
   	OnImportCookieFile func(path string) (cookies.ImportResult, error)
   ```
   `NewApp()` (~`:581`): `cookieImportDlg: NewCookieImportDialogModel(),` (or the assignment form the constructor uses). `hasActiveOverlay()` (`:909-922`): add `|| a.cookieImportDlg.IsVisible()`.
2. `app_layout.go:113-114`: after the `importDlg` branch add `if a.cookieImportDlg.IsVisible() { return a.viewWithMode(a.cookieImportDlg.View()) }`.
3. `app_keys.go:227-233`: after the `importDlg` intercept add:
   ```go
   	if a.cookieImportDlg.IsVisible() {
   		action, path := a.cookieImportDlg.HandleKey(msg)
   		if action == "import" {
   			a.cookieImportDlg.SetImporting()
   			return a, tea.Batch(a.importCookieFileCmd(path), a.cookieImportDlg.SpinnerInit())
   		}
   		return a, nil
   	}
   ```
   (`msg` is the `tea.KeyPressMsg`; match how the `importDlg` intercept obtains `key`/`msg`.)
4. `app_update.go:1186-1187` `routeComponentMsg`: `if a.cookieImportDlg.IsVisible() { return a.cookieImportDlg.UpdateComponents(msg) }`. Add the result arm near `importResultMsg` (`:477-485`):
   ```go
   	case cookieImportResultMsg:
   		if !a.cookieImportDlg.IsVisible() {
   			return a, nil // closed while importing — the file is imported either way
   		}
   		a.cookieImportDlg.SetResult(msg.Result, msg.Err)
   		return a, nil
   ```
   with, in `app.go` beside `importResultMsg` (`:123-126`): `type cookieImportResultMsg struct { Result cookies.ImportResult; Err error }`.
5. `app_commands.go` beside `importFileCmd` (`:215`):
   ```go
   // importCookieFileCmd runs OnImportCookieFile off the UI goroutine. The
   // path is the only thing that ever reaches a log line — never the file.
   func (a *App) importCookieFileCmd(path string) tea.Cmd {
   	fn := a.OnImportCookieFile
   	return safeCmd(func() tea.Msg {
   		res, err := fn(path)
   		return cookieImportResultMsg{Result: res, Err: err}
   	})
   }
   ```
6. `app_actions.go` `buildMenuItems()` (after the `R L` block `:626-628`):
   ```go
   	if a.OnImportCookieFile != nil {
   		items = append(items, ActionMenuItem{Chord: "R I", Label: "Import Cookie File", HintLabel: "Import Cookies", Category: "Request"})
   	}
   ```
   `dispatchAction`: 
   ```go
   	case "R I":
   		if a.OnImportCookieFile == nil {
   			a.setFeedback("Cookie import is unavailable — no auto-cookie service is configured")
   			return a, nil
   		}
   		a.clearFeedback()
   		a.cookieImportDlg.SetSize(a.width, a.height)
   		return a, a.cookieImportDlg.Open()
   ```

- [ ] **Step 5: `cmd/moombox/tui_wiring.go`** — beside the other cookie callbacks (grep `OnForceRefreshCookies =`), gated the same way they are on the auto-cookie service existing:
```go
	app.OnImportCookieFile = func(path string) (cookies.ImportResult, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return cookies.ImportResult{}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return s.autoCookieSvc.ImportCookies(ctx, string(data))
	}
```
(use the service field name `tui_wiring.go` already uses for the auto-cookie service; if the Web import route wraps `ImportCookies` in a deferred re-check or in a specific timeout — read `internal/web/routes/cookies.go:597-698` — mirror that timeout.)

- [ ] **Step 6: Run the tests** — Step 2's command → PASS; then `go test -count=1 ./internal/tui/` → `ok` (incl. `TestHelpCoversEveryChord`: `R I` is category `Request`, covered by `sectionsFromMenu`).

- [ ] **Step 7: Docs** — `CLAUDE.md:109`: in the **R** chords sentence add `` `R I` (Import Cookie File — path prompt, imports a Netscape cookies.txt through the same verify-and-roll-back path as the Web import) `` after the `R L` item. `README.md` Request table (`:420-429`): row `| R I | Import cookie file (path prompt) |`; while there, backfill the four missing chords the table never had: Request `| R B | Re-scan feed history |`, `| R L | Cookie login (browser) |`; Open `| O C | Copy stream URL |`, `| O G | Open GitHub page |` (check each exists in `buildMenuItems()` first). `docs/spec/user-interfaces.md` Request table (`:233-245`): `| \`R I\` | Import Cookie File | Import callback is configured (auto-cookie service present) |`; Module/Overlay table (`:906-914`): `| Cookie import | \`settings.js\` import panel | \`CookieImportDialogModel\` (\`internal/tui/cookie_import_dialog.go\`) |`.

- [ ] **Step 8: Gates**
```bash
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./cmd ./internal && go build ./... && go vet ./... && staticcheck ./...
go test -count=1 ./internal/tui/ ./internal/docs/ ./cmd/moombox/
```
Expected: silent statics; three `ok`.

- [ ] **Step 9: Commit**
```
feat(tui): R I imports a cookie file through the verify-and-roll-back path

A path prompt (with ~ expansion and an existence check), then
AutoCookieService.ImportCookies, then the per-platform outcome — imported /
unchanged / rolled-back / rejected — rendered in the overlay. Cookie content
never reaches a log line. Nil callback deletes the chord (pinned).
```

---

### Task 3: TUI — `A W` Toggle Watched with a row glyph

**Files:**
- Modify: `internal/tui/app.go` (callback), `app_actions.go` (`buildMenuItems` + `dispatchAction`), `app_update.go` (result msg), `task_list.go:573-587` (`titleWidth`) and `:1112-1128` (`renderJob`)
- Create: `internal/tui/watched_test.go`
- Modify: `cmd/moombox/tui_wiring.go`
- Modify: `CLAUDE.md:109`, `README.md` Action table `:407-418`, `docs/spec/user-interfaces.md` Action table `:220-231`

**Interfaces:**
- Consumes: `db.UpdateJobFields(id, map[string]any{"watched": 1|0, "resume_position": nil})` (single job — mirrors `internal/web/routes/watch.go:74-99`; fires `OnJobUpdate`), `db.BatchSetWatched(ids []string, watched bool) error` (`internal/database/database_jobs.go:184`; Finished only; fires `OnJobsChange`), `Job.Watched bool` (`types.go:119`).
- Produces: `App.OnSetWatched func(ids []string, watched bool) error`; `watchedGlyph` rendering.

Toggle semantics (ruling): a single job flips its own `Watched`; a batch selection becomes watched unless EVERY selected Finished job is already watched, in which case all become unwatched.

- [ ] **Step 1: Write the failing tests** — `internal/tui/watched_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestWatchedGlyphIsCountedInTheRow: a watched Finished job shows the dim •
// between the platform tag and the title, the title budget shrinks by the
// glyph's width, and the rendered row never exceeds the list width.
func TestWatchedGlyphIsCountedInTheRow(t *testing.T) {
	m := NewTaskListModel()
	m.SetSize(60, 10)
	long := strings.Repeat("Title ", 20)
	plain := &database.Job{ID: "a", Title: long, Status: database.StatusFinished, Platform: "youtube"}
	watched := &database.Job{ID: "b", Title: long, Status: database.StatusFinished, Platform: "youtube", Watched: true}

	if got, want := m.titleWidth(plain)-m.titleWidth(watched), watchedGlyphWidth; got != want {
		t.Fatalf("title budget shrank by %d, want %d", got, want)
	}
	rowPlain := m.renderJob(plain, false, false, 58)
	rowWatched := m.renderJob(watched, false, false, 58)
	if strings.Contains(stripANSI(rowPlain), watchedGlyph) {
		t.Errorf("unwatched row carries the glyph: %q", rowPlain)
	}
	if !strings.Contains(stripANSI(rowWatched), watchedGlyph) {
		t.Errorf("watched row lacks the glyph: %q", rowWatched)
	}
	for _, row := range []string{rowPlain, rowWatched} {
		for _, line := range strings.Split(row, "\n") {
			if w := runewidth.StringWidth(stripANSI(line)); w > 58 {
				t.Errorf("row wraps: width %d > 58: %q", w, line)
			}
		}
	}
}

// TestToggleWatchedChord: A W is a batch-capable Action chord filtered to
// Finished jobs; dispatch calls OnSetWatched with the flipped value.
func TestToggleWatchedChord(t *testing.T) {
	app := NewApp()
	var gotIDs []string
	var gotWatched bool
	app.OnSetWatched = func(ids []string, watched bool) error { gotIDs, gotWatched = ids, watched; return nil }
	var item *ActionMenuItem
	for i := range app.buildMenuItems() {
		if it := app.buildMenuItems()[i]; it.Chord == "A W" {
			item = &it
		}
	}
	if item == nil || !item.NeedsJob || !item.SupportsBatch {
		t.Fatalf("A W missing or not a batch job chord: %+v", item)
	}
	if item.JobFilter(&database.Job{Status: database.StatusDownloading}) || !item.JobFilter(&database.Job{Status: database.StatusFinished}) {
		t.Fatal("A W must filter to Finished jobs")
	}
	job := &database.Job{ID: "j1", Status: database.StatusFinished, Watched: false}
	app.dispatchAction("A W", job)
	if len(gotIDs) != 1 || gotIDs[0] != "j1" || gotWatched != true {
		t.Fatalf("dispatch = (%v, %v), want ([j1], true)", gotIDs, gotWatched)
	}
	job.Watched = true
	app.dispatchAction("A W", job)
	if gotWatched != false {
		t.Fatal("a watched job must toggle to unwatched")
	}
}
```
Use the package's real constructor/sizing names (`NewTaskListModel`, `SetSize`) and an existing ANSI-stripping helper if one exists (`grep -n 'func stripANSI\|ansi.Strip\|StripANSI' internal/tui/*.go`); the `renderJob` signature is `(job *database.Job, selected bool, archived bool, maxW int) string` (`task_list.go:1036`). If the dispatch test cannot run without a result-message round trip (the dispatch returns a `tea.Cmd`), execute the returned cmd: `if cmd != nil { cmd() }`.

- [ ] **Step 2: Run** — FAIL to compile (`watchedGlyph`, `OnSetWatched`).

- [ ] **Step 3: `task_list.go`** — near the other row constants:
```go
// watchedGlyph marks a Finished job the operator has watched (the Web UI's
// eye badge). Dim, one cell plus a space, counted in titleWidth so the row
// never wraps.
const (
	watchedGlyph      = "•"
	watchedGlyphWidth = 2 // "• "
)
```
`titleWidth` (`:573-587`): add `watchedW := 0; if job.Watched { watchedW = watchedGlyphWidth }` and subtract `watchedW` in the `tw := max(...)` expression. `renderJob` (`:1112-1128`): right after the platform-tag block and before the title block:
```go
	// Watched glyph — same position titleWidth accounts for it.
	if job.Watched {
		glyphStyle := taskDimStyle // whichever dim/faint style the file already defines for muted text
		if dimmed {
			glyphStyle = glyphStyle.Faint(true)
		}
		parts = append(parts, glyphStyle.Render(watchedGlyph+" "))
	}
```
(use the file's existing muted style name; grep `Faint(true)` in `task_list.go`).

- [ ] **Step 4: Chord + callback**
- `app.go` beside `OnDeleteJob`: `OnSetWatched func(ids []string, watched bool) error` with a doc comment (`// OnSetWatched marks jobs watched/unwatched (the A W chord); the Web's /watched routes are the twin.`); result type near the other result msgs: `type setWatchedResultMsg struct { Count int; Watched bool; Err error }`.
- `buildMenuItems()`: after the `A D` entry:
  ```go
  		{Chord: "A W", Label: "Toggle Watched", HintLabel: "Watched", Category: "Action", NeedsJob: true, SupportsBatch: true,
  			DisabledReason: "no finished jobs",
  			JobFilter: func(j *database.Job) bool { return j.Status == database.StatusFinished }},
  ```
  (only `if a.OnSetWatched != nil`? — the Action entries above are not callback-gated in the same way; follow how `A D` handles a nil `OnDeleteJob` in dispatch: it checks the callback at dispatch time. Do the same: unconditional menu entry, nil-check in dispatch with a feedback line.)
- `dispatchAction`, modelled on `A D` (`:217-241`):
  ```go
  	case "A W":
  		if a.OnSetWatched == nil {
  			a.setFeedback("Watched toggling is unavailable")
  			return a, nil
  		}
  		setFn := a.OnSetWatched
  		if job == nil && a.taskList.SelectedCount() > 0 {
  			ids := a.taskList.SelectedIDs()
  			allWatched := true
  			finished := ids[:0]
  			for _, id := range ids {
  				if j := a.taskList.GetJobByID(id); j != nil && j.Status == database.StatusFinished {
  					finished = append(finished, id)
  					if !j.Watched {
  						allWatched = false
  					}
  				}
  			}
  			if len(finished) == 0 {
  				a.setFeedback("No finished jobs in selection")
  				return a, nil
  			}
  			a.taskList.ClearSelection()
  			watched := !allWatched
  			return a, safeCmd(func() tea.Msg {
  				return setWatchedResultMsg{Count: len(finished), Watched: watched, Err: setFn(finished, watched)}
  			})
  		} else if job != nil && job.Status == database.StatusFinished {
  			watched := !job.Watched
  			id := job.ID
  			return a, safeCmd(func() tea.Msg {
  				return setWatchedResultMsg{Count: 1, Watched: watched, Err: setFn([]string{id}, watched)}
  			})
  		}
  ```
  (`GetJobByID` is how `A R`/`A I` resolve ids — `:141-154`.)
- `app_update.go` beside `deleteJobsResultMsg`:
  ```go
  	case setWatchedResultMsg:
  		if msg.Err != nil {
  			a.setFeedback("Watched update failed: " + msg.Err.Error())
  			return a, nil
  		}
  		verb := "Marked"
  		if !msg.Watched {
  			verb = "Unmarked"
  		}
  		a.setFeedback(fmt.Sprintf("%s %d job(s) watched", verb, msg.Count))
  		return a, nil
  ```
  (The row updates arrive through the normal DB subscriber path — `UpdateJobFields` fires `OnJobUpdate`, `BatchSetWatched` fires `OnJobsChange`.)
- `cmd/moombox/tui_wiring.go` beside `app.OnDeleteJob =`:
  ```go
  	app.OnSetWatched = func(ids []string, watched bool) error {
  		if len(ids) == 1 {
  			// Single job: the per-job update path (OnJobUpdate), exactly what
  			// the web's POST/DELETE /api/jobs/{id}/watched do.
  			fields := map[string]any{"watched": 0}
  			if watched {
  				fields = map[string]any{"watched": 1, "resume_position": nil}
  			}
  			if s.db.UpdateJobFields(ids[0], fields) == nil {
  				return fmt.Errorf("job %s not found", ids[0])
  			}
  			return nil
  		}
  		return s.db.BatchSetWatched(ids, watched)
  	}
  ```
  Read `internal/web/routes/watch.go:74-99` first and mirror EXACTLY which fields each of the two single-job routes writes (the unwatched route may or may not clear `resume_position`); the comment above must state the truth.

- [ ] **Step 5: Tests** — Step 1's tests PASS; `go test -count=1 ./internal/tui/` `ok` (help coverage: `A W` is `Action`).

- [ ] **Step 6: Docs** — `CLAUDE.md:109`: add to the **A** prefix mention `` `A W` (Toggle Watched — batch-capable, Finished jobs; the row shows a dim `•`) ``. `README.md` Action table: `| A W | Toggle watched (Finished jobs) |`. `docs/spec/user-interfaces.md` Action table (`:220-231`): `| \`A W\` | Toggle Watched | Yes | No | Status is Finished |`; in the TUI task-list description (grep `[TW]` or `platform tag`) one sentence: `A watched job carries a dim \`•\` between the platform tag and the title (\`watchedGlyph\`, counted in \`titleWidth\`).`

- [ ] **Step 7: Gates** — as Task 2 Step 8. **Commit:**
```
feat(tui): A W toggles watched, with a dim • on the row

Batch-capable over Finished jobs (a selection becomes watched unless every
job already is). Single jobs go through UpdateJobFields like the web routes;
batches through BatchSetWatched. The glyph is counted in titleWidth so the
row cannot wrap (pinned by the first row-width test).
```

---

### Task 4: TUI — orphans dialog `A` deletes every entry in the current section

**Files:**
- Modify: `internal/tui/files_dialog.go` (model fields `:163-183`, `HandleKey` `:420-458`, `View` footer), `app_keys.go` (the files-dialog intercept: new action strings), `app_commands.go:367-405` (new cmd), `app_update.go:541-565` (new result msg), `app.go` (msg type)
- Test: `internal/tui/files_dialog_test.go` (extend)
- Modify: `docs/spec/user-interfaces.md` (the files/orphans dialog description — grep `A O` / `Orphaned`)

**Interfaces:** Consumes the existing per-item callbacks `OnDeleteOrphan(path) error` / `OnDeleteHistoryEntry(videoID) error` (`app.go:490-494`) and the dialog's refresh cmd. "Section" (ruling) = the half the cursor is in: the orphaned-files half or the orphaned-history half (`SelectedFile()` vs `SelectedHistory()`), not the staging/output/trim types.

- [ ] **Step 1: Write the failing test** — in `files_dialog_test.go`, following `TestFilesDialog_PagingClearsDeleteConfirm` (`:119-132`):

```go
// TestFilesDialog_DeleteAllInSectionNeedsTwoPresses: A arms a section-wide
// confirm distinct from the per-item one; a second A within the window
// returns the bulk action with every entry of THAT section; navigation
// disarms.
func TestFilesDialog_DeleteAllInSectionNeedsTwoPresses(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(80, 24)
	m.SetFiles([]OrphanedFileEntry{{Path: "/a/one.ts", RelPath: "one.ts", Type: "staging"}, {Path: "/a/two.ts", RelPath: "two.ts", Type: "output"}})
	m.SetHistory([]OrphanedHistoryEntry{{VideoID: "vid1"}})
	// cursor starts on the first file
	action, data := m.HandleKey(keyMsg("A"))
	if action != "" || !strings.Contains(m.feedbackMsg, "Press A again") {
		t.Fatalf("first A must arm, got action %q feedback %q", action, m.feedbackMsg)
	}
	m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.deleteAllArmed {
		t.Fatal("navigation must disarm the section confirm")
	}
	m.HandleKey(keyMsg("A"))
	action, data = m.HandleKey(keyMsg("A"))
	paths, ok := data.([]string)
	if action != "delete-all-files" || !ok || len(paths) != 2 {
		t.Fatalf("second A = (%q, %#v), want delete-all-files with both paths", action, data)
	}
	// move onto the history half and repeat
	m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m.HandleKey(keyMsg("A"))
	action, data = m.HandleKey(keyMsg("A"))
	ids, ok := data.([]string)
	if action != "delete-all-history" || !ok || len(ids) != 1 || ids[0] != "vid1" {
		t.Fatalf("history half: (%q, %#v)", action, data)
	}
}

// TestFilesDialog_BulkFailuresAreListed: the dialog shows how many were
// deleted and names the failures.
func TestFilesDialog_BulkFailuresAreListed(t *testing.T) {
	m := NewFilesDialogModel()
	m.SetSize(80, 24)
	m.SetBulkResult(3, []string{"two.ts: permission denied"})
	v := m.View()
	for _, want := range []string{"Deleted 3", "1 failed", "two.ts: permission denied"} {
		if !strings.Contains(v, want) {
			t.Errorf("view lacks %q:\n%s", want, v)
		}
	}
}
```
(`keyMsg` helper — `task_search_test.go:11-21`; if it is not shared with this file, copy it locally as `filesKey`. Use the dialog's real setter names for files/history — `SetFiles`/`SetHistory` or whatever `app_update.go:541-565` calls.)

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: `files_dialog.go`**
- Fields: `deleteAllArmed bool`, `deleteAllTimer time.Time`, `deleteAllSection string` (`"files"`/`"history"`).
- `HandleKey`: new arm before `case "d", "D":`:
  ```go
  	case "a", "A":
  		section, count := m.currentSection()
  		if count == 0 {
  			return "", nil
  		}
  		m.actionErr = ""
  		if m.deleteAllArmed && m.deleteAllSection == section && time.Now().Before(m.deleteAllTimer) {
  			m.deleteAllArmed = false
  			m.feedbackMsg = ""
  			if section == "files" {
  				paths := make([]string, 0, len(m.files))
  				for _, f := range m.files {
  					paths = append(paths, f.Path)
  				}
  				return "delete-all-files", paths
  			}
  			ids := make([]string, 0, len(m.history))
  			for _, h := range m.history {
  				ids = append(ids, h.VideoID)
  			}
  			return "delete-all-history", ids
  		}
  		m.deleteAllArmed, m.deleteAllSection = true, section
  		m.deleteAllTimer = time.Now().Add(3 * time.Second)
  		noun := "orphaned files"
  		if section == "history" {
  			noun = "history entries"
  		}
  		m.feedbackMsg = fmt.Sprintf("Press A again to delete all %d %s", count, noun)
  		return "", nil
  ```
  with helper:
  ```go
  // currentSection reports which half the cursor is in and how many entries
  // that half holds; "" when the cursor is on the divider.
  func (m *FilesDialogModel) currentSection() (string, int) {
  	if m.SelectedFile() != nil {
  		return "files", len(m.files)
  	}
  	if m.SelectedHistory() != nil {
  		return "history", len(m.history)
  	}
  	return "", 0
  }
  ```
  In the navigation branch that clears `deleteConfirmID` (`:452-458`) also clear `deleteAllArmed` and `deleteAllSection`.
- `SetBulkResult(deleted int, failures []string)`: sets `feedbackMsg = fmt.Sprintf("Deleted %d", deleted)` and, when failures exist, `actionErr = fmt.Sprintf("%d failed: %s", len(failures), strings.Join(failures, "; "))` (truncate to the first three failures with "…and N more" beyond that).
- Footer (`View`, the help line that lists `D: Delete`): add `A: Delete all in section`.

- [ ] **Step 4: App side** — `app_keys.go` files-dialog intercept: handle `"delete-all-files"` → `return a, tea.Batch(a.deleteAllOrphansCmd(data.([]string)), a.filesDlg.SpinnerInit())` and `"delete-all-history"` → `a.deleteAllHistoryCmd(data.([]string))`. `app_commands.go`:
```go
// deleteAllOrphansCmd loops the per-item callback, collecting failures so the
// dialog can name them; the list is refreshed afterwards by the result arm.
func (a *App) deleteAllOrphansCmd(paths []string) tea.Cmd {
	fn := a.OnDeleteOrphan
	return safeCmd(func() tea.Msg {
		var failures []string
		deleted := 0
		for _, p := range paths {
			if err := fn(p); err != nil {
				failures = append(failures, filepath.Base(p)+": "+err.Error())
				continue
			}
			deleted++
		}
		return bulkOrphanResultMsg{Deleted: deleted, Failures: failures}
	})
}
```
and the same shape `deleteAllHistoryCmd(ids)` over `OnDeleteHistoryEntry`. `app.go`: `type bulkOrphanResultMsg struct { Deleted int; Failures []string }`. `app_update.go` beside `deleteOrphanResultMsg` (`:541-565`): `case bulkOrphanResultMsg: a.filesDlg.SetBulkResult(msg.Deleted, msg.Failures); return a, <the same refresh cmd(s) the single-delete arm returns>`.

- [ ] **Step 5: Tests** — new tests PASS; `go test -count=1 ./internal/tui/` `ok`.

- [ ] **Step 6: Docs** — `docs/spec/user-interfaces.md`: in the files/orphans dialog description add `\`A\` deletes every entry in the half the cursor is in (files or history) after the same two-press confirm as \`D\`; per-item failures are collected and listed in the dialog.`

- [ ] **Step 7: Gates** — as Task 2 Step 8 (tui + docs). **Commit:**
```
feat(tui): A deletes every orphan in the current section

Two-press confirm like D, then the existing per-item callbacks in a loop;
failures are counted and named in the dialog instead of stopping the sweep.
```

---

### Task 5: Skip an update — `routes.DismissUpdate` shared by the Web route and a TUI `S` key

**Files:**
- Modify: `internal/web/routes/update.go:217-239` (extract), `internal/web/routes/update_test.go` (add a unit test for the helper)
- Modify: `internal/tui/release_notes_overlay.go:126` (footer), `app_keys.go:67-84` (the overlay's key arms), `app.go` (callback), `app_update.go` (result arm), `cmd/moombox/tui_wiring.go:290-333`
- Test: `internal/tui/release_notes_skip_test.go`
- Modify: `docs/spec/user-interfaces.md:242` (`R N` row) and `:767` (dismiss route row)

**Interfaces:**
- Produces: `func DismissUpdate(store *config.Store, tag string) error` in `routes` (writes `cfg.Updates.SkippedVersion = tag`, `store.SaveLocked()`, `SharedUpdateInfo.CompareAndSwap(pending, nil)` only when `pending.TagName == tag`); `App.OnDismissUpdate func(tag string) error`.

- [ ] **Step 1: Write the failing tests**
`internal/web/routes/update_test.go` — add (mirror the file's existing fixture for a `*config.Store` and `SharedUpdateInfo`):
```go
// TestDismissUpdateSkipsExactlyThePendingTag: the helper the route and the
// TUI share persists the skipped version and clears the shared pointer only
// when it still holds that tag — a newer release found meanwhile survives.
func TestDismissUpdateSkipsExactlyThePendingTag(t *testing.T) {
	store := newTestStore(t) // whatever this file's tests use to build a config.Store
	SharedUpdateInfo.Store(&updater.ReleaseInfo{TagName: "v9.9.9"})
	t.Cleanup(func() { SharedUpdateInfo.Store(nil) })
	if err := DismissUpdate(store, "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	if got := store.Config().Updates.SkippedVersion; got != "v9.9.9" {
		t.Errorf("SkippedVersion = %q", got)
	}
	if SharedUpdateInfo.Load() != nil {
		t.Error("pending pointer not cleared")
	}
	SharedUpdateInfo.Store(&updater.ReleaseInfo{TagName: "v10.0.0"})
	if err := DismissUpdate(store, "v9.9.9"); err != nil {
		t.Fatal(err)
	}
	if p := SharedUpdateInfo.Load(); p == nil || p.TagName != "v10.0.0" {
		t.Error("a newer pending release must survive a stale dismiss")
	}
}
```
`internal/tui/release_notes_skip_test.go`:
```go
package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestReleaseNotesSkipKey: S skips the pending version through
// OnDismissUpdate, clears updateAvailable and closes the overlay; with no
// update pending (notes of the current version) S does nothing and the
// footer does not offer it.
func TestReleaseNotesSkipKey(t *testing.T) {
	app := NewApp()
	skipped := ""
	app.OnDismissUpdate = func(tag string) error { skipped = tag; return nil }
	app.updateAvailable = &UpdateStatusMsg{Version: "9.9.9", TagName: "v9.9.9", ReleaseNotes: "notes"}
	app.releaseNotesPopup.open("v9.9.9", "notes", 80, 24)
	if !strings.Contains(app.releaseNotesPopup.View(), "S: Skip") {
		t.Fatal("footer must offer S while an update is pending")
	}
	_, cmd := app.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if cmd != nil {
		cmd()
	}
	if skipped != "v9.9.9" {
		t.Fatalf("OnDismissUpdate got %q", skipped)
	}
	// the result arm
	app.Update(dismissUpdateResultMsg{Tag: "v9.9.9"})
	if app.updateAvailable != nil || app.releaseNotesPopup.isOpen() {
		t.Fatal("skip must clear updateAvailable and close the overlay")
	}

	app.updateAvailable = nil
	skipped = ""
	app.releaseNotesPopup.open("v1.0.0", "current notes", 80, 24)
	if strings.Contains(app.releaseNotesPopup.View(), "S: Skip") {
		t.Fatal("footer must not offer S without a pending update")
	}
	app.handleKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	if skipped != "" {
		t.Fatal("S without a pending update must not call OnDismissUpdate")
	}
}
```
Use the overlay's real method names (`open`/`close`/`isOpen`, `release_notes_overlay.go:17-93`) and the App's real key entry point (`handleKey`? `Update(tea.KeyPressMsg)`? — grep `func (a *App) handleKey`); the overlay must learn whether an update is pending — give `open` a `pending bool` parameter or a setter, whichever is smaller.

- [ ] **Step 2: Run** — both FAIL to compile.

- [ ] **Step 3: `internal/web/routes/update.go`** — add above `UpdateRoutes`:
```go
// DismissUpdate records tag as the skipped version and clears the shared
// pending-update pointer if it still names that tag. Shared by the
// POST /api/update/dismiss route and the TUI's S key in the release-notes
// overlay. CompareAndSwap, not Store(nil): a newer release found while the
// dismiss was in flight must survive.
func DismissUpdate(store *config.Store, tag string) error {
	mu := store.RWMutex()
	cfg := store.Config()
	mu.Lock()
	old := cfg.Updates.SkippedVersion
	cfg.Updates.SkippedVersion = tag
	if err := store.SaveLocked(); err != nil {
		cfg.Updates.SkippedVersion = old
		mu.Unlock()
		return err
	}
	mu.Unlock()
	if pending := SharedUpdateInfo.Load(); pending != nil && pending.TagName == tag {
		SharedUpdateInfo.CompareAndSwap(pending, nil)
	}
	return nil
}
```
(Read the handler's exact locking/rollback at `:223-236` and keep its semantics — if it restores `oldVal` on save failure, so does the helper.) The route body becomes: load `pending`; 400 when nil; `if err := DismissUpdate(store, pending.TagName); err != nil { jsonError(w, "failed to save config", 500); return }`; respond `{"success": true, "skipped": pending.TagName}`. Existing route tests must still pass unchanged.

- [ ] **Step 4: TUI**
- `app.go` beside `OnApplyUpdate` (`:504`): `OnDismissUpdate func(tag string) error` (doc: `// OnDismissUpdate skips a pending version (the S key in the release-notes overlay); nil hides the key.`); msg `type dismissUpdateResultMsg struct { Tag string; Err error }`.
- `release_notes_overlay.go`: a `pending bool` field set by `open` (or `setPending`), footer `:126` → when pending and skippable: `"U: Apply update  S: Skip this version  ↑/↓: Scroll  Esc/Q: Close"`, else the current text.
- `app_keys.go:67-84` add an arm:
  ```go
  	case "s", "S":
  		if a.updateAvailable == nil || a.OnDismissUpdate == nil {
  			return a, nil
  		}
  		tag := a.updateAvailable.TagName
  		fn := a.OnDismissUpdate
  		a.setFeedback("Skipping " + tag + "...")
  		return a, safeCmd(func() tea.Msg { return dismissUpdateResultMsg{Tag: tag, Err: fn(tag)} })
  ```
- `app_update.go` beside `updateCheckResultMsg` (`:301-311`):
  ```go
  	case dismissUpdateResultMsg:
  		if msg.Err != nil {
  			a.setFeedback("Could not skip " + msg.Tag + ": " + msg.Err.Error())
  			return a, nil
  		}
  		if a.updateAvailable != nil && a.updateAvailable.TagName == msg.Tag {
  			a.updateAvailable = nil
  		}
  		a.releaseNotesPopup.close()
  		a.setFeedback("Skipped " + msg.Tag + " — you'll be notified about the next release")
  		return a, nil
  ```
  Where `R N` opens the overlay (`app_actions.go:317-343`) pass `a.updateAvailable != nil` as the pending flag. Check the status bar / `buildMenuItems` gates on `a.updateAvailable` (`R U`, the badge) — they must follow the cleared pointer without further work (they read it live).
- `cmd/moombox/tui_wiring.go` inside the `if s.upd != nil {` block: `app.OnDismissUpdate = func(tag string) error { return routes.DismissUpdate(s.configStore, tag) }` (use the store field name the file already uses).

- [ ] **Step 5: Tests** — both new tests PASS; `go test -count=1 ./internal/web/routes/ ./internal/tui/` ok.

- [ ] **Step 6: Docs** — `docs/spec/user-interfaces.md:242` (`R N` row): append ` \`S\` inside the overlay skips the pending version (\`OnDismissUpdate\` → \`routes.DismissUpdate\`, the same helper \`POST /api/update/dismiss\` uses).` `:767` (dismiss route row): append ` Body shared with the TUI via \`DismissUpdate\`.` `CLAUDE.md:109` `R N` parenthetical: add `, and \`S\` skips the pending version`.

- [ ] **Step 7: Gates** — statics + `go test -count=1 ./internal/web/routes/ ./internal/tui/ ./internal/docs/ ./cmd/moombox/`. **Commit:**
```
feat(update): S in the release-notes overlay skips the pending version

routes.DismissUpdate holds the body POST /api/update/dismiss had (skipped
version persisted; the shared pointer cleared only if it still names that
tag) and the TUI calls it too.
```

---

### Task 6: TUI — `c` clears the log view

**Files:**
- Modify: `internal/tui/log_viewer.go` (new `Clear()`), `app_keys.go:446-453` (`handleLogKey`), `help.go:36-45` (`navigationKeys`)
- Test: `internal/tui/log_viewer_test.go` (extend or create)
- Modify: `README.md` Quick Keys table `:439-449`, `docs/spec/user-interfaces.md` single-key table `:257-265`, `CLAUDE.md:109` single keys

- [ ] **Step 1: Write the failing test**
```go
// TestLogViewerClearEmptiesEverything: Clear drops the history, the filtered
// view, and any active search so the next AddLine starts a fresh view.
func TestLogViewerClearEmptiesEverything(t *testing.T) {
	m := NewLogViewerModel()
	m.SetSize(80, 20)
	m.AddLines([]string{"[INFO] one", "[WARN] two", "[INFO] three"})
	m.StartSearch()
	typeSearch(m, "two") // use the file's existing helper for typing into the search input, or send KeyPressMsgs
	m.HandleSearchKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Clear()
	if len(m.lines) != 0 || len(m.filtered) != 0 || m.searchQuery != "" || m.searchRegex != nil || m.matchCount != 0 {
		t.Fatalf("Clear left state: lines=%d filtered=%d query=%q", len(m.lines), len(m.filtered), m.searchQuery)
	}
	if strings.Contains(m.View(), "two") {
		t.Fatal("cleared line still rendered")
	}
	m.AddLine("[INFO] four")
	if !strings.Contains(m.View(), "four") {
		t.Fatal("AddLine after Clear must render")
	}
}
```
(Match the model's real constructor/method names from `log_viewer.go:47-130,318-386`.)

- [ ] **Step 2: Run** — FAIL (`Clear` undefined).

- [ ] **Step 3: `log_viewer.go`**
```go
// Clear empties the view: history, filtered lines, and any search. The
// level filter is kept — it is a preference, not content.
func (m *LogViewerModel) Clear() {
	m.lines = nil
	m.searching = false
	m.searchInput.SetValue("")
	m.searchQuery = ""
	m.searchRegex = nil
	m.matchCount = 0
	m.viewport.ClearHighlights()
	m.rebuildFiltered()
	m.viewport.GotoTop()
	m.autoScroll = true
}
```
`app_keys.go` `handleLogKey` (`:446-453`): add `case "c", "C": if !a.logs.IsSearching() { a.logs.Clear(); a.setFeedback("Log view cleared"); return a, nil }` — confirm `handleLogKey` runs only when `PanelLogs` has focus and after the search intercept (so `c` typed into the search box is not swallowed). `help.go` `navigationKeys`: `{"c", "Clear the log view (Logs)"}` beside `n / N`.

- [ ] **Step 4: Tests + docs** — `go test -count=1 ./internal/tui/` ok. `README.md` Quick Keys: `| c | Clear log view (log panel focused) |`; `docs/spec/user-interfaces.md` single-key table: `| \`c\` | Clear the log view (log panel focused) | — |` (match the table's columns); `CLAUDE.md:109` single keys list: add `**c** (Clear log view — log panel only)`.

- [ ] **Step 5: Gates** — statics + `go test -count=1 ./internal/tui/ ./internal/docs/`. **Commit:**
```
feat(tui): c clears the log view

History, filtered view and search go; the level filter stays. Help lists it.
```

---

### Task 7: TUI — `R Y` yt-dlp plugin overlay on a shared `routes.YtdlpPluginStatus`

**Files:**
- Modify: `internal/web/routes/ytdlp.go:33-72` (extract), `internal/web/routes/ytdlp_test.go` (unit test for the helper; existing route tests unchanged)
- Create: `internal/tui/ytdlp_dialog.go`, `internal/tui/ytdlp_dialog_test.go`
- Modify: `internal/tui/app.go` (field, two callbacks, msg types, `hasActiveOverlay`), `app_layout.go`, `app_keys.go`, `app_update.go` (`routeComponentMsg` + result arms), `app_actions.go` (chord), `app_commands.go` (cmds)
- Modify: `cmd/moombox/tui_wiring.go`
- Modify: `CLAUDE.md:109`, `README.md` Request table, `docs/spec/user-interfaces.md` Request table + `:787-792` yt-dlp rows + Module/Overlay table

**Interfaces:**
- Produces: 
  ```go
  // YtdlpPluginInfo is what GET /api/ytdlp-plugin/status reports and what the
  // TUI's R Y overlay shows.
  type YtdlpPluginInfo struct {
  	Installed     bool   `json:"installed"`
  	PluginDir     string `json:"pluginDir"`
  	CurrentPort   int    `json:"currentPort"`
  	HTTPSEnabled  bool   `json:"httpsEnabled"`
  	InstalledPort int    `json:"installedPort"`
  	PortMismatch  bool   `json:"portMismatch"`
  	ExtractedPath string `json:"extractedPath"`
  }
  func YtdlpPluginStatus(port int, httpsEnabled bool) (YtdlpPluginInfo, error)
  ```
  `App.OnYtdlpPluginStatus func() (routes.YtdlpPluginInfo, error)`, `App.OnInstallYtdlpPlugin func() error` (both zero-arg closures over the live port/https in `cmd/moombox`), `YtdlpDialogModel`.
- Consumes: `routes.InstallYtdlpPlugin(port int, httpsEnabled bool) error` (`ytdlp.go:136-147`).

- [ ] **Step 1: Write the failing tests**
`internal/web/routes/ytdlp_test.go` — add a test that calls `YtdlpPluginStatus(7740, false)` with the plugin dir redirected to a temp dir (read `ytdlp_test.go` for how the existing tests isolate `ytdlpPluginDir()` — an env var or an overridable func var) and asserts `Installed == false` on an empty dir, then after `InstallYtdlpPlugin(7740, false)` asserts `Installed == true`, `InstalledPort == 7740`, `PortMismatch == false`, and that `YtdlpPluginStatus(7741, false)` reports `PortMismatch == true`. Also assert the GET route's JSON still carries exactly the seven keys (existing test or a new one marshalling `YtdlpPluginInfo{}` and checking the key set).
`internal/tui/ytdlp_dialog_test.go`:
```go
// TestYtdlpDialogShowsStatusAndInstalls: the overlay loads through the
// status callback, renders installed/dir/port mismatch, and I runs the
// install callback then reloads.
func TestYtdlpDialogShowsStatusAndInstalls(t *testing.T) {
	app := NewApp()
	statusCalls, installCalls := 0, 0
	app.OnYtdlpPluginStatus = func() (routes.YtdlpPluginInfo, error) {
		statusCalls++
		return routes.YtdlpPluginInfo{Installed: statusCalls > 1, PluginDir: "/plug", CurrentPort: 7740, InstalledPort: 7739, PortMismatch: statusCalls == 1}, nil
	}
	app.OnInstallYtdlpPlugin = func() error { installCalls++; return nil }
	if !ryOffered(app) {
		t.Fatal("R Y missing with the callbacks wired")
	}
	_, cmd := app.dispatchAction("R Y", nil)
	if !app.ytdlpDlg.IsVisible() || !strings.Contains(app.ytdlpDlg.View(), "Loading") {
		t.Fatal("R Y must open the overlay in its loading state")
	}
	app.Update(runCmd(cmd)) // execute the batch and feed its status msg back
	v := app.ytdlpDlg.View()
	for _, want := range []string{"not installed", "/plug", "7740", "7739", "mismatch"} {
		if !strings.Contains(strings.ToLower(v), strings.ToLower(want)) {
			t.Errorf("status view lacks %q:\n%s", want, v)
		}
	}
	_, cmd = app.handleKey(tea.KeyPressMsg{Code: 'i', Text: "i"})
	app.Update(runCmd(cmd))
	if installCalls != 1 || statusCalls != 2 {
		t.Fatalf("install=%d status=%d, want 1 and 2 (install then reload)", installCalls, statusCalls)
	}
	if !strings.Contains(strings.ToLower(app.ytdlpDlg.View()), "installed") {
		t.Error("view not refreshed after install")
	}
	app.OnYtdlpPluginStatus = nil
	if ryOffered(app) {
		t.Fatal("R Y offered without the status callback")
	}
}
```
`runCmd` executes a `tea.Cmd` (unwrapping `tea.Batch` — look for an existing test helper that drains batch cmds, e.g. in `cookie_login_chord_test.go` or `app_test.go`; if none, write one that handles `tea.BatchMsg`), `ryOffered` like `riOffered`.

- [ ] **Step 2: Run** — FAIL to compile.

- [ ] **Step 3: `internal/web/routes/ytdlp.go`** — move the GET handler's computation (`:33-62`) into `YtdlpPluginStatus(port, httpsEnabled)` returning `YtdlpPluginInfo` (the handler becomes: `info, err := YtdlpPluginStatus(currentPort(), httpsEnabled)`; on error `jsonError(rw, err.Error(), 500)`; else `jsonResponse(rw, info)` — the struct's JSON tags reproduce the seven keys byte-for-byte). Define the struct above `YtdlpRoutes`.

- [ ] **Step 4: `internal/tui/ytdlp_dialog.go`** — the `client_tokens_dialog.go` shape (`Open` sets loading + spinner; `UpdateComponents` ticks while loading; `SetStatus(info)`/`SetError(msg)`; `View` three-way): 
```go
type YtdlpDialogModel struct {
	visible       bool
	width, height int
	loading       bool
	installing    bool
	info          routes.YtdlpPluginInfo
	errorMsg      string
	spinner       spinner.Model
}
func NewYtdlpDialogModel() *YtdlpDialogModel
func (m *YtdlpDialogModel) IsVisible() bool
func (m *YtdlpDialogModel) SetSize(w, h int)
func (m *YtdlpDialogModel) Open() tea.Cmd            // loading=true, errorMsg="", returns spinner.Tick
func (m *YtdlpDialogModel) Close()
func (m *YtdlpDialogModel) SetInstalling()           // installing=true (spinner text "Installing...")
func (m *YtdlpDialogModel) SetStatus(info routes.YtdlpPluginInfo) // loading=false, installing=false
func (m *YtdlpDialogModel) SetError(msg string)
func (m *YtdlpDialogModel) UpdateComponents(msg tea.Msg) tea.Cmd
func (m *YtdlpDialogModel) HandleKey(msg tea.KeyPressMsg) string // "close" | "install" | ""
func (m *YtdlpDialogModel) View() string
```
`View` rows when loaded:
```
yt-dlp Plugin
  Installed:      yes | not installed
  Plugin dir:     <PluginDir>
  Moombox port:   <CurrentPort> (https: on/off)
  Plugin points:  <InstalledPort>   ← shown only when Installed
  Port mismatch:  yes — press I to rewrite the plugin for the current port   ← only when PortMismatch
  Plugin file:    <ExtractedPath>   ← only when non-empty
  I: Install / reinstall   R: Refresh   Esc: Close
```
`HandleKey`: `esc`/`q` → Close + "close"; `i`/`I` when !loading && !installing → "install"; `r`/`R` → "refresh" (App re-runs the status cmd).

- [ ] **Step 5: App wiring** — the four coordinated additions (as Task 2 Step 4, with `ytdlpDlg`), plus:
- `app.go`: `OnYtdlpPluginStatus func() (routes.YtdlpPluginInfo, error)`, `OnInstallYtdlpPlugin func() error` (doc: `// OnInstallYtdlpPlugin (re)writes the yt-dlp plugin for the live port — the R Y overlay's I key. Distinct from the setup wizard's OnInstallYtdlp, which reports nothing back.`); msgs `ytdlpStatusMsg{Info routes.YtdlpPluginInfo; Err error}`, `ytdlpInstallResultMsg{Err error}`.
- `app_commands.go`: `ytdlpStatusCmd()` → `ytdlpStatusMsg`; `ytdlpInstallCmd()` → `ytdlpInstallResultMsg`.
- `app_keys.go` intercept: `"install"` → `a.ytdlpDlg.SetInstalling(); return a, tea.Batch(a.ytdlpInstallCmd(), a.ytdlpDlg.SpinnerInit())`; `"refresh"` → `tea.Batch(a.ytdlpStatusCmd(), a.ytdlpDlg.Open())`.
- `app_update.go`: `case ytdlpStatusMsg:` → `SetError`/`SetStatus` when visible; `case ytdlpInstallResultMsg:` → on error `SetError("Install failed: …")`, else `return a, a.ytdlpStatusCmd()` (reload) — and `routeComponentMsg` branch.
- `buildMenuItems()`: `if a.OnYtdlpPluginStatus != nil { items = append(items, ActionMenuItem{Chord: "R Y", Label: "yt-dlp Plugin", HintLabel: "yt-dlp", Category: "Request"}) }`. `dispatchAction` `case "R Y":` nil-guard feedback; `a.ytdlpDlg.SetSize(...)`; `return a, tea.Batch(a.ytdlpDlg.Open(), a.ytdlpStatusCmd())`.
- `cmd/moombox/tui_wiring.go`: 
  ```go
  	app.OnYtdlpPluginStatus = func() (routes.YtdlpPluginInfo, error) {
  		return routes.YtdlpPluginStatus(currentPort(), httpsEnabled)
  	}
  	app.OnInstallYtdlpPlugin = func() error { return routes.InstallYtdlpPlugin(currentPort(), httpsEnabled) }
  ```
  where `currentPort`/`httpsEnabled` are whatever `cmd/moombox/routes_wiring.go` passes to `routes.YtdlpRoutes(r, currentPort, httpsEnabled)` — reuse the same values (grep `YtdlpRoutes(`).

- [ ] **Step 6: Tests** — new tests PASS; `go test -count=1 ./internal/web/routes/ ./internal/tui/ ./cmd/moombox/` ok.

- [ ] **Step 7: Docs** — `CLAUDE.md:109` **R** chords: add `` `R Y` (yt-dlp Plugin — installed / plugin dir / port mismatch, `I` installs) ``. `README.md` Request table: `| R Y | yt-dlp plugin status / install |`. `docs/spec/user-interfaces.md` Request table: `| \`R Y\` | yt-dlp Plugin | Status callback is configured |`; yt-dlp rows (`:787-792`): the GET row gains ` Computation shared with the TUI's \`R Y\` overlay via \`YtdlpPluginStatus\`.`; Module/Overlay table: `| yt-dlp plugin | Settings → Integrations card (\`settings.js\` \`loadYtdlpPluginStatus\`) | \`YtdlpDialogModel\` (\`internal/tui/ytdlp_dialog.go\`) |`.

- [ ] **Step 8: Gates** — statics + `go test -count=1 ./internal/web/routes/ ./internal/tui/ ./internal/docs/ ./cmd/moombox/`. **Commit:**
```
feat(tui): R Y shows the yt-dlp plugin status and installs it

routes.YtdlpPluginStatus is the computation GET /api/ytdlp-plugin/status
had; the overlay renders it and I rewrites the plugin for the live port
through the same InstallYtdlpPlugin the web uses.
```

---

## Self-Review

**Spec coverage (§8):** Web 1 (rescan button, `checkMonitorsNow` shape, debounced toast with `retryAfterMs`) and 2 (`data-copy` Copy stream URL, `streamUrl` twin with four-case test) → Task 1. TUI 3 (`R I` path input with `~` expansion + existence check → `OnImportCookieFile` → `ImportCookies`, per-platform outcome rendered in the overlay, Esc closes, never logs content, test with fake callback) → Task 2. 4 (`A W`, `SupportsBatch`, Finished filter, `OnSetWatched(ids, watched)` → `BatchSetWatched` for batches [single via `UpdateJobFields` — ruling, mirrors the web], `•` glyph counted in `titleWidth`, appears/disappears + no wrap test) → Task 3. 5 (orphans `A` deletes the section after two presses, loops the per-item callbacks, collects failures) → Task 4. 6 (`S` skips via `OnDismissUpdate`; handler body → `routes.DismissUpdate(store, tag)` used by both) → Task 5 (spec's `shared` parameter is the package-level `SharedUpdateInfo`, so the signature is `(store, tag)` — ruling). 7 (`c` clears lines/filtered/search when `PanelLogs` focused; help lists it) → Task 6 (listed in `navigationKeys`, where the other log keys live — ruling). 8 (`R Y` async overlay from `routes.YtdlpPluginStatus`, `I` installs and refreshes) → Task 7 (`YtdlpPluginStatus(port, https)` not `(cfg)` — the route already computes from a live port getter, and the TUI closures do the same — ruling). Docs: CLAUDE.md chord line, README tables (+ the four missing rows backfilled), user-interfaces.md chord tables + Module/Overlay rows, SPEC.md has no parity table → per task.

**Placeholders:** every step has code or exact text; where a package name must be confirmed (style names, key constants, `keyMsg` helper, store fixture) the plan says which file to read and what to match — no "add appropriate handling".

**Type consistency:** `OnImportCookieFile func(string) (cookies.ImportResult, error)` in test, App field, cmd and wiring; `OnSetWatched func([]string, bool) error` throughout; `DismissUpdate(store *config.Store, tag string) error` in test, helper, route and wiring; `YtdlpPluginInfo` fields/tags identical in struct, test and view; dialog method names identical between each test and its model.
