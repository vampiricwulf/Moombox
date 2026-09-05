# Arc J — Residuals Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the carried residuals the improvement-chain spec names for Arc J (chat-timing CP1/T-F10/T-F12, the cookie-subsystem seams, the overlay focus trap) plus every ARC J INTAKE item the chain accumulated (test infrastructure, TUI polish, docs, gopls hints), each with its own small design and its own pin.

**Architecture:** Fourteen independent tasks on one branch, ordered mechanical → infrastructure → behaviour → UI → docs. Behaviour changes are deliberate, few, and each is its own commit with a test that fails before and passes after; everything else is behaviour-free and pinned by the existing suites. Chat-timing work obeys the player ledger's rulings (`.superpowers/sdd/2026-09-03-player-chat-sync-and-overlay/progress.md`: R1, R2, R7, R9–R13, R21–R23, R25, R27, R29, Task 4 "one epoch per file").

**Tech Stack:** Go 1.27 (`GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command), staticcheck 2026.2.1 (hard gate), goja, Node 24 `node --test` + jsdom (`web/tests`).

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §9 J. Design notes + rulings J1–J16 and the ruling amendments: `.superpowers/sdd/2026-09-05-improvement-j-residuals/design-notes.md`; located intake: `intake.md` there.

## Global Constraints

- One task = one dispatch = its own commit(s); never `go test ./...` inside a task (the controller runs the full gate at the merge candidate).
- Every task touching `web/public/*.js` runs `go test -count=1 ./internal/web/routes/` AND `node --test web/tests/*.test.mjs` (the routes tests lift app.js bodies into goja stubs and grep the shipped JS). Every task touching a Go symbol or a `docs/spec/*.md` file runs `go test -count=1 ./internal/docs/`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (`go list -deps ./internal/tui/ | grep -c 'internal/web/routes\|internal/bgutils'` → 0).
- staticcheck clean on every touched package; gofmt silent (`gofmt -l ./cmd ./internal ./tools ./web`).
- LF line endings on every touched file (`perl -0777 -ne 'print tr/\r//' <file>` → 0).
- Every commit ends with the two trailers `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp`.
- Git is read-only beyond `git add <files>` / `git commit` (no stash, checkout, reset, `add -A`).
- Chat timing: the player ledger's rulings are the design of record; a task that touches `internal/chat`, `internal/twitch/chat.go` or `web/public/modules/chat-timeline.js` reads them first and preserves them.

---

### Task 1: gopls hints sweep (behaviour-free)

**Files:** the sites below (verify each with the current gopls or by reading; a hint that is a false positive is recorded in the report, not "fixed").
- Modify: `internal/tui/settings_view.go` (:100, :813, :820 `writestring`; :659 `unusedparams` w), `internal/tui/import_dialog.go` (:215–:253 `writestring`, 10 sites), `internal/tui/add_video.go` (:616, :666, :714, :735 unused `h`/`w`), `internal/tui/trim_dialog.go` (:461, :545 unused `h`), `internal/cookies/autocookies_setup_reap_test.go` (:677 `restoreRealProbe` unused), `internal/cookies/refresh_transitions_test.go` (:226 `unusedwrite`), `internal/web/routes/cookies_shiftclick_test.go` (:43 QF1003 tagged switch), `internal/worker/part_merge_test.go` (:605, :632, :964, :982 `fmtappendf`).

**Rules:** `writestring`: `b.WriteString(fmt.Sprintf(...))` → `fmt.Fprintf(&b, ...)`; `x.WriteString(a + b)` → two `WriteString` calls only when it reads better, else leave. `unusedparams`: rename to `_` ONLY when the parameter is not part of an interface/callback signature that other code relies on (a `func(w, h int)` passed as a callback keeps its shape — rename to `_`, do not drop). `unusedfunc`: delete `restoreRealProbe` if nothing calls it (grep). `unusedwrite`: delete the dead assignment or turn it into the assertion it meant to be. QF1003: convert to `switch c { case …: }`. `fmtappendf`: `[]byte(fmt.Sprintf(...))` → `fmt.Appendf(nil, ...)`.

- [ ] **Step 1:** apply each site; after each package, `GOTMPDIR=… go vet ./<pkg>/ && staticcheck ./<pkg>/ && go test -count=1 ./<pkg>/`.
- [ ] **Step 2:** gofmt; LF check on every touched file.
- [ ] **Step 3:** commit `chore: gopls hint sweep — WriteString, unused params, tagged switch, fmt.Appendf` with a body listing each site and any hint left as a false positive + trailers.

---

### Task 2: `internal/webtest` — one goja harness for settings.js

**Files:**
- Create: `internal/webtest/settings_vm.go`, `internal/webtest/settings_vm_test.go`
- Modify: `internal/tui/settings_js_vm_test.go` (`settingsVM`), `internal/web/routes/cookies_lasterror_panel_test.go` (`settingsPanelVM`) — both become one-line wrappers.

**Interfaces:**
```go
package webtest // import "github.com/vampiricwulf/Moombox/internal/webtest"

// SettingsVM returns a goja runtime with utils.js and settings.js evaluated
// from the embedded web assets, in that order, with every `export ` and the
// `import {…} from "./utils.js"` line stripped — the ONE way to evaluate
// settings.js in Go tests (two harnesses used to carry the rule "utils.js
// first" in doc comments; a third would have re-broken it).
func SettingsVM(t testing.TB) *goja.Runtime
```
- [ ] **Step 1:** read both current harnesses fully; the shared function must reproduce EXACTLY the transformations both apply today (the tui one strips `export ` per module; the routes one also regex-strips the utils import — settings.js imports only `./utils.js`, and the rule "never add a second import to settings.js" stands). Where the two differ, keep the union that makes both existing test files pass unchanged.
- [ ] **Step 2:** write `settings_vm_test.go`: `SettingsVM(t)` evaluates without error and `vm.Get("snapshotRestartValues")` (a utils.js export the routes harness needed — verify the name in `web/public/modules/utils.js`) is a function.
- [ ] **Step 3:** replace both harness bodies with `return webtest.SettingsVM(t)`; delete the now-unused helpers in each test file (staticcheck U1000 is a hard gate).
- [ ] **Step 4:** gates: `go vet ./internal/webtest/ ./internal/tui/ ./internal/web/routes/`, `staticcheck` same, `go test -count=1 ./internal/webtest/ ./internal/tui/ ./internal/web/routes/`, `go list -deps ./internal/tui/ | grep -c 'internal/web/routes\|internal/bgutils'` → 0 (webtest imports the `web` assets embed, which the tui tests already import — confirm `go list -deps -test ./internal/tui/ | grep -c internal/bgutils` is also 0).
- [ ] **Step 5:** commit `test(webtest): one goja harness evaluates settings.js for tui and routes` + trailers.

---

### Task 3: the docs citation test names the declaring file

**Files:**
- Modify: `internal/docs/citations_test.go` (`fileFacts`, `parseGoFile`, check (b) at ≈:386–:410), `internal/docs/citation_allowlist.txt` only if a genuinely shared symbol needs it (with a reason line).
- Modify: whichever `docs/spec/*.md` lines the tightened check flags (re-point the file half).

**Design (ruling J10b):** `fileFacts` gains `declared map[string]bool` filled from `f.Decls` (top-level `func` names; method names as both `Type.Method` and `Method`; every `type`, `var`, `const` name incl. grouped specs). Add `declaredElsewhere(t, dir, file, sym) (string, bool)` that parses the sibling non-test `.go` files of `dir` once per test run (cache keyed by dir) and returns the first sibling that declares `sym`. Check (b) becomes:
```go
switch {
case ff.declares(sym):
	// the cited file is the declaring file
case ff.resolves(sym):
	if other, ok := declaredElsewhere(t, filepath.Dir(abs), abs, sym); ok {
		t.Errorf("%s:%d cites `%s` (`%s`), but that file only mentions it — it is declared in %s", doc, lineNo, prev.text, tok, other)
	}
	// a mention with no declaring sibling stays acceptable (fields, shared consts)
default:
	t.Errorf("%s:%d cites `%s` (`%s`), but that file neither declares nor mentions it (a comment does not count)", doc, lineNo, prev.text, tok)
}
```
- [ ] **Step 1:** table test `TestCitationDeclaringFile` with a temp two-file package (`a.go` declares `Foo`, `b.go` calls it): citing `b.go` for `Foo` fails with the new message; citing `a.go` passes; a symbol declared nowhere but mentioned in `b.go` passes.
- [ ] **Step 2:** implement; run `go test -count=1 ./internal/docs/`; for EVERY newly flagged pair, re-point the doc's file half to the declaring file the message names (file half only). Expect a handful (Arc I's sweep already fixed the cookies package; other packages may surface).
- [ ] **Step 3:** the floor at ≈:441 ("only %d symbol/path pairs were checked") stays; gofmt; LF.
- [ ] **Step 4:** commit `test(docs): a symbol citation must name the declaring file, not a caller` + body listing the re-pointed lines + trailers.

---

### Task 4: engine loop tests use `t.Context()`

**Files:** `internal/engine/*_test.go` — every `context.WithTimeout(context.Background(), …)` (≈60 sites; `grep -c 'context.Background()' internal/engine/*_test.go`) that drives a downloader loop becomes `context.WithTimeout(t.Context(), …)`; plain `context.Background()` passed to a downloader becomes `t.Context()`. Leave `context.Background()` where the code under test itself requires a non-test context (none expected).
- [ ] **Step 1:** sed-style replacement, then read each diff hunk once; helper functions that take `t *testing.T` pass `t.Context()`; helpers without `t` receive a `ctx` parameter from their caller (no new `context.Background()`).
- [ ] **Step 2:** `go vet ./internal/engine/ && staticcheck ./internal/engine/ && go test -count=1 ./internal/engine/` (≈6 s; if the wall time grows, a test now relies on the context surviving — find it and give it an explicit timeout instead).
- [ ] **Step 3:** commit `test(engine): loops are driven from t.Context() so a failed wait cancels them` + trailers.

---

### Task 5: one rollback helper and a jar-reload seam (J3 + J4)

**Files:**
- Create: `internal/cookies/cookie_rollback.go`
- Modify: `internal/cookies/cookie_import.go` (≈:470–:535), `internal/cookies/autocookies_refresh.go` (≈:536–:572), one new test file `internal/cookies/cookie_rollback_test.go`.

**Interfaces:**
```go
// loadCookieJar is the seam through which every restore path reloads the
// jar after writing the previous cookies back — a test can make it fail.
var loadCookieJar = func(s *AutoCookieService, path string) error { return s.jar.Load(path) }

// restorePreviousCookies writes `restored` over cookies.txt and reloads the
// jar. Both callers built the same two failure sentences; they live here once.
// The returned error already carries ErrImportRollbackIncomplete and names
// the platforms (labels) and which half failed.
func (s *AutoCookieService) restorePreviousCookies(restored string, platforms, labels []string, origin string) error
```
where `origin` is `"a rejected import"` or `"the browser profile did not verify"` — the ONLY text that differs between the two callers today (compare the two blocks line by line first; if any other wording differs, keep the caller-specific sentence by passing it in, never by dropping a truthful phrase). The refresh path today returns `fmt.Errorf("restore previous cookies: %w", restoreErr)` (not the sentinel) — keep the refresh caller's return values unchanged by having it wrap/unwrap as it does now; only the write + reload + log + setError block moves.
- [ ] **Step 1:** write `cookie_rollback_test.go` FIRST: (a) with `loadCookieJar` swapped for a failing func (restore via `t.Cleanup`), the import path returns an error that `errors.Is(err, ErrImportRollbackIncomplete)` and whose text contains "could not be reloaded"; (b) the refresh path's equivalent (drive `refreshCookiesDetailed` through the existing test scaffolding for a rejected profile — read `autocookies_refresh_*_test.go` for the fixture that reaches the rollback arm) sets `lastError` containing "reloading them failed" and returns `refreshAborted()`. Run: fail (no seam yet).
- [ ] **Step 2:** implement the helper + seam; both callers delegate; identical log messages and `setError` texts as today (diff the strings).
- [ ] **Step 3:** gates: gofmt; `go vet ./internal/cookies/ && staticcheck ./internal/cookies/`; `go test -count=1 ./internal/cookies/ ./internal/docs/`; `go doc -all ./internal/cookies` unchanged vs HEAD~ (unexported additions only).
- [ ] **Step 4:** commit `refactor(cookies): one restorePreviousCookies for import and refresh, behind a jar-reload seam` + trailers.

---

### Task 6: per-platform auth-verify windows (J5 — behaviour change)

**Files:** `internal/cookies/autocookies_profile.go` (`checkPlatformAuth` :641), `internal/cookies/autocookies.go` (:40–:58 budget comment), `docs/spec/data-and-storage.md` (the cross-writer-window sentence — grep `authVerifyTimeout`), a test in `internal/cookies/autocookies_profile_test.go` (or the file that already tests `checkPlatformAuth` — grep).

- [ ] **Step 1:** failing test: fake verifiers where the YouTube verifier blocks until its context is done and Twitch answers `true` immediately; today Twitch's result is `verifyUnknown` (its shared deadline is spent); after the change it is `verifyOK`. Assert on `tw.state == verifyOK` and that the call takes ≈ one window, not two (verifiers run sequentially today — if they do, the test bounds wall time at `< 2*authVerifyTimeout`; use a shortened timeout via a package var if `authVerifyTimeout` is a const — make it `var` ONLY inside the test through a build-tag-free seam: introduce `var authVerifyWindow = authVerifyTimeout` and use the var in `checkPlatformAuth`).
- [ ] **Step 2:** `checkPlatformAuth`: one `context.WithTimeout(ctx, authVerifyWindow)` per `check(...)` call (inside `check`, `defer cancel()`); delete the shared `vctx`.
- [ ] **Step 3:** comments and docs: `autocookies.go:40-58` — the budget line becomes "authVerifyTimeout — one window PER platform … = 30s" and the sum "≈ 107s against a 120s cap"; `data-and-storage.md`'s sentence pricing the import path against the window: re-derive (read it; the number it quotes changes from 15 s to 30 s for the two-platform worst case).
- [ ] **Step 4:** gates: gofmt/vet/staticcheck cookies; `go test -count=1 ./internal/cookies/ ./internal/docs/`.
- [ ] **Step 5:** commit `fix(cookies): each platform gets its own auth-verify window` with a body stating the budget arithmetic + trailers.

---

### Task 7: a replay run refuses a live run's sidecar; legacy offsets derived from the epoch (J1 — behaviour change)

**Files:**
- Modify: `internal/chat/types.go` (`ChatResumeState` :81), `internal/chat/downloader.go` (`saveResume` :1161, `Start` resume block :328–:358), `internal/chat/downloader_epoch_test.go` (or a new `downloader_mode_test.go`).
- Modify: `web/public/modules/chat-timeline.js` (new export), `web/public/modules/player.js` (`_fetchChatData` — the point where the fetched chat body becomes `playerChatMessages`), `web/tests/chat-timeline.test.mjs` (new cases).
- Docs: `docs/spec/data-and-storage.md` (the resume-sidecar field list, if it enumerates fields — grep `recentIds`).

**Go design:**
```go
// ChatResumeState …
	// Mode records which kind of run wrote the sidecar: "live" (the run was
	// live/upcoming) or "replay". A replay run must not adopt a live run's
	// sidecar — its count, continuation and dedup IDs describe the live half
	// of a mixed-mode file. Empty on sidecars written before this field.
	Mode string `json:"mode,omitempty"`
```
`saveResume`: `Mode: modeOf(cd.opts.IsLiveOrUpcoming)` with `func modeOf(live bool) string { if live { return "live" }; return "replay" }`. `Start`'s resume block: the condition `if err == nil && state != nil && state.VideoID == cd.opts.VideoID {` gains `&& !(state.Mode == "live" && !cd.opts.IsLiveOrUpcoming)`; before the `if`, when that refusal fires, `cd.logInfo("chat: ignoring the live run's resume sidecar for a replay run", "videoID", …)` (use the downloader's existing logging helper — read `logDebug`'s neighbours). A refused sidecar is NOT deleted here (the completion rule handles sidecars on exit; deleting on entry would race a concurrent live run — record this in the doc comment).
- [ ] **Step 1 (Go, failing tests):** (a) a sidecar with `Mode:"live"` loaded by a downloader with `IsLiveOrUpcoming:false` → `cd.messageCount == 0`, continuation is the opts' own, dedup empty (assert through the existing test seams in `downloader_epoch_test.go` — read how it builds a downloader and a sidecar); (b) `Mode:"replay"` + replay run → adopted as today; (c) empty `Mode` (legacy) + replay run → adopted as today; (d) `saveResume` writes `mode:"live"` for a live run and `"replay"` otherwise.
- [ ] **Step 2 (Go):** implement; `go test -count=1 ./internal/chat/`.
- [ ] **Step 3 (JS, failing tests):** `chat-timeline.js` gains
  ```js
  /** T-F12 remainder: messages written before hasOffset existed (or with
   *  hasOffset false) carry offsetMs 0 and pile at the start. When the file
   *  header has an epoch and the message a timestampUsec, the offset is
   *  derivable: offsetMs = timestampUsec/1000 − epochMs. Messages that already
   *  have an offset are untouched; without an epoch nothing changes (R10 keeps
   *  the overlay from flooding). Returns the same array, mutated in place. */
  export function deriveMissingOffsets(messages, streamStartTime) {
    const epochMs = Date.parse(streamStartTime || "");
    if (!Number.isFinite(epochMs)) return messages;
    for (const m of messages) {
      if (m.hasOffset) continue;
      const usec = typeof m.timestampUsec === "number" ? m.timestampUsec : Number(m.timestampUsec);
      if (!Number.isFinite(usec) || usec <= 0) continue;
      m.offsetMs = Math.round(usec / 1000) - epochMs;
      m.hasOffset = true;
    }
    return messages;
  }
  ```
  Tests in `web/tests/chat-timeline.test.mjs`: derives for `hasOffset` absent + usec present; leaves `hasOffset:true` messages alone; no epoch → untouched; negative results allowed (pre-show, N-F2). Confirm the JSON field names against `internal/chat/types.go` (`timestampUsec` — grep the `ChatMessage` struct tags) before writing the test.
- [ ] **Step 4 (JS):** call `deriveMissingOffsets(data.messages, data.streamStartTime)` in `player.js` right where the fetched chat body's messages are first stored (read `_fetchChatData` and `_computeChatParts`; for multi-part Twitch files apply per part before `mergePartChats`); gates: `node --test web/tests/*.test.mjs` (count grows by the new cases), `go test -count=1 ./internal/web/routes/`.
- [ ] **Step 5:** docs: if `data-and-storage.md` lists the sidecar's fields, add `mode`; one sentence in `user-interfaces.md`'s player section if it describes offsets ("messages without an offset are placed from the file's epoch when it has one").
- [ ] **Step 6:** two commits: `fix(chat): a replay run does not adopt a live run's resume sidecar` and `fix(player): legacy chat messages without offsets are placed from the file's epoch` + trailers.

---

### Task 8: a resumed Twitch part keeps its recording base (J2 — behaviour change, bounded)

**Files:** `internal/twitch/chat.go` (the resume/RollFile path that sets `recordingStartMs`; the offset base at :806–:816), one test in `internal/twitch/chat_*_test.go` (find the file that tests offsets/RollFile).

- [ ] **Step 1:** read how `recordingStartMs` is initialised when a downloader starts against an EXISTING part file (daemon restart mid-part): if it is set from `time.Now()` (or left 0 so `streamStartMs` applies) rather than from the part file's own header/first message, that is T-F10. Write the failing test: an existing part file whose header carries a recording start (or whose first message has a known offset base) → after resume, a message with `TimestampMs = base + 5000` gets `OffsetMs == 5000`, not `now − base` off.
- [ ] **Step 2:** on resume, read the part file's header (the writer's own format — `flush`/`RollFile` write it; read it back with the same struct) and seed `recordingStartMs` from it; log one Info line ("twitch chat: resuming part with its recorded base"). If the header carries no base, keep today's behaviour and write that finding in the report (the task then ends as DONE_WITH_CONCERNS with the test asserting today's behaviour instead — say so).
- [ ] **Step 3:** gates: gofmt/vet/staticcheck twitch; `go test -count=1 ./internal/twitch/ ./internal/docs/`.
- [ ] **Step 4:** commit `fix(twitch): a resumed chat part keeps its recording base instead of the restart time` + trailers.

---

### Task 9: a Web-side update dismiss clears the TUI badge (J8 — behaviour change)

**Files:** `internal/web/routes/update.go` (`UpdateRouteDeps`, the `/api/update/dismiss` handler ≈:236–:250), `cmd/moombox/routes_wiring.go` (≈:162–:176), `internal/tui/app_update.go` (`case UpdateStatusMsg:` :296), tests: `internal/web/routes/update_test.go` (or the file testing the dismiss route), `internal/tui/app_update_test.go` (or the update-badge test file — grep `UpdateStatusMsg` in tui tests).

```go
// UpdateRouteDeps …
	// OnDismissed runs after a dismiss is persisted, with the tag that was
	// skipped — the TUI clears its badge from here (the Web already hides
	// its own). Optional.
	OnDismissed func(tag string)
```
Handler: after `DismissUpdate` succeeds, `if deps.OnDismissed != nil { deps.OnDismissed(pending.TagName) }`. Wiring: `OnDismissed: func(string) { select { case s.tuiUpdateStatusCh <- tui.UpdateStatusMsg{}: default: } }`. TUI:
```go
	case UpdateStatusMsg:
		if msg.Version == "" {
			// A dismiss elsewhere (the Web) cleared the pending update.
			a.updateAvailable = nil
			a.details.updateInfo = nil
			return a, a.listenForUpdates()
		}
		a.updateAvailable = &msg
		…
```
- [ ] **Step 1:** failing tests: routes — POST `/api/update/dismiss` with a pending release invokes `OnDismissed("v9.9.9")` once (httptest, existing pattern); tui — after an `UpdateStatusMsg{Version:"9.9.9"}` set the badge, an empty `UpdateStatusMsg{}` clears `updateAvailable` and `details.updateInfo`, and the status bar no longer renders the update glyph (grep how the badge renders).
- [ ] **Step 2:** implement; gates: gofmt/vet/staticcheck on routes, tui, cmd/moombox; `go test -count=1 ./internal/web/routes/ ./internal/tui/ ./cmd/moombox/`; deps 0.
- [ ] **Step 3:** docs: `docs/spec/user-interfaces.md`'s "Async Message Types" row for `UpdateStatusMsg` gains "an empty message clears the badge (Web-side dismiss)".
- [ ] **Step 4:** commit `fix(tui,web): dismissing an update in the dashboard clears the TUI badge` + trailers.

---

### Task 10: the job-status action sets live in utils.js (J9)

**Files:** `web/public/modules/utils.js`, `web/public/modules/job-details.js`, `web/public/app.js`.
- [ ] Move `CANCEL_STATUSES`, `RESUME_STATUSES`, `MUX_STATUSES`, `REINIT_STATUSES` (read `job-details.js`'s export block for the exact four names and values) verbatim into `utils.js` as exports beside `canResumeJob` (which they conceptually belong with — check whether `canResumeJob` already duplicates one of them; if so, have it use the set). `job-details.js` and `app.js` import them from `./modules/utils.js` / `./utils.js`; `job-details.js` stops exporting them; `app.js` imports only the three it uses.
- [ ] Gates: `node --test web/tests/*.test.mjs` (unchanged count, 0 fail), `go test -count=1 ./internal/web/routes/`; grep confirms no other importer.
- [ ] Commit `refactor(web): the job-status action sets live in utils.js` + trailers.

---

### Task 11: the resume dialog traps focus (J6 — behaviour change, a11y)

**Files:** `web/public/modules/player.js` (`_showResumeDialog` ≈:1805–:1870), `web/tests/player.test.mjs`.
- [ ] **Step 1:** failing jsdom test: open a job with a saved position so the resume dialog shows (the harness has a fixture for it — grep `resume-continue` in `player.test.mjs`); focus `#resume-start`, dispatch `keydown Tab` → `document.activeElement.id === "resume-continue"`; focus `#resume-continue`, dispatch `keydown` Tab with `shiftKey: true` → `resume-start`.
- [ ] **Step 2:** in `_showResumeDialog`, after the Escape handler, add on the SAME signal:
  ```js
    // Focus trap (U-M8): Tab and Shift+Tab cycle within the dialog's two
    // actions while it is open; focus is restored on dismiss (already wired).
    const focusables = () => [...overlay.querySelectorAll("sl-button, button, [tabindex]:not([tabindex='-1'])")]
      .filter((el) => !el.disabled);
    overlay.addEventListener("keydown", (e) => {
      if (e.key !== "Tab") return;
      const items = focusables();
      if (!items.length) return;
      const first = items[0], last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }, { signal: sig });
  ```
  If `sl-button` does not receive focus in jsdom (custom element without a shadow root), the test targets the elements the trap actually moves between — say so in the report; do not add `tabindex` attributes to production markup for the test's sake.
- [ ] **Step 3:** gates: `node --test web/tests/*.test.mjs` (+2), `go test -count=1 ./internal/web/routes/`; LF.
- [ ] **Step 4:** commit `fix(player): the resume dialog traps Tab within its actions` + trailers.

---

### Task 12: player and route pins from the can-wait list (J15 — test-only + one guard)

**Files:** `web/tests/player.test.mjs`, `internal/web/routes/*_test.go` (the two 304 tests — grep `notModifiedSince`), `web/public/modules/player.js` (one guard).
Pins (each ≤ 30 lines; the player review §7 numbering):
1. **#1** the two 304 tests also assert `Last-Modified` is present on the 304 response (routes).
2. **#2** R9 mid-flight: after `h.seek(10000)` with messages already flying, at least one animation in `h.anims` has `currentTime > 0`.
3. **#4** a segmented job with no `durationSeconds` and `video.duration = 60` → `_videoDurationMs() === 0` and no "after end" divider.
4. **#5** `tick(60000)` then `seek(60000)` keeps the `.post` region.
5. **#23** production guard + pin: in `_playerKeyHandler` (grep), Space does nothing while `.resume-overlay` is present (`if (document.querySelector(".resume-overlay")) return;` at the top of the Space arm), and a test presses Space with the dialog open → the video is not played.
- [ ] Gates: node (+5), routes tests; LF. Commit `test(player,web): pins from the player review's can-wait list; Space is inert under the resume dialog` + trailers.

---

### Task 13: TUI polish (J11)

**Files:** `internal/tui/files_dialog.go` (:261, :370, :620, :267), `internal/tui/status_bar.go` (the chord hint at :260), `internal/tui/task_list.go` (:35 `FilterErrors`), `internal/tui/app_layout.go` (`View` :92), `internal/tui/help.go` (:160–:200), tests beside each.
- [ ] **(a)** `func filesBoxDims(w, h int) (boxW, boxH, listH int)` returning `max(min(80, w-4), 40)`, `max(min(24, h-4), 10)`, `max(boxH-7, 1)`; the three sites call it (`:370`'s `contentW` derives from `boxW-4`). Test: `filesBoxDims(200, 50) == (80, 24, 17)`, `filesBoxDims(50, 12) == (46, 10, 3)`.
- [ ] **(b)** the chord hint line: bound to the status bar's width with `truncateString(hint, width)` (read :255–:270; the hint must never wrap the bar to two lines). Test: at width 60 the rendered hint line has `lipgloss.Width <= 60`.
- [ ] **(c)** `FilterErrors` → `FilterIssues` (all references; grep docs — none cite it; the citation test confirms).
- [ ] **(d)** minimum terminal size: in `App.View` after the `Initializing...` check: `if a.width < minTermWidth || a.height < minTermHeight { return a.viewWithMode(fmt.Sprintf("Terminal too small: %d×%d (Moombox needs at least %d×%d)", a.width, a.height, minTermWidth, minTermHeight)) }` with `const minTermWidth, minTermHeight = 60, 20`. Test: `SetSize(50, 10)` → the view contains "Terminal too small". Document in `docs/spec/user-interfaces.md`'s TUI section and README's TUI notes (one sentence each).
- [ ] **(e)** help rows wrap instead of being cut: in `help.go`'s row builder, wrap `k.desc` at `w - 2 - 14` columns (use `lipgloss.NewStyle().Width(...)`'s wrapping or `ansi.Wrap`) so continuation lines are indented under the description column. Test: at width 80 every row of the filter-language line survives (`strings.Contains(view, "\"quoted phrase\"")`); `TestHelpCoversEveryChord` stays green.
- [ ] Gates: gofmt/vet/staticcheck tui; `go test -count=1 ./internal/tui/ ./internal/docs/`; deps 0. Commit `fix(tui): files dialog sizing helper, bounded chord hint, FilterIssues, a minimum-size screen, help rows that wrap` + trailers.

---

### Task 14: docs (J12 a–c)

**Files:** `docs/spec/user-interfaces.md` (:308 "Async Message Types" table), `README.md` (:419), `docs/spec/architecture.md` (:183 column alignment).
- [ ] **(a)** `grep -n 'Msg struct\|Msg =\|Msg{' internal/tui/*.go | grep -v _test` → the full list of `tea.Msg` types; diff against the table; add one row per missing type (source, content) — expected at least `statsSnapshotMsg`, `statsRefreshTickMsg`, `bulkOrphanResultMsg`, the cookie-import result msg, the yt-dlp dialog msgs, `updateCheckResultMsg`; read each type's doc comment for the wording.
- [ ] **(b)** README.md:419 "Browse orphaned files" vs the spec's "Browse Orphaned Items": the TUI's actual menu label wins (read `buildMenuItems()` in `internal/tui/app_actions.go`); make README and user-interfaces.md say the same.
- [ ] **(c)** architecture.md:183: fix the table's column alignment (pipes) for the rows Arcs F–I added.
- [ ] Gates: `go test -count=1 ./internal/docs/`; LF. Commit `docs: every TUI message type in the table; one orphaned-items label; a straight package table` + trailers.
