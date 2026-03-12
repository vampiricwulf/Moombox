# FFmpeg-Shared Installer Audit Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix 15 issues found in the FFmpeg installer subsystem across security, correctness, error handling, UX, and code quality.

**Architecture:** All changes are surgical edits to existing files — no new packages or structural changes. The fixes are grouped into 7 tasks ordered by dependency: constants first (used by later tasks), then security/correctness in `ffmpeg.go`, then TUI fixes, then Web UI fixes.

**Tech Stack:** Go 1.25, `golang.org/x/sync/singleflight` (already in go.mod), Bubble Tea TUI, Vanilla JS frontend

**Spec:** `docs/superpowers/specs/2026-03-11-ffmpeg-installer-audit-design.md`

---

## Chunk 1: Backend fixes (ffmpeg.go)

### Task 1: Add method constants and fix imports (Q1, C3 prep)

Introduces the method string constants used by all later tasks, and adds `strconv` to the import block (needed for C3 fix in Task 3).

**Files:**
- Modify: `internal/web/routes/ffmpeg.go:1-23` (imports), `internal/web/routes/ffmpeg.go:320` (after installTimeout const)

- [ ] **Step 1: Define method constants**

In `internal/web/routes/ffmpeg.go`, after the `installTimeout` const (line 320), add:

```go
// Install method constants — used in switch statements, API validation, and
// generateInstallScript. Frontend sends these as the wire format.
const (
	MethodChoco        = "choco"
	MethodChocoInstall = "choco-install"
	MethodWinget       = "winget"
)
```

- [ ] **Step 2: Replace all method string literals with constants**

Replace every occurrence of the method strings in `ffmpeg.go`:

In `InstallFFmpeg` (line 334-362):
```go
switch method {
case MethodChoco:
	// ...
case MethodChocoInstall:
	// ...
case MethodWinget:
	// ...
default:
```

In `generateInstallScript` (line 403-413):
```go
switch method {
case MethodChoco:
	// ...
case MethodChocoInstall:
	// ...
case MethodWinget:
	// ...
default:
```

In `PrepareInstall` (line 449-452):
```go
switch method {
case MethodChoco, MethodChocoInstall, MethodWinget:
default:
```

- [ ] **Step 3: Verify build compiles**

Run: `go build ./internal/web/routes/...`
Expected: Compiles with no errors.

- [ ] **Step 4: Commit**

```bash
git add internal/web/routes/ffmpeg.go
git commit -m "refactor: add FFmpeg method constants and strconv/singleflight imports (Q1)"
```

---

### Task 2: Security fixes — S1, S2, S3

Fixes PowerShell string injection, generateToken error handling, and ConfirmInstall mutex.

**Files:**
- Modify: `internal/web/routes/ffmpeg.go:429` (S1), `ffmpeg.go:434-438` (S2), `ffmpeg.go:464,470` (S2 callers), `ffmpeg.go:490` (S3)

- [ ] **Step 1: Fix S1 — Use single-quoted PS string for resultPath**

In `generateInstallScript` (line 429), change the `WriteAllText` line from double-quoted to single-quoted for the path:

```go
// Before:
[IO.File]::WriteAllText("` + resultPath + `", $result)

// After:
[IO.File]::WriteAllText('` + resultPath + `', $result)
```

The full script template (lines 418-430) becomes:
```go
	script := `$ErrorActionPreference = 'Continue'
$output = ""
try {
` + installCmd + `
    $exitCode = $LASTEXITCODE
    if ($null -eq $exitCode) { $exitCode = 0 }
} catch {
    $output += $_.Exception.Message
    $exitCode = 1
}
$result = @{ exitCode = $exitCode; output = $output } | ConvertTo-Json
[IO.File]::WriteAllText('` + resultPath + `', $result)
`
```

- [ ] **Step 2: Fix S2 — Return error from generateToken**

Change `generateToken` (line 434-439) to return an error:

