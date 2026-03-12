# FFmpeg Installer Audit (Pass 2) Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix 12 issues from the second-pass FFmpeg installer audit — freeze/hang scenarios, missing timeouts, error traps, stale UI state, and misleading status text.

**Architecture:** All changes are surgical edits to existing files. 7 tasks grouped by dependency: TUI critical fix first (F1), then backend (F5), then TUI UX (F8), then Web UI changes (F2-F4, F6-F7, F9-F11), then startup timeout (F12). No new files or packages.

**Tech Stack:** Go 1.25, Bubble Tea TUI, Vanilla JS frontend (Shoelace v2.16)

**Spec:** `docs/superpowers/specs/2026-03-12-ffmpeg-installer-audit-2-design.md`

---

## Chunk 1: TUI & Backend fixes

### Task 1: F1 — TUI Ctrl+C unblocked during install/check

The critical fix: when `installing` or `checking` is true, `HandleKey` returns `""` which means the FFmpeg overlay block in `app_keys.go` falls through to `return a, nil` at line 68, never reaching the Ctrl+C handler at line 220. Add a Ctrl+C check before calling `HandleKey`.

**Files:**
- Modify: `internal/tui/app_keys.go:46-68`

- [ ] **Step 1: Add Ctrl+C guard before HandleKey call**

In `internal/tui/app_keys.go`, replace lines 46-68:

```go
	// FFmpeg check overlay takes priority over all other dialogs
	if a.ffmpegCheck.IsVisible() {
		action := a.ffmpegCheck.HandleKey(key)
```

with:

```go
	// FFmpeg check overlay takes priority over all other dialogs
	if a.ffmpegCheck.IsVisible() {
		// Allow Ctrl+C even during install/check (HandleKey blocks all input
		// in those states, so the normal Ctrl+C handler at line 220 is unreachable).
		if key == keyCtrlC {
			return a, tea.Quit
		}
		action := a.ffmpegCheck.HandleKey(key)
```

- [ ] **Step 2: Verify build**

Run: `go build ./internal/tui/...`
Expected: No errors.

- [ ] **Step 3: Verify tests**

Run: `go test ./internal/tui/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/tui/app_keys.go
git commit -m "fix(tui): allow Ctrl+C during FFmpeg install/check (F1)"
```

### Task 2: F5 — Remove phantom scriptPath cleanup from cleanExpiredPending

In `cleanExpiredPending`, `os.Remove(pi.scriptPath)` always fails silently because the script file is only written at `ConfirmInstall` time. Same bug fixed in E1 for `RejectInstall`.

**Files:**
- Modify: `internal/web/routes/ffmpeg.go:416-425`

- [ ] **Step 1: Remove the scriptPath removal line**

In `internal/web/routes/ffmpeg.go`, replace lines 418-423:

```go
	for token, pi := range pendingInstalls {
		if now.Sub(pi.createdAt) > pendingInstallTTL {
			os.Remove(pi.scriptPath)
			os.Remove(pi.resultPath)
			delete(pendingInstalls, token)
		}
```

with:

```go
	for token, pi := range pendingInstalls {
		if now.Sub(pi.createdAt) > pendingInstallTTL {
			// scriptPath is only written to disk at ConfirmInstall time,
			// so it doesn't exist for expired pending installs.
			os.Remove(pi.resultPath)
			delete(pendingInstalls, token)
		}
```

- [ ] **Step 2: Verify build**

Run: `go build ./internal/web/routes/...`
Expected: No errors.

- [ ] **Step 3: Verify tests**

Run: `go test ./internal/web/routes/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/web/routes/ffmpeg.go
git commit -m "fix(ffmpeg): remove phantom scriptPath cleanup in expiry (F5)"
```

### Task 3: F8 — Show "Verifying installation..." during post-install check

After `PrepareInstall` runs directly (already elevated) or `ConfirmInstall` succeeds, the handler dispatches `ffmpegCheckCmd("")` for verification. But the stale `installResult` text ("Checking permissions..." or "Installing with administrator privileges...") persists during the entire verification phase.

In the review mode view (`ffmpeg_check.go:559-561`), `installing` takes priority over `installResult` in the if/else-if chain. So we must also clear `installing = false` to make the message visible.

**Files:**
- Modify: `internal/tui/app_update.go:326-344`

- [ ] **Step 1: Add verify message in ffmpegPrepareResultMsg default branch**

In `internal/tui/app_update.go`, replace lines 332-334:

