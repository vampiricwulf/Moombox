# Arc C — Dead Code, Dead Config, Logout, Wording Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `staticcheck ./...` a hard CI gate by removing every dead symbol it flags, retire the unread `pot_provider_url` setting, document three uncalled GET routes, add a Web logout icon, and align the two UIs' status wording (Issues / Auth Required / Refresh Cookies from Browser) — plus the tooling intake Arcs A, B and D handed this arc.

**Architecture:** Seven independent tasks on one branch. Task 1 is pure deletion across seven packages and must land first (Task 7 flips CI to a hard gate on the strength of it). Tasks 2–6 are small, self-contained edits with their own tests: config/README docs, a 40-line `logout.js` module wired into the status bar, a `StatusLabel` helper beside `StatusColor`, a three-line bucket rename in `filter-engine.js`, and a stamp file that stops `tools/fetch-node` trusting the tracked `version.txt` for gitignored blobs.

**Tech Stack:** Go 1.27.1 (`toolchain go1.27.1`), staticcheck 2026.2.1, vanilla ES modules + `node:test`, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-09-04-improvement-chain-design.md` §6 (Arc C) and §1 rulings Q4 (Cancelled → Issues bucket on both UIs), Q5 (TUI shows "Auth Required" for `COOKIES?`), Q6 (logout = status-bar icon beside the theme toggle, visible only when auth is on AND authenticated), Q7 (keep uncalled routes; document the three GETs in README), Q8 (remove `pot_provider_url`). Intake rows: `.superpowers/sdd/2026-09-04-improvement-chain/progress.md` lines 13–34 (tls SA1019, gopls unusedfunc items, fetch-node idempotence, version.txt eol, smoke-test comment, staticcheck pin + hard gate).

## Global Constraints

- Worktree `D:/Git/Moombox/.worktrees/improvement-c-cleanup`, branch `improvement-c-cleanup`, cut from `main`. Never push. Never `cd` to the main checkout.
- Every `go` command carries `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp`. Implementers run ONLY the packages their task names — the controller runs the single full `go test -count=1 ./...`; one full run at a time on this machine.
- `gofmt -l ./cmd ./internal ./tools ./web` prints nothing; `go vet ./...` silent; from Task 1 onward `staticcheck ./...` (2026.2.1, installed at `C:/Users/Wulf/go/bin/staticcheck`) prints nothing.
- No new Go modules, no new npm dependencies. `web/tests/player.test.mjs` stays the ONLY suite that imports jsdom (`web/tests/README.md`); new node tests are pure.
- Labels are verbatim and shared by both UIs: `Auth Required` (the Web already uses it at `web/public/app.js:4164`), `Issues`, `Refresh Cookies from Browser`, `no jobs to resume`, `no jobs to reinitialize`.
- Docs cited by `internal/docs/citations_test.go` (the six `docs/spec/*.md` deep-dives) must keep every backticked `path`/`Symbol` citation resolvable — cite only files and symbols that exist after your change; never cite the gitignored stamp file Task 6 adds.
- Every commit ends with both trailers, exactly:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Jw2Ne9cGsn3R9bJBNuXudp
  ```
  Write the message to a file and commit with `git commit -F <file>` (never `-F -`).
- Line numbers below were read at `main` = `1a38737`. Re-verify with `grep -n` before editing; the text quoted beside each is the anchor.

---

### Task 1: staticcheck-clean sweep (dead symbols, deprecated field, unused test helpers)

**Files:**
- Modify: `internal/youtube/player_api.go:60-64,108-183` (`loggedSigRoutes`, `ClearLoggedRoutes`, `decryptSig`, `decryptSigLegacy`), `:96-97` (SetCipher doc comment)
- Modify: `internal/worker/strategies.go:247-250` (ClearLoggedRoutes call), `:548-561` (`challengeLabel`)
- Modify: `internal/worker/strategy_youtube_dash.go:107-108` (dormant-minter comment)
- Modify: `internal/cipher/decrypt.go:51`
- Modify: `internal/notifications/manager.go:18-27`
- Modify: `internal/youtube/browse.go:254`
- Modify: `internal/cipher/player_cache_test.go:17-19,371-378`
- Modify: `internal/twitch/chat_reauth_test.go:113-122,124`
- Modify: `internal/twitch/api_gql_log_hygiene_test.go:30`
- Modify: `internal/web/tls.go:165-167`
- Modify: `cmd/moombox/launcher_unix.go:36-39` (+ its `os/exec` import)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `staticcheck ./...` exit 0 — Task 7 flips CI on it. Removes exported `(*PlayerAPI).ClearLoggedRoutes` (its only caller is the line this task deletes).

The ten findings staticcheck 2026.2.1 reports on `main`:

```
internal\cipher\player_cache_test.go:17:6: func testPlayerURL is unused (U1000)
internal\cipher\player_cache_test.go:374:2: this value of urls is never used (SA4006)
internal\notifications\manager.go:20:5: var discordWebhookPath is unused (U1000)
internal\notifications\manager.go:25:6: func redactDiscordWebhookURL is unused (U1000)
internal\twitch\chat_reauth_test.go:113:28: func (*holdingIRCServer).nextSession is unused (U1000)
internal\web\tls.go:167:3: (crypto/tls.Config).PreferServerCipherSuites has been deprecated since Go 1.18: PreferServerCipherSuites is ignored.  (SA1019)
internal\worker\strategies.go:556:6: func challengeLabel is unused (U1000)
internal\youtube\browse.go:254:35: should convert v (type MembershipVideo) to TabItem instead of using struct literal (S1016)
internal\youtube\player_api.go:127:21: func (*PlayerAPI).decryptSig is unused (U1000)
internal\youtube\player_api.go:171:21: func (*PlayerAPI).decryptSigLegacy is unused (U1000)
```

plus two gopls `unusedfunc` items staticcheck cannot see across build tags: `cmd/moombox/launcher_unix.go:39 setSysProcAttr` (its only caller, `launcher_windows.go:212`, is in the Windows file — the Linux stub is dead) and the `strings.Builder` concatenation at `internal/twitch/api_gql_log_hygiene_test.go:30`.

- [ ] **Step 1: Record the baseline**

```bash
cd D:/Git/Moombox/.worktrees/improvement-c-cleanup
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./... | tee D:/Git/Moombox/.superpowers/sdd/2026-09-05-improvement-c-cleanup/staticcheck-before.txt | wc -l
```
Expected: `10`.

- [ ] **Step 2: `internal/youtube/player_api.go` — remove the sig-route logging path**

Delete, in this order (grep each anchor first):
1. The field and its comment (`:60-64`):
   ```go
   	// loggedSigRoutes tracks which (playerID, route) tuples we've already
   	// logged "sig decrypted" for. Prevents 20-50 lines per video probe;
   	// operators only need to see the route the FIRST time it succeeds for
   	// a given player.
   	loggedSigRoutes sync.Map // map[string]struct{} keyed by "<playerID>|<route>"
   ```
2. `ClearLoggedRoutes` with its whole doc comment (`:108-121`, from `// ClearLoggedRoutes drops the per-route log dedup state` through the closing `}`).
3. `decryptSig` with its doc comment (`:123-169`, from `// decryptSig solves sig via the routed cipher.Solver` through the `return "", err` + `}`).
4. `decryptSigLegacy` (`:171-183`).
5. In `SetCipher`'s doc comment (`:96-97`) change `GetSolvers/DecryptSig/DecryptN path on cipherSolver.` to `GetSolvers/DecryptN path on cipherSolver.`

Keep `cipherSolver` (GetSts and `decryptN`'s legacy path use it) and `decryptN`. `go build` will report whichever of `sync`, `slog`, `cipher` imports became unused — remove only those it names.

Why the field and the exported method go too: with `decryptSig` gone nothing ever writes `loggedSigRoutes`, so `ClearLoggedRoutes` deletes keys that can no longer exist. A write-only map is the same dead code with a different shape.

- [ ] **Step 3: `internal/worker/strategies.go` — the caller and `challengeLabel`**

At `:247-250` delete the comment and the call, leaving `InvalidateVisitorData`:
```go
	if job.YT != nil {
		job.YT.InvalidateVisitorData()
	}
```
Delete `challengeLabel` and its doc comment (`:548-561`, from `// challengeLabel compresses a challenge value` through `}`).

- [ ] **Step 4: `internal/worker/strategy_youtube_dash.go:107-108` — fold the dormant-minter note**

Replace
```go
	// bgutil-ytdlp-pot-provider uses; the challenge-sourced fresh-per-mint
	// minters exceed upstream and stay dormant (see GenerateGvsPoToken).
```
with
```go
	// bgutil-ytdlp-pot-provider uses; the challenge-sourced fresh-per-mint
	// minters exceed upstream and stay dormant (see GenerateGvsPoToken). If
	// premieres 403 despite the yt-dlp-parity bindings, those minters are the
	// next variable to trial, and the provenance log line should then also
	// say whether a watch-page ytAtN challenge was present ("page"/"none").
```

- [ ] **Step 5: `internal/cipher/decrypt.go:51`**

`// This is the worker-side equivalent of PlayerAPI.decryptSig / decryptN:` → `// This is the worker-side equivalent of PlayerAPI.decryptN:`

- [ ] **Step 6: `internal/notifications/manager.go:18-27`**

Delete `discordWebhookPath` (var + comment) and `redactDiscordWebhookURL` (func + comment). `discordWebhookRe` on `:16` stays, so `regexp` stays imported.

- [ ] **Step 7: `internal/youtube/browse.go:254`**

```go
		page.Items = append(page.Items, TabItem(v))
```
(`TabItem` at `browse.go:57` and `MembershipVideo` at `channel_membership.go:38` have identical field sets — that is what S1016 asserts.)

- [ ] **Step 8: `internal/cipher/player_cache_test.go`**

Delete `testPlayerURL` (`:17-19`). In `TestFetch_Singleflight_CoalescesByCacheKey` delete the first `urls` block — the comment `// Build three locale variants of the same URL — singleflight key is`, the `baseURL := strings.TrimSuffix(...)` line and the `urls := []string{ ... }` literal (`:371-378`) — and change the later `urls = []string{` (`:398`, under `// Rebuild URLs against the slow server's base.`) to `urls := []string{`. Reword that comment to `// Three locale variants of the same URL — the singleflight key is` / `// CacheKey(playerURL), which strips the locale.` If `strings` is now unused in the file, `go vet` says so; remove the import.

- [ ] **Step 9: `internal/twitch/chat_reauth_test.go`**

Delete `(*holdingIRCServer).nextSession` (`:113-122`). Replace the first paragraph of the next comment (`:124-125`)
```go
// nextSessionWhileRunning is nextSession for the tests that drive a real Start,
// and it watches Start as well as the fixture.
```
with
```go
// nextSessionWhileRunning is the session wait for the tests that drive a real
// Start (the others go through ircReplier.nextSession in
// chat_irc_fallback_test.go), and it watches Start as well as the fixture.
```
and in the paragraph after it change `reports the generic ten-second timeout above` to `reports the fixture's generic ten-second timeout`.

- [ ] **Step 10: `internal/twitch/api_gql_log_hygiene_test.go:30`**

```go
	b.WriteString(level)
	b.WriteString(" ")
	b.WriteString(msg)
```

- [ ] **Step 11: `internal/web/tls.go:165-167`**

Delete the two comment lines and the field:
```go
		// Server picks the cipher to enforce the curated order rather
		// than letting the client downgrade us to a weaker entry.
		PreferServerCipherSuites: true,
```
(Go has ignored the field since 1.17 and always applies the server's `CipherSuites` order for TLS 1.2, so nothing changes at runtime.)

- [ ] **Step 12: `cmd/moombox/launcher_unix.go:36-39`**

Delete the stub and its comment:
```go
// setSysProcAttr is a no-op on Linux. There's no equivalent of
// CreationFlags=createNoWindow because Linux processes don't open
// console windows the same way Windows ones do.
func setSysProcAttr(cmd *exec.Cmd) {}
```
and drop `"os/exec"` from the import block (it was the only use). `launcher_windows.go` keeps its `setSysProcAttr`; its only caller is in the same file.

- [ ] **Step 13: Gates**

```bash
cd D:/Git/Moombox/.worktrees/improvement-c-cleanup
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./cmd ./internal ./tools ./web
go build ./... && go vet ./... && staticcheck ./... && echo STATICCHECK-CLEAN
GOOS=linux GOARCH=amd64 go build ./... && GOOS=linux GOARCH=amd64 go vet ./cmd/moombox/ && echo LINUX-OK
go test -count=1 ./internal/youtube/ ./internal/worker/ ./internal/cipher/ ./internal/notifications/ ./internal/twitch/ ./internal/web/ ./cmd/moombox/
```
Expected: gofmt prints nothing; `STATICCHECK-CLEAN`; `LINUX-OK`; seven `ok` lines (worker ~25 s, twitch ~20 s). Paste the literal output into your report.

- [ ] **Step 14: Commit**

```
chore: remove the dead sig-route logging path and the other staticcheck findings

decryptSig/decryptSigLegacy had no callers since URL resolution moved to
cipher.RoutedResolveURL; without them loggedSigRoutes was write-only, so
ClearLoggedRoutes and its one caller go too. challengeLabel's dormant-minter
note moves into the mint-path comment it pointed at. PreferServerCipherSuites
has been a no-op since Go 1.17; the Linux setSysProcAttr stub had no caller;
two test helpers were unused. staticcheck ./... is clean from this commit.
```

---

### Task 2: Retire `pot_provider_url`; document the three uncalled GET routes

**Files:**
- Modify: `internal/config/types.go:185`
- Modify: `internal/web/routes/config_routes.go:602-604`
- Modify: `config.example.toml:150-151`
- Modify: `docs/spec/data-and-storage.md:520`
- Modify: `REWRITE_GUIDE.md:520`
- Modify: `README.md:484-507` (API table)
- Test: `internal/config/config_test.go` (new test)

**Interfaces:** none.

- [ ] **Step 1: Write the failing test** — append to `internal/config/config_test.go`:

```go
// TestLoadIgnoresRetiredPotProviderURL: pot_provider_url was read by nothing
// and was removed from DownloaderConfig (2026-09-04 improvement chain, Q8).
// A config.toml written by an older build still carries the key; toml.Decode
// ignores keys the struct does not declare, so the file must load, and the
// next Save drops the key.
func TestLoadIgnoresRetiredPotProviderURL(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	content := `
[network]
port = 8080

[downloader]
pot_provider_url = "http://127.0.0.1:4416"
max_video_resolution = 720
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load with the retired key: %v", err)
	}
	if cfg.Downloader.MaxVideoResolution != 720 {
		t.Errorf("MaxVideoResolution = %d, want 720 (the section around the retired key must still decode)", cfg.Downloader.MaxVideoResolution)
	}
}
```
(`Load`, `os`, `filepath` are already used by `TestLoadFromFile` at `:215`; match that test's imports and the exact field name `MaxVideoResolution` — grep `max_video_resolution` in `types.go` if it differs.)

- [ ] **Step 2: Run it** — `go test -count=1 -run TestLoadIgnoresRetiredPotProviderURL ./internal/config/` → PASS already (the key is currently declared). That is expected: this test pins the post-removal behaviour; it must still pass after Step 3.

- [ ] **Step 3: Remove the field and its writers**

- `internal/config/types.go:185`: delete `PotProviderURL string \`toml:"pot_provider_url,omitempty" json:"pot_provider_url,omitempty"\``.
- `internal/web/routes/config_routes.go:602-604`: delete
  ```go
  		if v, ok := dl["pot_provider_url"].(string); ok {
  			cfg.Downloader.PotProviderURL = v
  		}
  ```
- `config.example.toml:150-151`: delete `# Advanced: external POT provider URL (instead of built-in BotGuard)` and `# pot_provider_url = ""` and the blank line before them (one blank line must remain before `# ─── Cookies`).
- `docs/spec/data-and-storage.md:520`: delete the row `| PotProviderURL | string | "" | \`pot_provider_url\` | External PO token provider |`.
- `REWRITE_GUIDE.md:520`: delete `    PotProviderURL         string \`toml:"pot_provider_url,omitempty"\``.

`grep -rn -i 'pot_provider_url\|PotProviderURL' --include='*.go' --include='*.js' --include='*.html' --include='*.toml' --include='*.md' cmd internal web config.example.toml README.md REWRITE_GUIDE.md SPEC.md docs/spec` must then print nothing (the spec doc under `docs/superpowers/` may still name it — that is history, leave it).

- [ ] **Step 4: README API rows** — in the table at `README.md:483-507`, after the `GET /api/jobs/{id}/chat` row (`:488`) insert:

```
| GET | `/api/jobs/{id}/segments` | List segments for a multi-segment recording |
| GET | `/api/jobs/{id}/trims` | List trim clips created from this job |
```
and after the `GET /api/stats` row (`:507`, the table's last row) append:
```
| GET | `/api/logs` | Get recent log lines (from the in-memory ring buffer) |
```
Wording is copied from `docs/spec/user-interfaces.md:499,503,804`.

- [ ] **Step 5: Gates**

```bash
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./cmd ./internal && go build ./... && go vet ./... && staticcheck ./...
go test -count=1 ./internal/config/ ./internal/web/routes/ ./internal/docs/
```
Expected: silent statics; three `ok` (routes ~18 s).

- [ ] **Step 6: Commit**

```
chore(config,docs): retire pot_provider_url; document the three uncalled GET routes

Nothing read Downloader.PotProviderURL. Old config files still load (unknown
TOML keys are ignored; the next save drops it) — pinned by
TestLoadIgnoresRetiredPotProviderURL. README's API table gains GET /api/logs,
/api/jobs/{id}/segments and /api/jobs/{id}/trims (Q7: keep and document).
```

---

### Task 3: Web logout icon in the status bar

**Files:**
- Create: `web/public/modules/logout.js`
- Modify: `web/public/index.html:2112` (before `theme-toggle`)
- Modify: `web/public/app.js` — imports (top of file), the header-button wiring block at `:447-456`, `checkSecurityBanner()` at `:1535-1550`
- Test: `web/tests/logout.test.mjs` (new, pure — no jsdom)
- Modify: `docs/spec/user-interfaces.md:53` (module table row), `:478` (logout route row); `SPEC.md:616`

**Interfaces:**
- Consumes: `GET /api/auth/status` JSON `{ authRequired, authenticated, hasPassword, passwordlessExternal }` (`internal/web/routes/auth.go:54-57`); `POST /api/auth/logout` (`auth.go:158`, clears the session cookie — covered by `TestAuthLogoutInvalidatesSession`).
- Produces: `logoutVisible(status) → boolean`, `applyLogoutVisibility(button, status)`, `bindLogout(button, { fetchFn, reload })`.

The Web UI only ever loads when auth is off or the session is valid (otherwise the server serves the login page), so `authRequired && authenticated` is exactly "there is a session to end". The 401 fetch wrapper at `app.js:4970-4994` exempts `/api/auth/*`, so the click's own reload is the only one. Other header POSTs are bare `fetch(url, { method: "POST" })` (`app.js:781,817`); CSRF is satisfied by the browser's Origin header (`internal/web/middleware.go:107`), so the logout POST needs nothing more.

- [ ] **Step 1: Write the failing tests** — `web/tests/logout.test.mjs`:

```js
// Tests for web/public/modules/logout.js — pure (no DOM library): the button
// is a duck-typed element carrying exactly what the module touches.

import { test } from "node:test";
import assert from "node:assert/strict";

import { logoutVisible, applyLogoutVisibility, bindLogout } from "../public/modules/logout.js";

function fakeButton() {
  return {
    style: { display: "none" },
    disabled: false,
    handlers: {},
    addEventListener(type, fn) { this.handlers[type] = fn; },
    click() { return this.handlers.click?.(); },
  };
}

test("logoutVisible: only authRequired AND authenticated shows the button", () => {
  assert.equal(logoutVisible({ authRequired: true, authenticated: true }), true);
  assert.equal(logoutVisible({ authRequired: true, authenticated: false }), false);
  assert.equal(logoutVisible({ authRequired: false, authenticated: true }), false);
  assert.equal(logoutVisible({ authRequired: false, authenticated: false }), false);
  assert.equal(logoutVisible(null), false);
  assert.equal(logoutVisible(undefined), false);
});

test("applyLogoutVisibility toggles display like the other header icons", () => {
  const btn = fakeButton();
  applyLogoutVisibility(btn, { authRequired: true, authenticated: true });
  assert.equal(btn.style.display, "");
  applyLogoutVisibility(btn, { authRequired: false, authenticated: false });
  assert.equal(btn.style.display, "none");
  assert.doesNotThrow(() => applyLogoutVisibility(null, { authRequired: true, authenticated: true }));
});

test("bindLogout: click POSTs /api/auth/logout, then reloads", async () => {
  const btn = fakeButton();
  const calls = [];
  let reloaded = 0;
  bindLogout(btn, {
    fetchFn: async (url, opts) => { calls.push([url, opts]); return { ok: true, status: 200 }; },
    reload: () => { reloaded++; },
  });
  await btn.click();
  assert.deepEqual(calls, [["/api/auth/logout", { method: "POST" }]]);
  assert.equal(reloaded, 1);
  assert.equal(btn.disabled, true, "the button is disabled while the request is in flight so a double-click cannot POST twice");
});

test("bindLogout: a failed POST still reloads (the login page is the honest state)", async () => {
  const btn = fakeButton();
  let reloaded = 0;
  bindLogout(btn, {
    fetchFn: async () => { throw new TypeError("network down"); },
    reload: () => { reloaded++; },
  });
  await btn.click();
  assert.equal(reloaded, 1);
});

test("bindLogout tolerates a missing button", () => {
  assert.doesNotThrow(() => bindLogout(null, { fetchFn: async () => ({}), reload: () => {} }));
});
```

- [ ] **Step 2: Run it** — `node --test web/tests/logout.test.mjs` → FAIL: `Cannot find module '.../public/modules/logout.js'`.

- [ ] **Step 3: Create `web/public/modules/logout.js`**

```js
/**
 * Status-bar logout icon (2026-09-04 improvement chain, ruling Q6).
 *
 * The icon exists only when there is a session to end: the server requires
 * auth AND this browser is authenticated. The dashboard is never served to an
 * unauthenticated visitor when auth is on (the login page is), so the pair
 * is the whole rule. Kept out of app.js so the rule and the click are unit
 * tests, not a manual check.
 */

