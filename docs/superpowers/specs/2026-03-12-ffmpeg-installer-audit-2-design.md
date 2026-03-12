# FFmpeg Installer Audit (Pass 2) — Design Spec

**Date**: 2026-03-12
**Scope**: Second-pass audit focusing on edge cases, freeze/hang scenarios, and user-facing flow robustness
**Approach**: Fix all issues + light cleanup (same as pass 1)
**Prerequisite**: All 15 fixes from pass 1 (`docs/superpowers/specs/2026-03-11-ffmpeg-installer-audit-design.md`) are applied

## Files In Scope

| File | Lines | Role |
|------|-------|------|
| `internal/tui/app_keys.go` | 412 | Key dispatch (FFmpeg overlay block: lines 46-68) |
| `internal/tui/app_update.go` | 504 | Message handling (FFmpeg handlers: lines 326-376) |
| `internal/tui/ffmpeg_check.go` | 650 | FFmpeg overlay model, views, key handlers |
| `internal/web/routes/ffmpeg.go` | ~692 | API routes, install logic |
| `web/public/modules/setup.js` | 833 | Web UI FFmpeg overlay |
| `web/public/index.html` | ~70 lines FFmpeg markup | HTML markup for FFmpeg overlay (lines 1442-1511) |
| `cmd/moombox/main.go` | ~1660 | Entry point, startup FFmpeg check (line ~1417) |

## Findings & Fixes

### Critical (1 issue)

**F1. TUI: Ctrl+C blocked during install/check** (`app_keys.go:47-68`, `ffmpeg_check.go:221-226`)

When `installing` or `checking` is true, `HandleKey` returns `""` for every key. The key dispatch at `app_keys.go:47` calls `HandleKey`, gets `""`, and falls to `return a, nil` at line 68. The Ctrl+C handler at line 220 is never reached. If the elevated process hangs or the UAC prompt sits unanswered, the user cannot exit the TUI — they must kill the process externally.

**Fix**: In `app_keys.go`, add a Ctrl+C check *before* calling `HandleKey` in the FFmpeg overlay block. This matches how Ctrl+C works at line 220 for normal operation.

```go
if a.ffmpegCheck.IsVisible() {
    if key == keyCtrlC {
        return a, tea.Quit
    }
    action := a.ffmpegCheck.HandleKey(key)
    // ... rest of dispatch
}
```

### Web UI (3 issues)

**F2. No loading indicator for custom path check** (`setup.js:789-831`)

When the user clicks "Check" for a custom path, `POST /api/ffmpeg/check` fires with up to a 10-second timeout. Zero visual feedback during this wait — no spinner, no disabled state. The user might think nothing happened.

**F3. Double-click spawns concurrent probes** (`setup.js:789-831`)

Unlike `installFFmpeg` (which disables all buttons at line 657), `checkFFmpegPath` never disables the Check button or input. Rapid clicks spawn concurrent `ffmpeg -version` processes. Results could arrive out of order, causing flickering.

**Fix for F2+F3**: At the start of `checkFFmpegPath`:
1. Find the button that triggered the check (passed as parameter or found by ID)
2. Disable it and show inline spinner in the result area
3. Re-enable on completion (success, error, or catch)

```javascript
async checkFFmpegPath(inputId, resultId, btnId) {
    const btn = document.getElementById(btnId);
    if (btn) btn.disabled = true;
    if (resultEl) resultEl.innerHTML = '<sl-spinner style="font-size: 1rem;"></sl-spinner> Checking...';
    try {
        // ... existing fetch logic
    } finally {
        if (btn) btn.disabled = false;
    }
}
```

Update all callers to pass the button ID. This applies to:
- `ffmpeg-check-btn` click handler (line 147-148)
- `ffmpeg-custom-path` Enter handler (line 163-164)
- `ffmpeg-manual-check-btn` click handler (line 168-169)
- `ffmpeg-manual-path` Enter handler (line 171-172)

For Enter key handlers (no button click), disable the input element and pass `null` as `btnId`:

```javascript
document.getElementById("ffmpeg-custom-path")?.addEventListener("keydown", (e) => {
    if (e.key === "Enter") this.checkFFmpegPath("ffmpeg-custom-path", "ffmpeg-check-result", "ffmpeg-check-btn");
});
```

The function disables the input as well when it's non-empty:

```javascript
const input = document.getElementById(inputId);
if (input) input.disabled = true;
// ... fetch ...
// finally:
if (input) input.disabled = false;
```

**F4. `showFFmpegOverlay` doesn't reset result elements** (`setup.js:597-603`)

