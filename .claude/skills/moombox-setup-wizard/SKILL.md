---
name: moombox-setup-wizard
description: Use when modifying the first-run setup flow, adding setup steps, or changing the setup wizard in either Web UI or TUI
---

# Setup Wizard

The setup wizard has **two modes** (Quick and Advanced) implemented in **both UIs** with feature parity.

## Architecture

```
Quick Setup:  Mode Select → Cookies → Channels → Complete
Advanced:     Mode Select → 8 config sections → Channels → Complete
```

Completion saves config and triggers restart (exit code 42).

## Key Files

| Component | File |
|-----------|------|
| TUI wizard | `internal/tui/setup_wizard.go` |
| Web wizard | `web/public/modules/setup.js` |
| API endpoint | `internal/web/routes/jobs.go` → `SetupRoutes()` |
| API validation | `internal/web/routes/jobs.go` → `validateConfigUpdates()` |
| API application | `internal/web/routes/jobs.go` → `applyConfigUpdates()` |

## Checklist for Adding a Setup Step

### 1. Identify Mode
- **Quick mode**: Only cookies and channels. Adding steps here is rare — keep it minimal.
- **Advanced mode**: 8 sections (Network, Paths, Logs, Monitors, Downloader, Cookies, Channels, Integrations). Most new settings go here.

### 2. TUI Implementation
`internal/tui/setup_wizard.go`:
- **Advanced**: Add field to `advancedSetupSteps` definitions → builds `huh.Form` with `huh.NewInput`/`huh.NewSelect`/`huh.NewConfirm`. Uses `MapAccessor` for map-backed form values.
- **Quick**: Modify `handleSimpleKey()` for the relevant stage
- Apply values in `finishAdvancedSetup()` or `finishSimpleSetup()`

### 3. Web UI Implementation
`web/public/modules/setup.js` (`SetupController` class):
- Add form element in the appropriate step's HTML
- Gather value inline in `finishAdvancedSetup()` (config gathering is done directly in this method, not a separate function)
- Include in payload sent to `POST /api/setup/complete`

### 4. API Handling
`POST /api/setup/complete` in `internal/web/routes/jobs.go`:
- Validation: reuses `validateConfigUpdates()`
- Application: reuses `applyConfigUpdates()`
- Special handling for password hashing, directory creation, yt-dlp plugin install
- Triggers restart via `OnRestart()` callback (500ms delay goroutine)

### 5. Feature Parity
Both UIs must expose the same options. Quick mode is a subset of advanced — ensure the field appears in advanced at minimum.

## Existing Patterns

- **Auto-cookie flow**: `OnStartAutoCookie(platform)` → browser spawns → `OnFinishAutoCookie()` extracts cookies and returns `(cookies.SetupResult, error)` — accepted and verified are separate facts per platform; `OnCancelAutoCookie()` releases the setup slot. Both UIs use the same callbacks. In the TUI the cookie step is also reachable after first run via `R L` (`SetupWizardModel.OpenCookieLogin`, cookie-only mode: Esc and the third row close instead of advancing).
- **Channel editor**: Shared between quick and advanced. Supports add/edit/delete with platform-specific fields (A/Enter/D/Tab keys in TUI).
- **FFmpeg installer**: Web-only elevated install flow (choco/winget with script review dialog). TUI relies on PATH detection.
- **Restart**: Both UIs trigger restart after setup. Web polls `GET /api/setup/status` every 2s for reconnection (up to 2 minutes).

## Common Mistakes

- Adding to advanced but not updating the quick flow when relevant
- Forgetting to include new field in `finishAdvancedSetup()` config gathering (Web) — value never sent to API
- Not testing restart behavior — setup must trigger exit code 42 for launcher respawn