/**
 * @param {{authRequired?: boolean, authenticated?: boolean}|null|undefined} status
 *   the GET /api/auth/status payload
 * @returns {boolean}
 */
export function logoutVisible(status) {
  return !!(status && status.authRequired && status.authenticated);
}

/**
 * Show or hide the button for a status payload — the same style.display
 * toggling the other header icons use (btn-refresh-cookies, version-indicator).
 */
export function applyLogoutVisibility(button, status) {
  if (!button) return;
  button.style.display = logoutVisible(status) ? "" : "none";
}

/**
 * Wire the click: POST /api/auth/logout, then reload so the server serves the
 * login page. app.js's fetch wrapper exempts /api/auth/* from its own 401
 * reload, so this is the only reload that fires. A failed POST still reloads:
 * the session may already be gone server-side, and the login page is the
 * honest state either way. Disabled for the duration so a double-click cannot
 * POST twice.
 *
 * @param {HTMLElement|null} button
 * @param {{fetchFn: typeof fetch, reload: () => void}} deps
 */
export function bindLogout(button, { fetchFn, reload }) {
  if (!button) return;
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      await fetchFn("/api/auth/logout", { method: "POST" });
    } catch {
      // fall through — see the doc comment
    }
    reload();
  });
}
```

- [ ] **Step 4: Run it** — `node --test web/tests/logout.test.mjs` → 5 pass.

- [ ] **Step 5: Markup** — `web/public/index.html:2112`, insert immediately before `<sl-icon-button id="theme-toggle" name="moon" label="Toggle theme"></sl-icon-button>`:

```html
                <sl-icon-button id="btn-logout" name="box-arrow-right" label="Log out" title="Log out" style="display:none"></sl-icon-button>