```go
// generateToken creates a random 16-byte hex token for pending install tracking.
func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate secure token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
```

Update the two callers in `PrepareInstall`:

Line 464 (resultPath token):
```go
	// Not elevated — generate script for review
	resultToken, err := generateToken()
	if err != nil {
		return false, "", "", err
	}
	resultPath := filepath.Join(os.TempDir(), "moombox-ffmpeg-result-"+resultToken+".json")
```

Line 470 (install token):
```go
	token, err = generateToken()
	if err != nil {
		return false, "", "", err
	}
```

- [ ] **Step 3: Fix S3 — Add installMu to ConfirmInstall**

At the start of `ConfirmInstall` (line 490), acquire the mutex:

```go
func ConfirmInstall(token string) error {
	installMu.Lock()
	defer installMu.Unlock()

	pendingInstallsMu.Lock()
```

- [ ] **Step 4: Verify build compiles**

Run: `go build ./internal/web/routes/...`
Expected: Compiles with no errors.

- [ ] **Step 5: Commit**

```bash
git add internal/web/routes/ffmpeg.go
git commit -m "fix: security hardening for FFmpeg installer (S1, S2, S3)

S1: Use single-quoted PS string to prevent variable interpolation
S2: Propagate crypto/rand errors from generateToken
S3: Serialize ConfirmInstall with installMu to prevent concurrent installs"
```

---

### Task 3: Correctness fixes — C1, C2, C3

Fixes misleading docstring, cache blocking, and unidiomatic Sscanf.

**Files:**
- Modify: `internal/web/routes/ffmpeg.go:28-69` (C2 cache rewrite), `ffmpeg.go:268-275` (C3), `ffmpeg.go:637-638` (C1)

- [ ] **Step 1: Add `strconv` and `singleflight` imports**

In `internal/web/routes/ffmpeg.go`, add `"strconv"` to the stdlib import block (between `"regexp"` and `"runtime"`) and `"golang.org/x/sync/singleflight"` to the third-party block (after `"github.com/go-chi/chi/v5"`).

- [ ] **Step 2: Fix C3 — Replace fmt.Sscanf with strconv.Atoi**

Replace `parseFFmpegVersion` (lines 268-276):

```go
func parseFFmpegVersion(versionLine string) (major, minor int, ok bool) {
	m := ffmpegVersionRe.FindStringSubmatch(versionLine)
	if m == nil {
		return 0, 0, false
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(m[2])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}
```

- [ ] **Step 3: Fix C1 — Correct expandWindowsEnv docstring**

Replace the comment at lines 637-638:

```go
// expandWindowsEnv expands Windows-style %VAR% environment variable references
// by looking up each variable via os.Getenv. Unexpanded variables are kept as-is.
```

- [ ] **Step 4: Fix C2 — Replace blocking cache with singleflight + TTL**

Replace the cache variables and `CheckFFmpegCached`/`InvalidateFFmpegCache` (lines 28-69) with:

```go
// ffmpegCheckCache uses singleflight to coalesce concurrent checks and a TTL
// to avoid re-probing ffmpeg on every request (e.g., 2s polling in pollForRestart).
var (
	ffmpegCacheMu     sync.Mutex
	ffmpegCacheValid  bool
	ffmpegCacheResult bool
	ffmpegCacheVer    string
	ffmpegCacheWarn   string
	ffmpegCachePath   string
	ffmpegCacheTime   time.Time
	ffmpegFlight      singleflight.Group
)

const ffmpegCacheTTL = 10 * time.Second

// CheckFFmpegCached returns a cached result of checkFFmpeg, refreshing
// only if the cache is stale or the path has changed. Concurrent callers
// coalesce into a single ffmpeg probe via singleflight.
func CheckFFmpegCached(path string) (valid bool, version string, warning string) {
	// Fast path: return cached result if still fresh.
	ffmpegCacheMu.Lock()
	now := time.Now()
	if ffmpegCacheValid && ffmpegCachePath == path && now.Sub(ffmpegCacheTime) < ffmpegCacheTTL {
		v, ver, w := ffmpegCacheResult, ffmpegCacheVer, ffmpegCacheWarn
		ffmpegCacheMu.Unlock()
		return v, ver, w
	}
	ffmpegCacheMu.Unlock()

	// Slow path: coalesce concurrent probes via singleflight.
	type cacheResult struct {
		valid   bool
		version string
		warning string
	}
	result, _, _ := ffmpegFlight.Do("check:"+path, func() (any, error) {
		v, ver, w := checkFFmpeg(path)
		return cacheResult{v, ver, w}, nil
	})
	cr := result.(cacheResult)

	// Store result in cache.
	ffmpegCacheMu.Lock()
	ffmpegCacheValid = true
	ffmpegCacheResult = cr.valid
	ffmpegCacheVer = cr.version
	ffmpegCacheWarn = cr.warning
	ffmpegCachePath = path
	ffmpegCacheTime = time.Now()
	ffmpegCacheMu.Unlock()

	return cr.valid, cr.version, cr.warning
}

// InvalidateFFmpegCache clears the cached FFmpeg check result, forcing
// the next call to CheckFFmpegCached to re-probe.
func InvalidateFFmpegCache() {
	ffmpegCacheMu.Lock()
	ffmpegCacheValid = false
	ffmpegCacheMu.Unlock()
}
```

- [ ] **Step 5: Verify build compiles**

Run: `go build ./internal/web/routes/...`
Expected: Compiles with no errors. (`fmt` is still used by `fmt.Errorf`/`fmt.Sprintf` so it stays.)

- [ ] **Step 6: Commit**

```bash
git add internal/web/routes/ffmpeg.go
git commit -m "fix: correctness improvements for FFmpeg cache and parsing (C1, C2, C3)

C1: Fix misleading expandWindowsEnv docstring
C2: Use singleflight to coalesce concurrent ffmpeg probes
C3: Replace fmt.Sscanf with strconv.Atoi"
```

---

### Task 4: Error handling + code quality in ffmpeg.go (E1, E2, E3, Q2)

Fixes RejectInstall cleanup, rate limiter no-op, error message, and duplicated verification.

**Files:**
- Modify: `internal/web/routes/ffmpeg.go:105-109` (E2), `ffmpeg.go:197-211,232-246` (Q2), `ffmpeg.go:522` (E3), `ffmpeg.go:542-549` (E1)

- [ ] **Step 1: Fix E2 — Simplify rate limiter setup**

Replace lines 105-109. The current `r.With()` with no arguments creates a pointless subrouter. Use `r` directly when no rate limiter is configured:

```go
	// Rate-limit mutating ffmpeg endpoints (spawns processes)
	rl := r
	if deps.RateLimit != nil {
		rl = r.With(deps.RateLimit.Middleware)
	}
```

- [ ] **Step 2: Fix Q2 — Extract verifyInstallAndRespond helper**

Add after the `FFmpegRoutes` function (after line 261), before the version regex:

```go
// verifyInstallAndRespond checks FFmpeg availability after installation and
// writes the JSON success or error response. Used by both install and confirm handlers.
func verifyInstallAndRespond(rw http.ResponseWriter, logger interface{ Info(msg string, args ...any) }) {
	InvalidateFFmpegCache()
	valid, version, warning := checkFFmpeg("ffmpeg")
	if !valid {
		jsonError(rw, "FFmpeg installed but not found on PATH. You may need to restart.", http.StatusInternalServerError)
		return
	}
	logger.Info("FFmpeg installed successfully", "version", version)
	jsonResponse(rw, map[string]any{
		"success": true,
		"version": version,
		"warning": warning,
	})
}
```

Then replace the duplicated blocks in the install handler (lines 197-210):

```go
		// Ran directly (already elevated) — verify
		verifyInstallAndRespond(rw, deps.Logger)
	})
```