The TUI's `Open()` resets all state. The web's `showFFmpegOverlay` only toggles view visibility — doesn't clear `ffmpeg-check-result`, `ffmpeg-install-result`, `ffmpeg-confirm-result`, or `ffmpeg-manual-result`. Stale error messages persist across overlay re-opens.

**Fix**: Clear all result containers and re-enable buttons in `showFFmpegOverlay`:

```javascript
showFFmpegOverlay() {
    document.getElementById("ffmpeg-overlay").style.display = "flex";
    document.getElementById("ffmpeg-main-view").style.display = "";
    document.getElementById("ffmpeg-install-view").style.display = "none";
    document.getElementById("ffmpeg-script-review").style.display = "none";
    document.getElementById("ffmpeg-manual-install").style.display = "none";
    // Reset stale state
    for (const id of ["ffmpeg-check-result", "ffmpeg-install-result",
                       "ffmpeg-confirm-result", "ffmpeg-manual-result"]) {
        const el = document.getElementById(id);
        if (el) el.innerHTML = "";
    }
}
```

### Backend (1 issue)

**F5. `cleanExpiredPending` removes non-existent `scriptPath`** (`ffmpeg.go:420-421`)

Same pattern fixed in E1 (first audit) for `RejectInstall`: the script file is only written at `ConfirmInstall` time, so `os.Remove(pi.scriptPath)` in the expiry cleanup always fails silently. Should only remove `resultPath`.

**Fix**: Remove the `os.Remove(pi.scriptPath)` line, keep only `os.Remove(pi.resultPath)` with the same defensive comment used in the E1 fix.

### UX (3 issues)

**F6. `confirmElevatedInstall` fetch has no client-side timeout** (`setup.js:748`)

`ConfirmInstall` holds `installMu`, calls `runElevated` (blocks on UAC prompt — no Windows timeout), then `waitForProcess` (up to 5 minutes). The browser fetch has no `AbortController`, so it blocks indefinitely if the UAC prompt sits unanswered.

**Fix**: Add an `AbortController` with a 6-minute timeout (slightly above the 5-minute `installTimeout`). On abort, show a message telling the user to check if FFmpeg was installed by running `ffmpeg -version`.

```javascript
const controller = new AbortController();
const timeoutId = setTimeout(() => controller.abort(), 360000); // 6 minutes
try {
    const resp = await fetch("/api/ffmpeg/install/confirm", {
        signal: controller.signal,
        // ...
    });
} finally {
    clearTimeout(timeoutId);
}
```

The abort catch should show a specific message: "Install timed out — try running 'ffmpeg -version' in a terminal to check if it succeeded."

**F7. Script review has no cancel/back path** (`setup.js:717-733`, `index.html:1470-1492`)

From script review, the only options are "Trust & Continue" and "Distrust & Manually Install". No way to go back to install options to try a different method without going Distrust → Manual → Back → Install → pick method.

**Fix**: Add a "Cancel" text button below Trust/Distrust in the HTML (after the flex div, before `ffmpeg-confirm-progress`). In JS, wire it in `showScriptReview` with a dedicated handler that: (a) sends the reject API call to clean up the pending token, and (b) navigates back to `ffmpeg-install-view` (not manual install — `rejectElevatedInstall` goes to manual, which is the wrong destination for cancel).

```javascript
const cancelBtn = document.getElementById("ffmpeg-review-cancel-btn");
if (cancelBtn) {
    const newCancel = cancelBtn.cloneNode(true);
    cancelBtn.replaceWith(newCancel);
    newCancel.addEventListener("click", async () => {
        try { await fetch("/api/ffmpeg/install/reject", {
            method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ token }),
        }); } catch { /* ignore */ }
        document.getElementById("ffmpeg-script-review").style.display = "none";
        document.getElementById("ffmpeg-install-view").style.display = "";
    });
}
```

**F8. TUI install result text misleading during verification** (`app_update.go:326-344`)

When `PrepareInstall` runs directly (already elevated), the handler dispatches `ffmpegCheckCmd("")` for verification. But `installResult` still shows "Checking permissions..." (from `ffmpeg_check.go:335`) during the entire verification phase. When `ConfirmInstall` succeeds, the stale text is "Installing with administrator privileges..." (from `ffmpeg_check.go:420`). In both cases, the verification phase should have its own message.

**Fix**: Set `installResult` to "Verifying installation..." before dispatching the verify check:
- In `ffmpegPrepareResultMsg` default branch (before `app_update.go:334`): `a.ffmpegCheck.installResult = "Verifying installation..."`
- In `ffmpegConfirmResultMsg` success branch (`app_update.go:342`): `a.ffmpegCheck.installResult = "Verifying installation..."`

### Web UI — Error Recovery (2 issues)