```
(`box-arrow-right` is in Shoelace's default Bootstrap Icons library, the same library `arrow-clockwise` and `moon` come from.)

- [ ] **Step 6: Wire `app.js`**

1. With the other module imports at the top of `web/public/app.js` add
   ```js
   import { applyLogoutVisibility, bindLogout } from "./modules/logout.js";
   ```
2. In the header-button wiring block (after the `btn-refresh-cookies` listener that ends at `:456`, before `// Files tab buttons`) add:
   ```js
       // Status-bar logout — shown by checkSecurityBanner() when auth is on and
       // this session is authenticated (modules/logout.js owns both rules).
       bindLogout(document.getElementById("btn-logout"), {
         fetchFn: (url, opts) => fetch(url, opts),
         reload: () => window.location.reload(),
       });
   ```
3. In `checkSecurityBanner()` (`:1535`), after `const status = await resp.json();` add:
   ```js
         this.authStatus = status;
         applyLogoutVisibility(document.getElementById("btn-logout"), status);
   ```
   and extend the method's leading comment with one line: `// Also the one read of /api/auth/status the logout icon keys off.`

- [ ] **Step 7: Docs**

- `docs/spec/user-interfaces.md:53` — after the `filter-engine.js` row add:
  `| \`web/public/modules/logout.js\` | ~45 | Status-bar logout icon: \`logoutVisible\` (shown only when \`authRequired && authenticated\`, read from \`GET /api/auth/status\` in \`checkSecurityBanner\`) and \`bindLogout\` (click → \`POST /api/auth/logout\` → reload). |`