```go
	} else {
		// Ran directly (already elevated) — verify
		return a, a.ffmpegCheckCmd("")
	}
```

with:

```go
	} else {
		// Ran directly (already elevated) — verify
		a.ffmpegCheck.installing = false
		a.ffmpegCheck.installResult = "Verifying installation..."
		return a, a.ffmpegCheckCmd("")
	}
```

- [ ] **Step 2: Add verify message in ffmpegConfirmResultMsg success branch**

In `internal/tui/app_update.go`, replace lines 341-343:

```go
	} else {
		// Elevated install succeeded — verify FFmpeg is available
		return a, a.ffmpegCheckCmd("")
	}
```

with:

```go
	} else {
		// Elevated install succeeded — verify FFmpeg is available
		a.ffmpegCheck.installing = false
		a.ffmpegCheck.installResult = "Verifying installation..."
		return a, a.ffmpegCheckCmd("")
	}
```

- [ ] **Step 3: Verify build**

Run: `go build ./internal/tui/...`
Expected: No errors.

- [ ] **Step 4: Verify tests**

Run: `go test ./internal/tui/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tui/app_update.go
git commit -m "fix(tui): show 'Verifying installation...' during post-install check (F8)"
```

### Task 4: F12 — Increase startup FFmpeg check timeout to 10s

The startup check in `main.go` uses 3s timeout while `checkFFmpeg` in the backend uses 10s. Slow FFmpeg responses (e.g., antivirus scanning) cause false-positive overlay.

**Files:**
- Modify: `cmd/moombox/main.go:1417`

- [ ] **Step 1: Change 3s timeout to 10s**

In `cmd/moombox/main.go`, replace line 1417:

```go
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 3*time.Second)
```

with:

```go
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
```

- [ ] **Step 2: Verify build**

Run: `go build ./cmd/moombox/...`
Expected: No errors.

- [ ] **Step 3: Commit**

```bash
git add cmd/moombox/main.go
git commit -m "fix: increase startup FFmpeg check timeout to 10s to match backend (F12)"
```

## Chunk 2: Web UI fixes

### Task 5: F2+F3+F4 — Loading indicators, double-click prevention, stale state reset

Three related Web UI issues:
- F2: No loading indicator during custom path check
- F3: Double-click spawns concurrent probes (button/input not disabled)
- F4: `showFFmpegOverlay` doesn't clear stale result elements

**Files:**
- Modify: `web/public/modules/setup.js:143-178` (listeners), `web/public/modules/setup.js:597-603` (showFFmpegOverlay), `web/public/modules/setup.js:789-831` (checkFFmpegPath)

- [ ] **Step 1: Update checkFFmpegPath to accept and use btnId parameter**

In `web/public/modules/setup.js`, replace lines 789-831 (the entire `checkFFmpegPath` method):

```javascript
  async checkFFmpegPath(inputId, resultId, btnId) {
    const input = document.getElementById(inputId);
    const resultEl = document.getElementById(resultId);
    const btn = btnId ? document.getElementById(btnId) : null;
    const path = (input?.value || "").trim();
    if (!path) return;

    // Disable controls to prevent concurrent checks (F3)
    if (btn) btn.disabled = true;
    if (input) input.disabled = true;
    if (resultEl) resultEl.innerHTML = '<sl-spinner style="font-size: 1rem;"></sl-spinner> Checking...';

    try {
      const resp = await fetch("/api/ffmpeg/check", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path }),
      });
      const data = await resp.json();

      if (data.valid) {
        let html = `<sl-alert variant="success" open>Valid: ${this.esc(data.version)}</sl-alert>`;
        if (data.warning) {
          html += `<sl-alert variant="warning" open style="margin-top: 0.5em;">${this.esc(data.warning)}</sl-alert>`;
          html += `<sl-button variant="primary" style="width: 100%; margin-top: 0.75em;" id="ffmpeg-path-continue">Continue</sl-button>`;
        }
        if (resultEl) resultEl.innerHTML = html;
        if (data.warning) {
          document.getElementById("ffmpeg-path-continue")?.addEventListener("click", () => {
            document.getElementById("ffmpeg-overlay").style.display = "none";
            this.initializeApp();
          });
        } else {
          setTimeout(() => {
            document.getElementById("ffmpeg-overlay").style.display = "none";
            this.initializeApp();
          }, 1500);
        }
      } else {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="danger" open>FFmpeg not found at this path</sl-alert>';
        }
      }
    } catch (e) {
      if (resultEl) {
        resultEl.innerHTML = `<sl-alert variant="danger" open>Check failed: ${this.esc(e.message)}</sl-alert>`;
      }
    } finally {
      if (btn) btn.disabled = false;
      if (input) input.disabled = false;
    }
  }
```