And in the confirm handler (lines 232-245):

```go
		verifyInstallAndRespond(rw, deps.Logger)
	})
```

- [ ] **Step 3: Fix E3 — Improve ConfirmInstall error message**

At line 522, change:
```go
return fmt.Errorf("install may have succeeded but result file not found — restart to check")
```
to:
```go
return fmt.Errorf("install may have succeeded but result file not found — try running 'ffmpeg -version' in a terminal to verify")
```

- [ ] **Step 4: Fix E1 — Clean up RejectInstall**

Replace `RejectInstall` (lines 542-549):

```go
// RejectInstall cleans up a pending install that the user declined.
func RejectInstall(token string) {
	pendingInstallsMu.Lock()
	if pi, ok := pendingInstalls[token]; ok {
		// scriptPath was never written to disk (only written at ConfirmInstall time).
		// resultPath also doesn't exist yet, but clean up defensively in case a
		// stale elevated process created it.
		os.Remove(pi.resultPath)
		delete(pendingInstalls, token)
	}
	pendingInstallsMu.Unlock()
}
```

- [ ] **Step 5: Verify build compiles**

Run: `go build ./internal/web/routes/...`
Expected: Compiles with no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/web/routes/ffmpeg.go
git commit -m "fix: error handling and dedup in FFmpeg routes (E1, E2, E3, Q2)

E1: RejectInstall only removes resultPath (scriptPath never written)
E2: Simplify rate limiter setup when RateLimit is nil
E3: More actionable error message for missing result file
Q2: Extract verifyInstallAndRespond to deduplicate install/confirm handlers"
```

---

## Chunk 2: TUI fixes

### Task 5: TUI — checking spinner and deduplicated result handler (U1, Q3)

Adds loading indicator for custom/manual path checks and deduplicates the config persistence block.

**Files:**
- Modify: `internal/tui/ffmpeg_check.go:25-70` (add `checking` field), `ffmpeg_check.go:155-165` (spinner routing), `ffmpeg_check.go:212-219` (block input while checking), `ffmpeg_check.go:348-360` (handleCustomKey), `ffmpeg_check.go:419-432` (handleManualKey), `ffmpeg_check.go:495-507` (install view spinner), `ffmpeg_check.go:509-521` (custom view), `ffmpeg_check.go:565-584` (manual view)
- Modify: `internal/tui/app_keys.go:64-66` (batch spinner.Tick with check_custom)
- Modify: `internal/tui/app_update.go:347-382` (Q3 dedup)

- [ ] **Step 1: Add `checking` field to FFmpegCheckModel**

In `ffmpeg_check.go`, add after the `manualValid` field (around line 63):

```go
	// Path check in progress (custom or manual mode)
	checking bool
```

- [ ] **Step 2: Reset `checking` in `Open()`**

In `ffmpeg_check.go`, in the `Open()` method (around line 98, after `m.installError = false`), add:

```go
	m.checking = false
```

This prevents stale `checking` state from a previous overlay session from blocking input on re-open.

- [ ] **Step 3: Route spinner when checking**

In `UpdateComponents` (line 155-165), update the installing check to also handle checking:

```go
	// Route spinner when installing or checking a path
	if m.installing || m.checking {
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return cmd
	}
```

- [ ] **Step 4: Block input while checking**

In `HandleKey` (lines 212-219), add a checking guard after the installing guard:

```go
	if m.installing {
		return "" // Block input while installing
	}
	if m.checking {
		return "" // Block input while checking path
	}
```

- [ ] **Step 5: Set checking state in handleCustomKey and handleManualKey**

In `handleCustomKey` (around line 353-357):
```go
	case keyEnter:
		if m.customPath == "" || m.checking {
			return ""
		}
		m.checking = true
		m.spinner = newSpinner()
		return "check_custom:" + m.customPath