**F9. `showFFmpegInstallOptions` fetch error traps user** (`setup.js:606-638`)

When `showFFmpegInstallOptions` is called, it hides `ffmpeg-main-view` (line 606) and shows `ffmpeg-install-view`. It then fetches `GET /api/ffmpeg/install/prepare` to get available methods. If the fetch fails (network error, server error), only error text is rendered into `optionsEl` (lines 637-638). No Cancel/Back button is rendered — the user is stuck on a view with just an error message and no way to navigate back.

**Fix**: In the catch/error branch of `showFFmpegInstallOptions`, append a "Back" button after the error message that navigates back to `ffmpeg-main-view`:

```javascript
optionsEl.innerHTML = `<sl-alert variant="danger" open>
    <strong>Error:</strong> ${error.message || "Failed to load install options"}
</sl-alert>
<sl-button variant="text" style="margin-top: 0.5rem;"
    onclick="document.getElementById('ffmpeg-install-view').style.display='none';
             document.getElementById('ffmpeg-main-view').style.display='';">
    ← Back
</sl-button>`;
```

**F10. Trust button re-enabled after token consumed** (`setup.js:761-762`, `ffmpeg.go:538`)

In `confirmElevatedInstall`, the Trust button is re-enabled in the error-response branch (lines 761-762) and the catch block (lines 768-769). But `ConfirmInstall` on the backend deletes the pending token (`delete(pendingInstalls, token)`) at line 538 *before* attempting the install. If the install fails and the user clicks Trust again, they get a cryptic "install token not found or expired" error instead of a useful message.

**Fix**: On error in `confirmElevatedInstall`, do not re-enable the Trust button. Instead, show a "Back to install options" link alongside the error message. Only re-enable Trust on network/abort errors where the token may not have been consumed:

```javascript
} catch (err) {
    if (err.name === "AbortError") {
        // Timeout — token state unknown, re-enable Trust
        trustBtn.disabled = false;
        resultEl.innerHTML = "Install timed out — try running 'ffmpeg -version' in a terminal to check if it succeeded.";
    } else {
        // Network error — token state unknown, re-enable Trust
        trustBtn.disabled = false;
        resultEl.innerHTML = `Error: ${err.message}`;
    }
}
// On HTTP error response (token was consumed):
// Don't re-enable Trust — show Back button instead
if (!resp.ok) {
    resultEl.innerHTML = `<sl-alert variant="danger" open>${errorMsg}</sl-alert>
        <sl-button variant="text" onclick="...back to install view...">← Back to install options</sl-button>`;
    // Trust stays disabled
}
```

### Web UI — Timeouts (1 issue)

**F11. `installFFmpeg` direct-install has no client-side timeout** (`setup.js:662`)

The direct-install path (`installFFmpeg`, used when the process is already elevated) calls `POST /api/ffmpeg/install/prepare` with `method` and `confirm: true`. This can block for the full install duration (download + extract + PATH update) with no `AbortController`. Same gap as F6.

**Fix**: Add an `AbortController` with a 6-minute timeout, same pattern as the F6 fix:

```javascript
const controller = new AbortController();
const timeoutId = setTimeout(() => controller.abort(), 360000);
try {
    const resp = await fetch("/api/ffmpeg/install/prepare", {
        signal: controller.signal,
        // ...
    });
} finally {
    clearTimeout(timeoutId);
}
```

The abort catch should show the same message as F6: "Install timed out — try running 'ffmpeg -version' in a terminal to check if it succeeded."

### Startup (1 issue)

**F12. Startup FFmpeg check timeout mismatch** (`cmd/moombox/main.go:1417`, `ffmpeg.go:~80`)

The startup FFmpeg check in `main.go` uses a 3-second `context.WithTimeout`, while `checkFFmpeg` in the backend uses a 10-second timeout. If FFmpeg is installed but responds slowly (e.g., antivirus scanning the binary on first launch), the startup check times out and shows the FFmpeg overlay even though FFmpeg works fine. The user sees a "FFmpeg not found" prompt that resolves itself on retry.

**Fix**: Increase the startup timeout to 10 seconds to match the backend:

```go
checkCtx, checkCancel := context.WithTimeout(context.Background(), 10*time.Second)
```

This is a cold-start check that only runs once — the extra 7 seconds on failure is acceptable.

### Accepted Risks (1 observation)

**F13. Concurrent custom path checks aren't coalesced on backend** (`ffmpeg.go:147`)

`POST /api/ffmpeg/check` calls `checkFFmpeg(path)` directly (uncached). Multiple concurrent requests for the same path each spawn separate processes. Mitigated by: client-side fix F3 (prevents double-clicks) and the optional rate limiter middleware. No backend change needed.