- [ ] **Step 2: Update all checkFFmpegPath callers to pass button ID**

In `web/public/modules/setup.js`, update the four callers in `setupListeners()`:

Replace line 148:
```javascript
      this.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result");
```
with:
```javascript
      this.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result", "ffmpeg-check-btn");
```

Replace line 164:
```javascript
      if (e.key === "Enter") this.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result");
```
with:
```javascript
      if (e.key === "Enter") this.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result", "ffmpeg-check-btn");
```

Replace line 169:
```javascript
      this.checkFFmpegPath("ffmpeg-manual-path", "ffmpeg-manual-result");
```
with:
```javascript
      this.checkFFmpegPath("ffmpeg-manual-path", "ffmpeg-manual-result", "ffmpeg-manual-check-btn");
```

Replace line 172:
```javascript
      if (e.key === "Enter") this.checkFFmpegPath("ffmpeg-manual-path", "ffmpeg-manual-result");
```
with:
```javascript
      if (e.key === "Enter") this.checkFFmpegPath("ffmpeg-manual-path", "ffmpeg-manual-result", "ffmpeg-manual-check-btn");
```

- [ ] **Step 3: Clear stale result elements in showFFmpegOverlay (F4)**

In `web/public/modules/setup.js`, replace lines 597-603 (the `showFFmpegOverlay` method):

```javascript
  showFFmpegOverlay() {
    document.getElementById("ffmpeg-overlay").style.display = "flex";
    document.getElementById("ffmpeg-main-view").style.display = "";
    document.getElementById("ffmpeg-install-view").style.display = "none";
    document.getElementById("ffmpeg-script-review").style.display = "none";
    document.getElementById("ffmpeg-manual-install").style.display = "none";
    // Reset stale state from previous overlay opens
    for (const id of ["ffmpeg-check-result", "ffmpeg-install-result",
                       "ffmpeg-confirm-result", "ffmpeg-manual-result"]) {
      const el = document.getElementById(id);
      if (el) el.innerHTML = "";
    }
  }
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: No errors (embedded assets rebuild).

- [ ] **Step 5: Commit**

```bash
git add web/public/modules/setup.js
git commit -m "fix(web): add loading indicators, prevent double-click, clear stale state (F2+F3+F4)"
```

### Task 6: F6+F10+F11 — Client-side timeouts and Trust button re-enable fix

Three related fetch timeout/error-handling issues:
- F6: `confirmElevatedInstall` has no client-side timeout
- F10: Trust button re-enabled after backend consumed the token (retry gets cryptic error)
- F11: `installFFmpeg` direct-install has no client-side timeout

**Files:**
- Modify: `web/public/modules/setup.js:651-693` (installFFmpeg), `web/public/modules/setup.js:736-772` (confirmElevatedInstall)

- [ ] **Step 1: Add AbortController timeout to installFFmpeg (F11)**

In `web/public/modules/setup.js`, replace the `installFFmpeg` method (lines 651-693):

```javascript
  async installFFmpeg(method, btn) {
    const progress = document.getElementById("ffmpeg-install-progress");
    const resultEl = document.getElementById("ffmpeg-install-result");
    const optionsEl = document.getElementById("ffmpeg-install-options");

    // Disable all buttons
    optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = true; });
    if (progress) progress.style.display = "flex";
    if (resultEl) resultEl.innerHTML = "";

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 360000); // 6 minutes

    try {
      const resp = await fetch("/api/ffmpeg/install", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ method }),
        signal: controller.signal,
      });
      const data = await resp.json();

      if (data.needsElevation) {
        // Show script review view
        if (progress) progress.style.display = "none";
        document.getElementById("ffmpeg-install-view").style.display = "none";
        this.showScriptReview(data.script, data.token);
        return;
      }

      if (data.success) {
        this.showInstallSuccess(resultEl, data);
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>${this.esc(data.error || "Install failed")}</sl-alert>`;
        }
        optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = false; });
      }
    } catch (e) {
      if (e.name === "AbortError") {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="warning" open>Install timed out \u2014 try running \'ffmpeg -version\' in a terminal to check if it succeeded.</sl-alert>';
        }
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>Install failed: ${this.esc(e.message)}</sl-alert>`;
        }
      }
      optionsEl.querySelectorAll("sl-button").forEach((b) => { b.disabled = false; });
    } finally {
      clearTimeout(timeoutId);
      if (progress) progress.style.display = "none";
    }
  }