```

In `handleManualKey` (around line 427-431):
```go
	case keyEnter:
		if m.manualPath == "" || m.checking {
			return ""
		}
		m.checking = true
		m.spinner = newSpinner()
		return "check_custom:" + m.manualPath
```

- [ ] **Step 6: Clear checking state when results arrive**

In `SetCustomResult`:
```go
func (m *FFmpegCheckModel) SetCustomResult(result string, valid bool) {
	m.customResult = result
	m.customValid = valid
	m.checking = false
}
```

In `SetManualResult`:
```go
func (m *FFmpegCheckModel) SetManualResult(result string, valid bool) {
	m.manualResult = result
	m.manualValid = valid
	m.checking = false
}
```

Also in `SetInstallResult` (already clears `installing`, but also clear `checking` for safety):
```go
func (m *FFmpegCheckModel) SetInstallResult(result string, isError bool) {
	m.installResult = result
	m.installError = isError
	m.installing = false
	m.checking = false
}
```

- [ ] **Step 7: Show spinner in custom and manual views**

In the `ffmpegCustom` view section (around lines 509-521), add spinner after the text input:

```go
	case ffmpegCustom:
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Enter FFmpeg path:"))
		lines = append(lines, "")
		lines = append(lines, m.textInput.View())

		if m.checking {
			lines = append(lines, "")
			lines = append(lines, m.spinner.View()+" Checking path...")
		} else if m.customResult != "" {
```

In the `ffmpegManual` view section (around lines 565-584), add spinner after the text input:

```go
		lines = append(lines, lipgloss.NewStyle().Foreground(ColorWhite).Bold(true).Render("Enter FFmpeg path:"))
		lines = append(lines, m.textInput.View())

		if m.checking {
			lines = append(lines, "")
			lines = append(lines, m.spinner.View()+" Checking path...")
		} else if m.manualResult != "" {
```

- [ ] **Step 8: Batch spinner.Tick with check_custom in app_keys.go**

In `internal/tui/app_keys.go`, line 64-66, the `check_custom:` handler only returns `a.ffmpegCheckCmd(path)` — it doesn't start the spinner tick loop. Without this, the spinner would never animate. Fix:

```go
		case strings.HasPrefix(action, "check_custom:"):
			path := strings.TrimPrefix(action, "check_custom:")
			return a, tea.Batch(a.ffmpegCheckCmd(path), a.ffmpegCheck.spinner.Tick)
```

This matches the pattern used by `prepare:` and `confirm:` (lines 54, 57).

- [ ] **Step 9: Fix Q3 — Deduplicate TUI result handler**

In `internal/tui/app_update.go`, replace lines 347-382 with:

```go
	case ffmpegCheckResultMsg:
		if msg.Valid {
			switch a.ffmpegCheck.mode {
			case ffmpegCustom:
				a.ffmpegCheck.SetCustomResult("Valid: "+msg.Version, true)
			case ffmpegManual:
				a.ffmpegCheck.SetManualResult("Valid: "+msg.Version, true)
			default:
				a.ffmpegCheck.SetInstallResult("FFmpeg installed: "+msg.Version, false)
			}
			// Persist custom/manual FFmpeg path to config
			if (a.ffmpegCheck.mode == ffmpegCustom || a.ffmpegCheck.mode == ffmpegManual) && msg.Path != "" && a.cfg != nil {
				a.cfg.Paths.FfmpegPath = msg.Path
				if a.OnSaveConfig != nil {
					a.OnSaveConfig(a.cfg)
				}
			}
			a.ffmpegCheck.warning = msg.Warning
			a.ffmpegCheck.successDismiss = true
		} else {
			switch a.ffmpegCheck.mode {
			case ffmpegCustom:
				a.ffmpegCheck.SetCustomResult("Invalid: ffmpeg not found at this path", false)
			case ffmpegManual:
				a.ffmpegCheck.SetManualResult("Invalid: ffmpeg not found at this path", false)
			default:
				a.ffmpegCheck.SetInstallResult("FFmpeg installed but not found on PATH. Restart may be needed.", true)
			}
		}
		return a, nil
```

- [ ] **Step 10: Verify build compiles**

Run: `go build ./internal/tui/...`
Expected: Compiles with no errors.

- [ ] **Step 11: Commit**

```bash
git add internal/tui/ffmpeg_check.go internal/tui/app_keys.go internal/tui/app_update.go
git commit -m "fix: add path-check spinner and deduplicate result handler (U1, Q3)

U1: Show spinner while checking custom/manual FFmpeg paths, block duplicate dispatches
Q3: Collapse duplicated custom/manual config persistence into single condition"
```

---

## Chunk 3: Web UI fixes

### Task 6: Web UI — quit fallback and warning click-to-dismiss (U2, U3)

Fixes the quit button for direct navigation and replaces auto-dismiss with click-to-dismiss when warnings are present.

**Files:**
- Modify: `web/public/modules/setup.js:150-152` (U2), `setup.js:686-698` (U3 showInstallSuccess), `setup.js:786-796` (U3 checkFFmpegPath)

- [ ] **Step 1: Fix U2 — Add fallback message for window.close()**

In `setup.js`, replace the quit button handler (line 150-152):

```javascript
    document.getElementById("ffmpeg-quit-btn")?.addEventListener("click", () => {
      window.close();
      // window.close() only works if the page was opened by script.
      // Show fallback message after a short delay if still open.
      setTimeout(() => {
        const quitBtn = document.getElementById("ffmpeg-quit-btn");
        if (quitBtn) {
          quitBtn.disabled = true;
          quitBtn.textContent = "You can close this tab manually";
        }
      }, 300);
    });
```

- [ ] **Step 2: Fix U3 — Extract shared success handler with click-to-dismiss**

Replace `showInstallSuccess` (around lines 686-698) with a version that uses click-to-dismiss when there's a warning:

```javascript
  showInstallSuccess(resultEl, data) {
    let html = `<sl-alert variant="success" open>FFmpeg installed: ${this.esc(data.version)}</sl-alert>`;
    if (data.warning) {
      html += `<sl-alert variant="warning" open style="margin-top: 0.5em;">${this.esc(data.warning)}</sl-alert>`;
      html += `<sl-button variant="primary" style="width: 100%; margin-top: 0.75em;" id="ffmpeg-success-continue">Continue</sl-button>`;
    }
    if (resultEl) {
      resultEl.innerHTML = html;
    }
    if (data.warning) {
      document.getElementById("ffmpeg-success-continue")?.addEventListener("click", () => {
        document.getElementById("ffmpeg-overlay").style.display = "none";
        this.initializeApp();
      });
    } else {
      setTimeout(() => {
        document.getElementById("ffmpeg-overlay").style.display = "none";
        this.initializeApp();
      }, 1500);
    }
  }
```

- [ ] **Step 3: Fix U3 in checkFFmpegPath — same click-to-dismiss pattern**

Replace the success branch in `checkFFmpegPath` (around lines 786-796):

```javascript
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
```

- [ ] **Step 4: Build to verify embedded assets compile**

Run: `go build ./...`
Expected: Compiles with no errors. (Web assets are embedded via `go:embed`, so the build validates the files exist.)

- [ ] **Step 5: Commit**

```bash
git add web/public/modules/setup.js
git commit -m "fix: Web UI quit fallback and warning click-to-dismiss (U2, U3)

U2: Show 'close this tab manually' fallback when window.close() fails
U3: Replace auto-dismiss with click-to-dismiss when version warnings present"
```

---

### Task 7: Final build verification and full test run

**Files:** None (verification only)

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: Compiles with no errors.

- [ ] **Step 2: Run all tests**

Run: `go test ./...`
Expected: All tests pass.

- [ ] **Step 3: Run vet**

Run: `go vet ./...`
Expected: No issues.