- `docs/spec/user-interfaces.md:478` — the `/api/auth/logout` row's description gains: ` The Web UI's status bar shows a logout icon (\`btn-logout\`, beside the theme toggle) only while \`authRequired && authenticated\`; its click is this POST followed by a reload.`
- `SPEC.md:616` — the Status bar bullet ends `..., monitor check timers, theme toggle, and a logout icon when a password protects the UI and the session is authenticated`.

- [ ] **Step 8: Gates**

```bash
node --test web/tests/*.test.mjs 2>&1 | grep -E '^ℹ (tests|pass|fail)'
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
go build ./... && go test -count=1 ./internal/web/... ./internal/docs/
```
Expected: `tests 108`/`pass 108`/`fail 0` (103 + 5); routes/web/docs `ok` (the routes package reads `index.html` and `app.js` as source; the docs package resolves the new citations — `logout.js` must exist at the cited path).

- [ ] **Step 9: Commit**

```
feat(web): logout icon in the status bar

Shown beside the theme toggle only while the server requires auth and this
session is authenticated (Q6); click POSTs /api/auth/logout and reloads to
the login page. The rule and the click live in modules/logout.js with pure
node tests.
```

---

### Task 4: TUI wording — `StatusLabel`, hint sentences, `R F` label

**Files:**
- Modify: `internal/tui/styles.go` (after `StatusColor`, `:136-158`)
- Modify: `internal/tui/action_menu.go:115-117`
- Modify: `internal/tui/job_details.go:278`
- Modify: `internal/tui/app_actions.go:570,580,620`
- Test: `internal/tui/styles_status_label_test.go` (new)
- Modify: `docs/spec/user-interfaces.md:238,709,894`; `README.md:425`

**Interfaces:**
- Produces: `func StatusLabel(status string) string` in package `tui`.

`task_list.go:1037` (`renderJob`) uses the raw status only for `StatusIcon`/`StatusColor` — rows print no status text — so the two sites below are the only renders.

- [ ] **Step 1: Write the failing test** — `internal/tui/styles_status_label_test.go`:

```go
package tui

import (
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestStatusLabelMatchesTheWebUI: COOKIES? reads "Auth Required" — the label
// the Web UI already shows (web/public/app.js, statusLabel) — and every other
// status is its own name. Colour and icon key off the raw status, so a wrong
// label here would not change them; this pins the text.
func TestStatusLabelMatchesTheWebUI(t *testing.T) {
	if got := StatusLabel(string(database.StatusCookies)); got != "Auth Required" {
		t.Fatalf("StatusLabel(COOKIES?) = %q, want %q", got, "Auth Required")
	}
	for _, s := range []database.JobStatus{
		database.StatusUpcoming, database.StatusLive, database.StatusDownloading, database.StatusMuxing,
		database.StatusFinished, database.StatusError, database.StatusCancelled, database.StatusQueued,
	} {
		if got := StatusLabel(string(s)); got != string(s) {
			t.Errorf("StatusLabel(%s) = %q, want the status itself", s, got)
		}
	}
	if got := StatusLabel("Whatever"); got != "Whatever" {
		t.Errorf("unknown status must pass through, got %q", got)
	}
}

// TestActionMenuAndHintWording pins the operator-facing strings this arc
// reworded so a later edit cannot drift them apart from the docs.
func TestActionMenuAndHintWording(t *testing.T) {
	app := &App{}
	app.OnForceRefreshCookies = func() (cookies.RefreshResult, error) { return cookies.RefreshResult{}, nil }
	want := map[string][2]string{ // chord → {Label or DisabledReason, HintLabel}
		"R F": {"Refresh Cookies from Browser", "Refresh Cookies"},
	}
	reasons := map[string]string{
		"A R": "no jobs to resume",
		"A I": "no jobs to reinitialize",
	}
	seen := map[string]bool{}
	for _, it := range app.buildMenuItems() {
		if w, ok := want[it.Chord]; ok {
			seen[it.Chord] = true
			if it.Label != w[0] || it.HintLabel != w[1] {
				t.Errorf("%s: Label/HintLabel = %q/%q, want %q/%q", it.Chord, it.Label, it.HintLabel, w[0], w[1])
			}
		}
		if r, ok := reasons[it.Chord]; ok {
			seen[it.Chord] = true
			if it.DisabledReason != r {
				t.Errorf("%s: DisabledReason = %q, want %q", it.Chord, it.DisabledReason, r)
			}
		}
	}
	for _, c := range []string{"R F", "A R", "A I"} {
		if !seen[c] {
			t.Errorf("chord %s missing from buildMenuItems()", c)
		}
	}
}
```
Add `"github.com/vampiricwulf/Moombox/internal/cookies"` to the imports. `internal/tui/cookie_forcerefresh_chord_test.go:24-30,68` shows how an `App` with `OnForceRefreshCookies` set is built for `buildMenuItems()` — if `&App{}` needs more initialisation there, copy that test's construction verbatim.

- [ ] **Step 2: Run it** — `go test -count=1 -run 'TestStatusLabelMatchesTheWebUI|TestActionMenuAndHintWording' ./internal/tui/` → FAIL: `undefined: StatusLabel`.

- [ ] **Step 3: `internal/tui/styles.go`** — insert after `StatusColor`'s closing `}` (before `// StatusIcon returns a display icon`):

```go
// StatusLabel is the text the TUI shows for a job status. Every status is
// its own name except COOKIES?, which reads "Auth Required" — the wording the
// Web UI already uses for that state (2026-09-04 improvement chain, Q5).
// Colour and icon keep keying off the raw status.
func StatusLabel(status string) string {
	if database.JobStatus(status) == database.StatusCookies {
		return "Auth Required"
	}
	return status
}
```

- [ ] **Step 4: Render sites**

- `internal/tui/action_menu.go:116-117`:
  ```go
  	title := truncateString(j.Title, contentW-19)
  	line := fmt.Sprintf("  %s %s %s", icon, statusStyle.Render(padRight(StatusLabel(string(j.Status)), 13)), title)
  ```
  (column 11 → 13 because `Auth Required` is 13 characters; the title budget shrinks by the same two.)
- `internal/tui/job_details.go:278`: `m.addFieldColor("Status", StatusLabel(status), StatusColor(status))` — `status` stays the raw value for the gradient/border code that follows.

- [ ] **Step 5: `internal/tui/app_actions.go`**

- `:570` `DisabledReason: "no resumable jobs",` → `DisabledReason: "no jobs to resume",`
- `:580` `DisabledReason: "no retriable jobs",` → `DisabledReason: "no jobs to reinitialize",`
- `:620` `ActionMenuItem{Chord: "R F", Label: "Force Cookie Refresh", HintLabel: "Force Refresh", Category: "Request"}` → `ActionMenuItem{Chord: "R F", Label: "Refresh Cookies from Browser", HintLabel: "Refresh Cookies", Category: "Request"}`

- [ ] **Step 6: Run the tests** — same command as Step 2 → PASS; then `go test -count=1 ./internal/tui/` → `ok` (any other test that pinned the old strings fails here: fix the assertion to the new string, never the code).

- [ ] **Step 7: Docs**

- `docs/spec/user-interfaces.md:238`: `| \`R F\` | Force Cookie Refresh | ...` → `| \`R F\` | Refresh Cookies from Browser | ...` (keep the third column).
- `docs/spec/user-interfaces.md:709`: `**\`R F\` — Force Cookie Refresh.**` → `**\`R F\` — Refresh Cookies from Browser.**`
- `docs/spec/user-interfaces.md:894` (`COOKIES?` row of the status table): Visual Treatment cell gains `; the TUI labels it \`Auth Required\` (\`StatusLabel\`, \`internal/tui/styles.go\`), as the Web UI does`.
- `README.md:425`: `| R F | Force browser cookie refresh |` → `| R F | Refresh cookies from browser |`.

- [ ] **Step 8: Gates**

```bash
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./internal && go vet ./internal/tui/ && staticcheck ./internal/tui/
go test -count=1 ./internal/tui/ ./internal/docs/
```
Expected: silent statics; two `ok`.

- [ ] **Step 9: Commit**

```
feat(tui): "Auth Required" label, plainer hints, "Refresh Cookies from Browser"

StatusLabel sits beside StatusColor and is what the action menu and job
details print for COOKIES? (Q5 — the Web UI's wording). The disabled
reasons say what the operator can do ("no jobs to resume/reinitialize") and
the R F chord says what it does rather than how hard.
```

---

### Task 5: Web filter — the "Issues" bucket (Error | Cancelled | COOKIES?)

**Files:**
- Modify: `web/public/modules/filter-engine.js:6-10`
- Modify: `web/public/app.js:3842,4101`
- Modify: `web/tests/filter-engine.test.mjs:49-55,69-71`
- Modify: `README.md:443`

**Interfaces:** `status:issues` (new key), `status:errors` (alias), `status:finished` (now `Finished` only). TUI `Filter.String()` at `internal/tui/task_list.go:44` already says `Issues` and its filter already buckets Cancelled there — unchanged.

Filter tokens are not persisted (no localStorage use in `app.js` for them), so the alias exists for hand-typed queries and muscle memory only.

- [ ] **Step 1: Update the tests** — in `web/tests/filter-engine.test.mjs` replace the two bucket tests (`:49-55`) with:

```js
// Bucket membership is checked on its own fixture so the shared `jobs` list
// (which has no Cancelled/COOKIES? rows) keeps every other expectation.
const bucketJobs = [
  { id: "e", title: "", channelName: "", status: "Error", platform: "youtube" },
  { id: "c", title: "", channelName: "", status: "Cancelled", platform: "youtube" },
  { id: "k", title: "", channelName: "", status: "COOKIES?", platform: "youtube" },
  { id: "f", title: "", channelName: "", status: "Finished", platform: "youtube" },
];
function bucketWith(query) {
  return applyFilterTokens(bucketJobs, parseFilterQuery(query)).map(j => j.id);
}

test("status: issues groups Error + Cancelled + COOKIES? (Q4 — the TUI's Issues bucket)", () => {
  assert.deepEqual(bucketWith("status:issues"), ["e", "c", "k"]);
});

test("status: finished is Finished only — Cancelled moved to issues", () => {
  assert.deepEqual(bucketWith("status:finished"), ["f"]);
});

test("status: errors stays an alias of issues for hand-typed queries", () => {
  assert.deepEqual(bucketWith("status:errors"), bucketWith("status:issues"));
});
```
and change the OR test (`:69-71`) to use `status:active|status:issues` (same expected ids `["1","3","4","5"]`).

- [ ] **Step 2: Run** — `node --test web/tests/filter-engine.test.mjs` → the issues test FAILS (`status:issues` matches nothing → `[]`), finished FAILS (`Cancelled` is still in it).

- [ ] **Step 3: `web/public/modules/filter-engine.js:6-10`**

```js
const STATUS_FILTER_MAP = {
  active: ["Downloading", "Live", "Upcoming", "Muxing", "Queued"],
  // "Issues" is everything waiting on a human — failed, cancelled, or parked
  // on credentials — the bucket the TUI's F filter calls Issues too.
  issues: ["Error", "Cancelled", "COOKIES?"],
  finished: ["Finished"],
};
// status:errors was the bucket's name before Cancelled joined it; keep
// hand-typed queries working.
STATUS_FILTER_MAP.errors = STATUS_FILTER_MAP.issues;
```

- [ ] **Step 4: `web/public/app.js`**

- `:3842` `{ type: "status", value: "errors", label: "Errors" },` → `{ type: "status", value: "issues", label: "Issues" },`
- `:4101` `const labels = { active: "Active", errors: "Errors", finished: "Finished" };` → `const labels = { active: "Active", issues: "Issues", errors: "Issues", finished: "Finished" };`

- [ ] **Step 5: `README.md:443`** — `| F | Cycle status filter (All/Active/Errors/Finished) |` → `| F | Cycle status filter (All/Active/Issues/Finished) |`

- [ ] **Step 6: Gates**

```bash
node --test web/tests/*.test.mjs 2>&1 | grep -E '^ℹ (tests|pass|fail)'
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/web/routes/
```
Expected: `pass` = previous total + 1 (three bucket tests replace two), `fail 0`; routes `ok`.

- [ ] **Step 7: Commit**

```
feat(web): "Issues" filter bucket = Error | Cancelled | COOKIES?

Matches the TUI's F filter (Q4). status:errors stays as an alias; Finished
is Finished only. Bucket tests get their own fixture.
```

---

### Task 6: Tooling — fetch-node blob stamp, version.txt line endings, stale smoke-test comment

**Files:**
- Modify: `tools/fetch-node/main.go:11-12,20,104-118,126-130`
- Test: `tools/fetch-node/main_test.go` (new test)
- Modify: `internal/bgutils/embed/.gitignore`
- Modify: `.gitattributes` (after the `internal/docs/citation_allowlist.txt` block)
- Modify: `internal/cookies/job_linux_smoke_test.go:15-17`

**Interfaces:**
- Produces: `func blobsUpToDate(embedDir, wantStamp string) bool`, `const blobStampName = "node-blobs.stamp"`.

Why: on 2026-09-04 `main` had a Node-24 `version.txt` (tracked, updated by the Arc A merge) beside Node-22 `.gz` blobs (gitignored, never rebuilt), and `go run ./tools/fetch-node` said "already up to date" because the skip only checked that the tracked stamp matched and the files existed. A tracked file cannot vouch for untracked ones; the stamp that vouches must be written by the same run that wrote the blobs, beside them, ignored like them.

- [ ] **Step 1: Write the failing test** — append to `tools/fetch-node/main_test.go`:

```go
// TestBlobsUpToDate: the skip decision trusts only the stamp written beside
// the blobs by the run that produced them — never the tracked version.txt,
// which can be newer than the gitignored blobs after a merge (2026-09-04:
// Node 24 version.txt beside Node 22 blobs reported "already up to date").
func TestBlobsUpToDate(t *testing.T) {
	want := versionStamp()
	writeAll := func(dir string) {
		for _, tgt := range nodeTargets() {
			if err := os.WriteFile(filepath.Join(dir, tgt.embedName), []byte("gz"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	stamp := func(dir, s string) {
		if err := os.WriteFile(filepath.Join(dir, blobStampName), []byte(s+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("stamp matches and every blob present → up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, want)
		if !blobsUpToDate(dir, want) {
			t.Fatal("want true")
		}
	})
	t.Run("no stamp → not up to date, even with a matching version.txt", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte(want+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if blobsUpToDate(dir, want) {
			t.Fatal("version.txt must not vouch for the blobs")
		}
	})
	t.Run("stale stamp → not up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, "node@v22.0.0 old")
		if blobsUpToDate(dir, want) {
			t.Fatal("want false")
		}
	})
	t.Run("a missing blob → not up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, want)
		if err := os.Remove(filepath.Join(dir, nodeTargets()[0].embedName)); err != nil {
			t.Fatal(err)
		}
		if blobsUpToDate(dir, want) {
			t.Fatal("want false")
		}
	})
	t.Run("an empty blob → not up to date", func(t *testing.T) {
		dir := t.TempDir()
		writeAll(dir)
		stamp(dir, want)
		if err := os.WriteFile(filepath.Join(dir, nodeTargets()[0].embedName), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if blobsUpToDate(dir, want) {
			t.Fatal("want false")
		}
	})
}
```
Add `"os"` and `"path/filepath"` to the test file's imports if absent.

- [ ] **Step 2: Run** — `go test -count=1 -run TestBlobsUpToDate ./tools/fetch-node/` → FAIL: `undefined: blobsUpToDate`, `undefined: blobStampName`.

- [ ] **Step 3: `tools/fetch-node/main.go`**

Add near the other top-level declarations:
```go
// blobStampName is written beside the .gz blobs by the run that produced them
// and is gitignored with them. It — not the tracked version.txt — is what the
// idempotency check trusts: a checkout can carry a newer version.txt beside
// older blobs (seen 2026-09-04 after the Node 22→24 bump), and a tracked file
// cannot vouch for untracked ones.
const blobStampName = "node-blobs.stamp"

// blobsUpToDate reports whether every embed blob is present and non-empty and
// the stamp beside them matches the pinned manifest.
func blobsUpToDate(embedDir, wantStamp string) bool {
	stamp, err := os.ReadFile(filepath.Join(embedDir, blobStampName))
	if err != nil || strings.TrimSpace(string(stamp)) != wantStamp {
		return false
	}
	for _, tgt := range nodeTargets() {
		info, err := os.Stat(filepath.Join(embedDir, tgt.embedName))
		if err != nil || info.Size() == 0 {
			return false
		}
	}
	return true
}
```
Replace the idempotency block (`:104-118`, from `// Idempotency: skip if every target already exists and version.txt` through the closing `}` of `if allPresent {...}`'s enclosing `if`) with:
```go
	if blobsUpToDate(embedDir, wantStamp) {
		fmt.Printf("fetch-node: already up to date (%s)\n", wantStamp)
		return nil
	}
```
After the `version.txt` write (`:126-128`) add:
```go
	if err := os.WriteFile(filepath.Join(embedDir, blobStampName), []byte(wantStamp+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", blobStampName, err)
	}
```
Update the header comment: `:11-12` `// Idempotent: if internal/bgutils/embed/version.txt already matches the` … → `// Idempotent: if the gitignored stamp beside the blobs (node-blobs.stamp) matches the pinned manifest and every blob is present, nothing is fetched. version.txt is the tracked build input and is not consulted.` (keep the surrounding numbered steps; step 6 at `:20` still says only version.txt is committed — true).

- [ ] **Step 4: `internal/bgutils/embed/.gitignore`** — after `sidecar.tar.gz` add `node-blobs.stamp` with a comment line `# Written by tools/fetch-node beside the blobs; the idempotency stamp.`. Do NOT add it to `internal/docs/citations_test.go`'s `buildArtifacts` — nothing cites it (Global Constraints).

- [ ] **Step 5: Run** — `go test -count=1 ./tools/fetch-node/` → `ok`. Then prove it end to end in the worktree: `go run ./tools/fetch-node` must DOWNLOAD (the worktree's blobs were copied without a stamp) — abort with Ctrl-C-equivalent is not possible in a subagent, so instead run it fully (three ~50 MB downloads, ~1 min) and confirm the second run prints `already up to date`. Paste both first lines.

- [ ] **Step 6: `.gitattributes`** — after the `internal/docs/citation_allowlist.txt text eol=lf` line add:
```
# tools/fetch-node rewrites this stamp with LF; without the pin a
# core.autocrlf=true checkout shows it modified with identical content.
internal/bgutils/embed/version.txt text eol=lf
```
Then `git add --renormalize internal/bgutils/embed/version.txt` and check `git status --porcelain internal/bgutils/embed/version.txt` — if it shows `M`, the renormalised file is part of this commit (content identical; only the index's eol attribute changed).

- [ ] **Step 7: `internal/cookies/job_linux_smoke_test.go:15-17`** — replace
```go
// IT IS NOT RUN IN CI HERE, and that is deliberate rather than an oversight.
// The release workflow cross-compiles the Linux binaries from ubuntu and never
// runs `go test`; the machine this project is developed on is Windows; and the
```
with
```go
// IT IS NOT A CI GATE, and that is deliberate rather than an oversight.
// ci.yml runs the suite on ubuntu, where this file compiles, but it skips
// there because MOOMBOX_LIVE_PGROUP is unset; the machine this project is
// developed on is Windows; and the
```

- [ ] **Step 8: Gates**

```bash
export GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp
gofmt -l ./tools ./internal && go vet ./tools/... && staticcheck ./tools/... ./internal/cookies/
GOOS=linux GOARCH=amd64 go vet ./internal/cookies/
go test -count=1 ./tools/fetch-node/ ./internal/cookies/ ./internal/docs/
git diff --check
```
Expected: silent statics; three `ok`; `git diff --check` silent.

- [ ] **Step 9: Commit**

```
fix(tools): fetch-node trusts a stamp beside the blobs, not version.txt

The tracked version.txt can be newer than the gitignored .gz blobs after a
merge, and the idempotency check took its word for them (2026-09-04: Node 24
stamp beside Node 22 blobs, "already up to date"). The run that writes the
blobs now writes node-blobs.stamp beside them and only that stamp — plus
every blob present and non-empty — skips the fetch. version.txt is pinned
eol=lf so its LF rewrite stops flapping in git status; the Linux pgroup smoke
test's comment says why it skips in CI instead of claiming CI never tests.
```

---

### Task 7: staticcheck becomes a hard, pinned CI gate

**Files:**
- Modify: `.github/workflows/ci.yml:20-22,110-114`
- Modify: `docs/spec/operations.md:220`

**Interfaces:** Consumes Task 1's clean `staticcheck ./...`. Run this task LAST.

- [ ] **Step 1: Confirm the precondition** — `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp staticcheck ./...` prints nothing on the current branch head. If it prints anything, STOP and report BLOCKED with the output.

- [ ] **Step 2: `.github/workflows/ci.yml`**

Header `:20-22`:
```yaml
# staticcheck is a hard gate, pinned to a release rather than @latest:
# staticcheck lags Go releases, and an unpinned install can refuse to build
# against a new toolchain the morning go.mod moves to it.
```
Step `:110-114`:
```yaml
      - name: staticcheck
        run: |
          go install honnef.co/go/tools/cmd/staticcheck@2026.2.1
          staticcheck ./...
```
(`continue-on-error: true` removed; the name loses `(advisory until Arc C)`.)

- [ ] **Step 3: `docs/spec/operations.md:220`** — replace the sentence `\`staticcheck\` is advisory (\`continue-on-error\`) until Arc C of the 2026-09-04 improvement chain removes the seven pre-existing findings and flips it to a hard gate.` with `\`staticcheck ./...\` is a hard gate, installed at a pinned release (\`2026.2.1\` in \`.github/workflows/ci.yml\`) because staticcheck lags Go releases and \`@latest\` can refuse a new toolchain.` Keep the following sentence about the live gates.

- [ ] **Step 4: Gates**

```bash
git diff --check
GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp go test -count=1 ./internal/docs/
node -e "const y=require('fs').readFileSync('.github/workflows/ci.yml','utf8'); if(/continue-on-error/.test(y)) throw new Error('advisory flag still present'); if(!/staticcheck@2026\.2\.1/.test(y)) throw new Error('pin missing'); console.log('ci.yml OK')"
```
Expected: silent; `ok`; `ci.yml OK`.

- [ ] **Step 5: Commit**

```
ci: staticcheck is a hard gate, pinned to 2026.2.1

Arc C removed every pre-existing finding, so the advisory flag goes. Pinned
because staticcheck lags Go releases.
```

---

## Self-Review

**Spec coverage (§6):** (1) five dead symbols + browse conversion + staticcheck gate → Tasks 1 and 7 (Task 1 also removes the write-only `loggedSigRoutes`/`ClearLoggedRoutes` the deletion exposes — recorded as a plan-level ruling in the ledger). (2) `pot_provider_url` five sites + load test → Task 2. (3) README three rows → Task 2. (4) Logout: `#btn-logout` `sl-icon-button` `box-arrow-right`, last-but-one child before the theme toggle, hidden by default, `checkSecurityBanner()` stores `authStatus`, shown when `authRequired && authenticated`, click → POST → reload; jsdom-free node test by ruling (the suite's rule that `player.test.mjs` is the only jsdom suite outranks the spec's word "jsdom"; the test still covers visibility + the POST) → Task 3. (5) Wording: `StatusLabel` beside `StatusColor`, the two sites with the column/budget arithmetic, filter-engine `issues`/`finished`/`errors` alias, `STATUS_OPTIONS`/`_filterTokenLabel` say Issues, node tests + alias test, TUI filter untouched, hints, `R F` label → Tasks 4 and 5. Docs: chord table row, filter row (README), status label row → Tasks 4/5; SPEC.md names the status bar → Task 3. Intake: tls SA1019, `setSysProcAttr`, `testPlayerURL`, `nextSession`, concat → Task 1; fetch-node idempotence, version.txt eol, smoke comment → Task 6; pin + hard gate → Task 7; operations.md action versions already current → no task.

**Placeholders:** none — every edit carries its replacement text or code.

**Type consistency:** `StatusLabel(status string) string` (Task 4 test and code); `blobsUpToDate(embedDir, wantStamp string) bool` + `blobStampName` (Task 6 test and code); `logoutVisible/applyLogoutVisibility/bindLogout` signatures identical in Task 3's module, test, and app.js wiring (`{ fetchFn, reload }`).