```

- [ ] **Step 2: Add AbortController timeout and fix Trust re-enable in confirmElevatedInstall (F6+F10)**

In `web/public/modules/setup.js`, replace the `confirmElevatedInstall` method (lines 736-772):

```javascript
  async confirmElevatedInstall(token) {
    const progress = document.getElementById("ffmpeg-confirm-progress");
    const resultEl = document.getElementById("ffmpeg-confirm-result");
    const trustBtn = document.getElementById("ffmpeg-trust-btn");
    const distrustBtn = document.getElementById("ffmpeg-distrust-btn");

    if (trustBtn) trustBtn.disabled = true;
    if (distrustBtn) distrustBtn.disabled = true;
    if (progress) progress.style.display = "flex";
    if (resultEl) resultEl.innerHTML = "";

    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), 360000); // 6 minutes

    try {
      const resp = await fetch("/api/ffmpeg/install/confirm", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
        signal: controller.signal,
      });
      const data = await resp.json();

      if (data.success) {
        this.showInstallSuccess(resultEl, data);
      } else {
        // HTTP error response — token was consumed by backend, retry won't work (F10).
        // Show error with Back button instead of re-enabling Trust.
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>${this.esc(data.error || "Install failed")}</sl-alert>`;
        }
        this.appendBackToInstallButton(resultEl);
      }
    } catch (e) {
      // Both timeout and network errors leave token state unknown — re-enable buttons
      if (trustBtn) trustBtn.disabled = false;
      if (distrustBtn) distrustBtn.disabled = false;
      if (e.name === "AbortError") {
        if (resultEl) {
          resultEl.innerHTML = '<sl-alert variant="warning" open>Install timed out \u2014 try running \'ffmpeg -version\' in a terminal to check if it succeeded.</sl-alert>';
        }
      } else {
        if (resultEl) {
          resultEl.innerHTML = `<sl-alert variant="danger" open>Install failed: ${this.esc(e.message)}</sl-alert>`;
        }
      }
    } finally {
      clearTimeout(timeoutId);
      if (progress) progress.style.display = "none";
    }
  }
```

- [ ] **Step 3: Add the appendBackToInstallButton helper**

Add this method after `confirmElevatedInstall` in `web/public/modules/setup.js` (before `rejectElevatedInstall`):

```javascript
  /** Append a "Back to install options" button to the given result container. */
  appendBackToInstallButton(resultEl) {
    if (!resultEl) return;
    const backBtn = document.createElement("sl-button");
    backBtn.variant = "text";
    backBtn.style.cssText = "width: 100%; margin-top: 0.5em;";
    backBtn.innerHTML = '<sl-icon slot="prefix" name="arrow-left"></sl-icon> Back to install options';
    backBtn.addEventListener("click", () => {
      document.getElementById("ffmpeg-script-review").style.display = "none";
      document.getElementById("ffmpeg-install-view").style.display = "";
    });
    resultEl.appendChild(backBtn);
  }
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: No errors.

- [ ] **Step 5: Commit**

```bash
git add web/public/modules/setup.js
git commit -m "fix(web): add fetch timeouts and fix Trust button re-enable (F6+F10+F11)"
```

### Task 7: F7+F9 — Script review cancel button and install options error recovery

Two navigation issues:
- F7: Script review has no cancel/back path (only Trust and Distrust)
- F9: `showFFmpegInstallOptions` fetch error traps user with no Back button

**Files:**
- Modify: `web/public/index.html:1479-1492` (script review markup)
- Modify: `web/public/modules/setup.js:717-733` (showScriptReview), `web/public/modules/setup.js:605-639` (showFFmpegInstallOptions)

- [ ] **Step 1: Add Cancel button markup in HTML**

In `web/public/index.html`, after the flex div containing Trust/Distrust buttons (after line 1486, before `ffmpeg-confirm-progress`), add a Cancel button:

Replace lines 1479-1492:
```html
                    <div style="display: flex; gap: 0.5em;">
                        <sl-button id="ffmpeg-trust-btn" variant="primary" style="flex: 1;">
                            <sl-icon slot="prefix" name="check-lg"></sl-icon> Trust &amp; Continue
                        </sl-button>
                        <sl-button id="ffmpeg-distrust-btn" variant="default" style="flex: 1;">
                            <sl-icon slot="prefix" name="x-lg"></sl-icon> Distrust &amp; Manually Install
                        </sl-button>
                    </div>
                    <div id="ffmpeg-confirm-progress" style="display: none; margin-top: 1em;">
                        <sl-spinner style="font-size: 1.5rem;"></sl-spinner>
                        <span style="margin-left: 0.5em;">Installing with administrator privileges...</span>
                    </div>
                    <div id="ffmpeg-confirm-result" style="margin-top: 0.5em;"></div>
```

with:

```html
                    <div style="display: flex; gap: 0.5em;">
                        <sl-button id="ffmpeg-trust-btn" variant="primary" style="flex: 1;">
                            <sl-icon slot="prefix" name="check-lg"></sl-icon> Trust &amp; Continue
                        </sl-button>
                        <sl-button id="ffmpeg-distrust-btn" variant="default" style="flex: 1;">
                            <sl-icon slot="prefix" name="x-lg"></sl-icon> Distrust &amp; Manually Install
                        </sl-button>
                    </div>
                    <sl-button id="ffmpeg-review-cancel-btn" variant="text" style="width: 100%; margin-top: 0.25em;">
                        Cancel
                    </sl-button>
                    <div id="ffmpeg-confirm-progress" style="display: none; margin-top: 1em;">
                        <sl-spinner style="font-size: 1.5rem;"></sl-spinner>
                        <span style="margin-left: 0.5em;">Installing with administrator privileges...</span>
                    </div>
                    <div id="ffmpeg-confirm-result" style="margin-top: 0.5em;"></div>
```

- [ ] **Step 2: Wire Cancel button in showScriptReview**

In `web/public/modules/setup.js`, replace `showScriptReview` (lines 717-733):

```javascript
  showScriptReview(script, token) {
    const reviewEl = document.getElementById("ffmpeg-script-review");
    const codeEl = document.getElementById("ffmpeg-review-script");
    if (codeEl) codeEl.textContent = script;
    if (reviewEl) reviewEl.style.display = "";

    // Wire trust button
    const trustBtn = document.getElementById("ffmpeg-trust-btn");
    const distrustBtn = document.getElementById("ffmpeg-distrust-btn");

    const newTrust = trustBtn.cloneNode(true);
    trustBtn.replaceWith(newTrust);
    newTrust.addEventListener("click", () => this.confirmElevatedInstall(token));

    const newDistrust = distrustBtn.cloneNode(true);
    distrustBtn.replaceWith(newDistrust);
    newDistrust.addEventListener("click", () => this.rejectElevatedInstall(token));

    // Wire cancel button — rejects token and goes back to install options (not manual)
    const cancelBtn = document.getElementById("ffmpeg-review-cancel-btn");
    if (cancelBtn) {
      const newCancel = cancelBtn.cloneNode(true);
      cancelBtn.replaceWith(newCancel);
      newCancel.addEventListener("click", async () => {
        try {
          await fetch("/api/ffmpeg/install/reject", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ token }),
          });
        } catch { /* ignore */ }
        document.getElementById("ffmpeg-script-review").style.display = "none";
        document.getElementById("ffmpeg-install-view").style.display = "";
      });
    }
  }
```

- [ ] **Step 3: Add Back button to error branch in showFFmpegInstallOptions (F9)**

In `web/public/modules/setup.js`, replace line 638:

```javascript
      optionsEl.innerHTML = `<p style="color: var(--sl-color-danger-600);">Failed to check install options: ${this.esc(e.message)}</p>`;
```

with:

```javascript
      optionsEl.innerHTML = `<sl-alert variant="danger" open>Failed to check install options: ${this.esc(e.message)}</sl-alert>`;
      const backBtn = document.createElement("sl-button");
      backBtn.variant = "text";
      backBtn.style.cssText = "width: 100%; margin-top: 0.5em;";
      backBtn.innerHTML = '<sl-icon slot="prefix" name="arrow-left"></sl-icon> Back';
      backBtn.addEventListener("click", () => {
        document.getElementById("ffmpeg-install-view").style.display = "none";
        document.getElementById("ffmpeg-main-view").style.display = "";
      });
      optionsEl.appendChild(backBtn);
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: No errors.

- [ ] **Step 5: Commit**

```bash
git add web/public/index.html web/public/modules/setup.js
git commit -m "fix(web): add script review cancel button and install error recovery (F7+F9)"
```

## Final Verification

- [ ] **Run full build**: `go build ./...`
- [ ] **Run all tests**: `go test ./...`
- [ ] **Run vet**: `go vet ./...`
