# User Interfaces

## Scope

This document specifies the two user interfaces provided by Moombox — a Web UI (vanilla JavaScript SPA served by the embedded HTTP server) and a TUI (terminal UI built on the Charmbracelet ecosystem). It covers their architectures, component structures, shared patterns, the WebSocket real-time sync protocol, the complete REST API surface, and the TUI chord system. It is the authoritative reference for any work that touches how users interact with Moombox.

## Rules and Constraints

- **Both UIs are first-class.** Neither the Web UI nor the TUI is a secondary or degraded experience. Feature parity is required. Every user-facing capability must exist in both.
- **Parity respects platform strengths.** The TUI emphasizes real-time feedback, keyboard-driven workflows, and dense information display. The Web UI emphasizes rich media playback, dashboards, and accessibility from any device on the network. The same operation may have different UX in each UI, but the capability itself must be present in both.
- **The TUI uses the Charm ecosystem.** Specifically: `charm.land/bubbletea/v2` (core Elm architecture), `charm.land/bubbles/v2` (pre-built components), `charm.land/huh/v2` (form framework), and `charm.land/lipgloss/v2` (styling/layout). Before building any custom TUI component, always check [Charm's repositories](https://github.com/charmbracelet) for an existing solution. Prefer extending Charm's building blocks over rolling custom implementations. This applies to lists, text inputs, forms, file pickers, tables, progress bars, viewports, spinners, and any other UI primitive.
- **The Web UI uses Shoelace v2.16 via CDN.** It is a vanilla JavaScript SPA — no framework (no React, Vue, Svelte, Angular, or similar). Shoelace provides the component library. Do not introduce a JavaScript framework.
- **WebSocket is the real-time sync mechanism for both UIs.** The Web UI connects directly. The TUI receives updates via Go channels fed from database subscribers that mirror what WebSocket broadcasts to web clients.
- **The TUI communicates with the backend via HTTP.** It makes HTTP requests to `localhost` (the same server the Web UI uses) with a custom `RoundTripper` that injects the `X-Internal-Token` header. This header bypasses CSRF validation. The TUI adjusts the base URL for custom ports and TLS configuration.
- **Static web assets require `go build` after changes.** Assets in `web/public/` are embedded into the binary via `go:embed` in `web/embed.go`. Editing a CSS or JS file has no effect until the binary is recompiled.
- **API route prefix is `/api/` with no version number.** Route registration in Go source and `fetch()` calls in frontend JavaScript must stay in sync. There is no `/api/v1/` or `/api/v2/` — just `/api/`.
- **The TUI must never block the Bubble Tea event loop.** All backend communication is asynchronous (Go commands returning messages). Channel sends from backend goroutines to the TUI use non-blocking operations with drop counters.

---

## Web UI Architecture

### Framework and Technology

The Web UI is a single-page application written in vanilla JavaScript. There is no build step, no bundler, no transpiler, and no framework. The component library is [Shoelace v2.16](https://shoelace.style/), loaded via CDN in `index.html`. All custom elements come from Shoelace; native HTML elements are used where Shoelace does not provide a component.

### File Layout

All static assets live under `web/public/`. The Go embedding is handled by `web/embed.go`, which contains:

```go
//go:embed public/*
var PublicFS embed.FS
```

The file structure:

| File | Lines | Purpose |
|------|-------|---------|
| `web/public/index.html` | — | SPA shell. Loads Shoelace from CDN, defines the base HTML structure, imports `app.js`. |
| `web/public/login.html` | — | Authentication page. Served inline by `AuthMiddleware` when auth is required and the user is not authenticated. The URL bar is preserved (no redirect to `/login`). |
| `web/public/app.js` | ~2,560 | Main SPA module. Job list, unified filter control, log viewer, status bar, WebSocket connection management, settings integration, theme switching. |
| `web/public/modules/player.js` | ~900 | Video player with niconico-style scrolling chat overlay. Handles multi-segment seeking (segments are separate video files that the player stitches together into a seamless timeline). |
| `web/public/modules/setup.js` | ~785 | First-run setup wizard. Walks the user through initial configuration, FFmpeg installation, yt-dlp plugin setup, and cookie capture. |
| `web/public/modules/settings.js` | ~1,600 | Settings dialog. Covers full config editing, channel management, cookie management, integration settings. |
| `web/public/modules/trimmer.js` | ~510 | Trim clip creation UI. Lets the user define start/end timestamps on a finished recording and create a trimmed clip. |
| `web/public/modules/stats.js` | ~160 | Statistics dashboard. Displays job counts, sizes, durations, and other aggregate metrics. |
| `web/public/modules/imports.js` | ~210 | Zip archive import. Upload a zip file containing video/chat/metadata to create a job from external content. |
| `web/public/modules/filter-parser.js` | ~110 | Filter query parser. Booru-style tag syntax: `status:active`, `channel:"name"`, `platform:youtube`, negation (`-tag`), OR groups (`a\|b`), quoting for spaces. |
| `web/public/modules/filter-engine.js` | ~65 | Filter engine. Evaluates parsed tokens against job objects. AND intersection across tokens, OR union within pipe groups. |
| `web/public/modules/utils.js` | ~70 | Shared formatting helpers (durations, file sizes, dates, etc.). |
| `web/public/moombox.css` | ~2,090 | All styles. Includes desktop layout, mobile responsive breakpoints, dark/light theme variables, and component-specific styles. |
| `web/public/favicon.svg` | — | SVG favicon for the web dashboard. |

### Embedding and Serving

The `web.PublicFS` embedded filesystem is mounted by the HTTP server. The server handles cache-busting by appending a build commit hash to asset URLs. Gzip compression is applied via `CompressionMiddleware` for responses under 1MB.

The login page (`login.html`) is not served as a separate route. Instead, `AuthMiddleware` intercepts unauthenticated requests and serves the login page inline, preserving the original URL in the browser's address bar. This means users never see a `/login` URL — they see the page they were trying to reach, with the login form overlaid.

### State Management

The Web UI uses a centralized `MoomboxApp` class (defined in `app.js`) as the single state container. It holds the current job list, filter tokens, theme preference, WebSocket connection, and references to loaded modules.

**Unified filter state:** Each panel (Tasks, Archived) maintains an independent array of filter tokens (`tasksFilterTokens`, `archivedFilterTokens`). Tokens are parsed from user input by `filter-parser.js` and evaluated against jobs by `filter-engine.js`. Structured tokens (status/channel/platform) appear as visual chips (`sl-tag`); free text stays in the input. An optgroup dropdown offers clickable options grouped by Statuses, Platforms, and Channels (auto-populated from current jobs).

Persistent client-side state is stored in `localStorage`:
- Theme preference (dark/light)
- Any module-specific preferences

### Module Loading

Secondary modules (`player`, `setup`, `settings`, `trimmer`, `stats`, `imports`) are loaded via dynamic `import()` when first needed. This keeps the initial page load fast — only `app.js` loads eagerly.

### Mobile Responsiveness

The CSS defines three breakpoints:

| Breakpoint | Target | Behavior |
|------------|--------|----------|
| `992px` | Tablet | Collapses sidebar, adjusts grid layout |
| `768px` | Phone | Single-column layout, touch-optimized spacing |
| `hover: none` media query | Touch devices | Removes hover-dependent interactions, increases touch targets |

---

## TUI Architecture

### Framework and Technology

The TUI is built on the [Charmbracelet](https://github.com/charmbracelet) ecosystem:

| Package | Usage |
|---------|-------|
| `charm.land/bubbletea/v2` | Core framework. Elm architecture: `Model` (state), `View` (render), `Update` (message dispatch). All state transitions happen through message passing. |
| `charm.land/bubbles/v2` | Pre-built components: `list` (task list), `viewport` (log viewer, detail scrolling), `spinner` (loading indicators), `paginator` (page navigation), `key` (key binding definitions). |
| `charm.land/huh/v2` | Form builder framework. Used for the Settings dialog and the Setup Wizard. Provides multi-step forms with inputs, selects, confirms, and validation. |
| `charm.land/lipgloss/v2` | Styling engine. Colors, borders, padding, margin, alignment, and layout composition. Every visual element in the TUI is styled through lipgloss. |

### Layout: Two-Over-One Panel Design with Focus Expansion

The TUI uses a split layout with two panels on top and a full-width log panel on the bottom. The focused panel's row expands to take more space.

**Height split (vertical):**
- Top panel focused (Tasks or Details): top row = 70% height, logs = 30% height
- Logs focused: top row = 25% height, logs = 75% height

**Width split (horizontal, top row only):**
- Tasks focused: tasks = 45%, details = 55%
- Details focused: tasks = 35%, details = 65%
- Logs focused (neither top panel focused): tasks = 50%, details = 50%

```
Example: Task List focused (45% width, 70% height)

┌──────────────────────┬─────────────────────────────────┐
│  Task List (focused)  │      Job Details                 │
│  45% width            │      55% width                   │
│                       │                                   │
│             70% height                                    │
│                       │                                   │
├──────────────────────┴─────────────────────────────────┤
│  Logs (full width, 30% height)                          │
├─────────────────────────────────────────────────────────┤
│  Status Bar                                             │
└─────────────────────────────────────────────────────────┘

Example: Logs focused (100% width, 75% height)

┌─────────────────────────┬──────────────────────────────┐
│  Task List               │   Job Details                 │
│  50% width, 25% height  │   50% width, 25% height      │
├─────────────────────────┴──────────────────────────────┤
│  Logs (focused, full width, 75% height)                 │
├─────────────────────────────────────────────────────────┤
│  Status Bar                                             │
└─────────────────────────────────────────────────────────┘
```

**Task List (top left):** Displays all jobs as a scrollable list. Arrow keys navigate. Enter selects a job and populates the details panel. Status is shown via icons and colors. Divider row separates active from archived jobs; clicking or pressing Enter on the divider toggles archive visibility.

**Job Details (top right):** Shows full metadata for the selected job: title, channel, platform, status, timestamps, progress, output file, quality, and available actions. Content auto-scrolls to accommodate long descriptions.

**Logs (bottom, full width):** Real-time log viewer. Lines arrive via batched messages (250ms flush window). Supports level filtering (debug/info/warn/error) and vim-style regex search (`/` to enter search, `n`/`N` to navigate matches, `Esc` to clear). Matched lines are highlighted in the viewport. Long lines soft-wrap within the available width. Auto-scrolls to newest entries unless the user has manually scrolled up.

**Focus navigation:** `Tab` / `Shift-Tab` cycles focus between panels. Mouse click on a panel changes focus. The focused panel receives keyboard input and has a visually distinct border.

### Source Files

| File | Purpose |
|------|---------|
| `app.go` | Main application model. Init, Update, View. Chord state machine. Menu builder. Action dispatcher. Message routing. Tick management. |
| `task_list.go` | Task list panel (top left). Job list rendering, selection, filtering, archive toggle, status icons. |
| `job_details.go` | Job details panel (top right). Metadata display, description toggle, progress rendering. |
| `log_viewer.go` | Log viewer panel (bottom). Log line buffering, level filtering, regex search, auto-scroll logic. |
| `status_bar.go` | Bottom bar. Chord hints (left), disk usage, active download count, cookie status (right). |
| `action_menu.go` | Command palette overlay (M key). Searchable list of all available actions. |
| `help.go` | Help overlay (? key). Displays all chords grouped by category. |
| `add_video.go` | Add Video overlay. Multi-step flow: URL input, format selection, timestamp configuration, confirmation. |
| `import_dialog.go` | Import overlay. Zip file upload with title/channel override fields. |
| `trim_dialog.go` | Trim overlay. Start/end time input, async encoding with progress display. |
| `files_dialog.go` | Orphaned files overlay. Browse and delete files that have no corresponding job. |
| `client_tokens_dialog.go` | Client token management overlay. List and delete persistent auth tokens. |
| `settings.go` | Settings overlay. Built with `huh` form framework. Full config editing. |
| `settings_mouse.go` | Mouse support for the Settings overlay: click tabs, fields, toggles/cycles, and action buttons. |
| `setup_wizard.go` | First-run setup overlay, and — via `R L` — the standalone cookie-login step on a configured install. Built with `huh`. Config, FFmpeg, yt-dlp plugin, cookies. |
| `ffmpeg_check.go` | FFmpeg validation/installation overlay. |
| `styles.go` | Lipgloss style definitions. Colors, borders, padding for all visual elements. |
| `keys.go` | Key binding definitions using `bubbles/key`. |
| `mouse.go` | Mouse event handling. Click-to-focus, scroll delegation, region hit testing. |
| `marquee.go` | Scrolling text animation for long strings that do not fit in available width. |
| `text_input.go` | Custom text input component (extends bubbles). |
| `progress_store.go` | Tracks download progress state for active jobs. |

### The Chord System

The chord system is the TUI's keyboard shortcut mechanism. It uses a prefix-key pattern where the user presses a prefix key followed by an action key, with an optional third confirmation key for destructive actions.

#### State Machine

The chord system is a three-state finite automaton:

1. **Idle** — No prefix active. Waiting for a prefix key or single-key shortcut.
2. **Prefix active** — A prefix key has been pressed. Waiting for the action key. A feedback message shows the available actions for this prefix.
3. **Confirm active** — A two-key chord was entered for a destructive action (`NeedsConfirm: true`). Waiting for the user to press the action key again to confirm.

**Timeout:** All chord states expire after **3 seconds** of inactivity. If the user presses a prefix key and does nothing for 3 seconds, the chord resets to Idle. If a confirm prompt is pending and 3 seconds pass, it also resets.

**Invalid keys:** If the user presses a key that does not match any valid action for the current prefix, the chord resets to Idle and the key is re-evaluated as a potential new prefix or single-key shortcut.

#### Single Source of Truth

`buildMenuItems()` in `app.go` is the **single source of truth** for all chords. It returns a slice of `ActionMenuItem` structs, each defining:

- `Chord` — the key combination (e.g., `"A A"`, `"R C"`, `"F"`)
- `Label` — full description (e.g., `"Add Video"`)
- `HintLabel` — abbreviated label for the status bar hint
- `Category` — grouping for the help overlay and action menu
- `NeedsJob` — whether the action requires a selected job
- `NeedsConfirm` — whether the action requires a third confirmation keypress
- `JobFilter` — optional predicate that filters which jobs the action applies to

`dispatchAction(chord, job)` in `app.go` is the **unified handler** that executes the action for any chord. Adding a new chord requires exactly two changes: one entry in `buildMenuItems()` and one case in `dispatchAction()`.

#### Prefix Keys

| Prefix | Category | Description |
|--------|----------|-------------|
| `A` | Action | Job manipulation: add, import, retry, cancel, delete, trim, orphans, tokens |
| `R` | Request | Backend requests: cookies, updates, signature verification, restart |
| `O` | Open | Open external resources: folder, stream page, web UI |
| `Q` | Quit | Application exit |

#### Complete Chord Catalog

**Action chords (A prefix):**

| Chord | Action | Requires Job | Confirm | Job Filter |
|-------|--------|:------------:|:-------:|------------|
| `A A` | Add Video dialog | No | No | — |
| `A I` | Import Archive | No | No | — |
| `A R` | Retry Job | Yes | No | Status is Error, Cancelled, or COOKIES? |
| `A C` | Cancel Job | Yes | Yes | Status is not Finished, Cancelled, or Error |
| `A D` | Delete Job | Yes | Yes | Any job |
| `A T` | Trim Video | Yes | No | Status is Finished and has output file |
| `A K` | Manage Client Tokens | No | No | Client tokens callback configured |
| `A O` | Browse Orphaned Files | No | No | — |

**Request chords (R prefix):**

| Chord | Action | Condition |
|-------|--------|-----------|
| `R B` | Re-scan Feed History | Backfill rescan callback is configured. Forces a full-catalog backfill re-scan of every configured YouTube channel. |
| `R C` | Recheck Cookies | Cookie recheck callback is configured |
| `R F` | Force Cookie Refresh | Cookie force-refresh callback is configured |
| `R L` | Cookie Login | Interactive-setup callback is configured (`SetSetupCallbacks`, bound unconditionally by `cmd/moombox`). Opens the setup wizard's cookie step **alone** — pick YouTube or Twitch, sign in in the browser that opens on the host, `Enter` extracts. Preselects the platform the status bar is flagging for re-login. |
| `R V` | Check for Updates | Update check callback is configured |
| `R N` | View Release Notes | Always available. Shows pending-update notes when an update is available; otherwise fetches current version's notes from GitHub. From inside the overlay: `U` applies the update, `Esc`/`Q` closes. |
| `R U` | Apply Update | An update is available and apply callback is configured |
| `R S` | Verify Signature | Signature verification callback is configured |
| `R P` | Restart Program | Restart callback is configured. Requires confirmation. |

**Open chords (O prefix):**

| Chord | Action | Requires Job | Job Filter |
|-------|--------|:------------:|------------|
| `O F` | Open Folder (explorer) | Yes | Job has an openable folder |
| `O S` | Open Stream Page (browser) | Yes | Job has a stream URL |
| `O W` | Open Web UI (browser) | No | — |
| `O C` | Copy Stream URL to clipboard (OSC 52) | Yes | Job has a stream URL |
| `O G` | Open GitHub Page (browser) | No | — |

**Single-key shortcuts:**

| Key | Action |
|-----|--------|
| `F` | Cycle filter mode (All / Downloading / Finished / Error / etc.) |
| `M` | Open Action Menu (command palette) |
| `` ` `` | Open Settings dialog |
| `?` | Open Help overlay |
| `/` | Enter log search mode (log panel focused only). `n`/`N` navigate to next/previous match. `Esc` clears search and returns to normal scroll. |

**Quit chord:**

| Chord | Action | Confirm |
|-------|--------|:-------:|
| `Q Q` | Quit application | Yes (the second Q is the confirmation) |

### Overlays (Modal Dialogs)

Overlays are full-screen or near-full-screen modal views that take over keyboard input. When an overlay is active, the panel layout is hidden and all input routes to the overlay. Pressing `Escape` closes most overlays.

| Overlay | Trigger | Description |
|---------|---------|-------------|
| Help | `?` | Displays all chords grouped by category with descriptions. Read-only. |
| Action Menu | `M` | Command palette. Searchable list of all available actions. Selecting an item executes it. |
| Add Video | `A A` | Multi-step form: (1) enter URL, (2) fetch and select format, (3) set timestamps, (4) confirm. Format fetch is async with a spinner. On error, auto-advances past format selection after a timeout. |
| Import | `A I` | Zip import form with title and channel override fields. |
| Trim | `A T` | Clip creation. Enter start/end seconds. Encoding runs asynchronously with a progress callback that updates the UI. |
| Orphaned Files & History | `A O` | Two sections in one list (with a divider): orphaned files in the output directory with no corresponding job, and orphaned processing-history rows (history entries with no matching job, which otherwise block re-discovery). Each section loads independently — a failure in one is shown inline without hiding the other. Delete with confirmation. |
| Client Tokens | `A K` | List of persistent client authentication tokens. Delete individual tokens. |
| Settings | `` ` `` | Full config editor built with the `huh` form framework. Supports full mouse interaction (click tabs, fields, toggles, cycle options, and action buttons). Action buttons at the bottom: `[ Save & Return ]` / `[ Return Without Saving ]` (when dirty), or `[ Return ]` (when clean). Presents a close confirmation when there are unsaved changes and the user attempts to dismiss. Smart dirty tracking: reverting a field back to its original value clears the dirty flag. Job detail panel renders clickable hyperlinks (OSC 8) for stream URLs and output paths. |
| Setup Wizard | First run, `R L` | Multi-step initial setup: configuration, FFmpeg check/install, yt-dlp plugin, cookie capture. Built with `huh`. `R L` opens the same overlay in **cookie-only** mode: the cookie step with no stages around it, `Esc` and the third row close it instead of advancing, and leaving cancels any browser it opened. |
| FFmpeg Check | Setup flow | Validates FFmpeg is on PATH. Offers installation options if missing. On Linux, also shows the distro-appropriate package manager command (`apt`, `dnf`, `pacman`, etc.) from `GET /api/ffmpeg/install-suggestion`. |
| Release Notes | `R N` | Shows release notes for the pending update (when an update is available) or the current version (fetched from GitHub). Rendered via `glamour` in the TUI. From inside: `U` applies the update, `Esc`/`Q` closes. Uses `bubbles/viewport` for scrolling. |

### Async Message Types

The TUI receives backend state changes via typed messages delivered through Bubble Tea's command/message system. Backend goroutines send to Go channels; the TUI polls these channels via commands and converts received values into Bubble Tea messages.

**Core data messages (from backend channels):**

| Message Type | Source | Content |
|--------------|--------|---------|
| `JobUpdateMsg` | Database subscriber | Single job that changed. Contains the full `*database.Job`. |
| `JobsUpdateMsg` | Database subscriber | Full job list changed (job added or deleted). Contains `[]*database.Job`. |
| `LogBatchMsg` | Logger subscriber | Batch of log lines accumulated over a 250ms flush window. Contains `[]string`. |
| `CheckTimersMsg` | Monitor callbacks | Next check times for Feed, DECAPI, and Twitch monitors. |
| `CookieStatusMsg` | Cookie service | `{YT, TW, YTActive, TWActive}` — one `CookieStatus` per platform (`None`, `OK`, `CookiesOnly`, `Relogin`, `Unknown`) plus each platform's active flag. There is no *expired* state: expiry has no UI reader at all. See §Status Bar. |
| `DiskStatusMsg` | Disk monitor | Disk usage percentage and warning/critical thresholds. |
| `UpdateStatusMsg` | Updater | New version available (tag name, release notes). |

**Internal tick messages:**

| Message Type | Interval | Purpose |
|--------------|----------|---------|
| `tickMsg` | 1 second | Main tick. Updates clocks, checks chord timeouts, refreshes dynamic content. |
| `progressTickMsg` | 16ms (active) / 500ms (idle) | Progress bar animation. Runs at ~60fps during active downloads, drops to 2fps when idle to save CPU. |
| `logFlushMsg` | 250ms | Triggers flushing accumulated log lines from the buffer to the viewport. |
| `marqueeTickMsg` | 150ms | Advances scrolling marquee text for overflowed labels. |

**Async operation result messages:**

These are returned by Bubble Tea commands that perform HTTP requests to the backend. Each carries the operation result (success data or error).

| Message | Operation |
|---------|-----------|
| `updateCheckResultMsg` | Manual update check |
| `updateApplyResultMsg` | Update download and apply |
| `signatureVerifyResultMsg` | Ed25519 signature verification |
| `fetchFormatsResultMsg` | Format list fetch for Add Video |
| `fetchFormatsAutoAdvanceMsg` | Timer to auto-skip format selection on error |
| `addVideoResultMsg` | Job creation result |
| `importResultMsg` | Zip import result |
| `createTrimResultMsg` | Trim creation result |
| `deleteTrimResultMsg` | Trim deletion result |
| `fetchOrphansResultMsg` | Orphaned file list fetch |
| `deleteOrphanResultMsg` | Orphaned file deletion |
| `ffmpegCheckResultMsg` | FFmpeg PATH check |
| `ffmpegPrepareResultMsg` | FFmpeg download preparation |
| `ffmpegConfirmResultMsg` | FFmpeg install confirmation |
| `cookieRecheckResultMsg` | Cookie recheck |
| `cookieForceRefreshResultMsg` | Cookie force refresh |
| `channelResolvedMsg` | Channel URL/name resolution |
| `fetchClientTokensResultMsg` | Client token list fetch |
| `deleteClientTokenResultMsg` | Client token deletion |
| `setupCookieFinishMsg` | Setup wizard cookie step completion |

### Non-Blocking Channel Communication

Backend goroutines deliver data to the TUI via Go channels. These sends are **always non-blocking**. The pattern:

```
select {
case ch <- msg:
    // delivered
default:
    // channel full — drop and increment counter
    dropCount++
}
```

If a send is dropped, a drop counter increments. The next successful send triggers a full state refresh (re-fetching all jobs from the database) so the TUI catches up on missed intermediate updates. This ensures the TUI never blocks a backend goroutine, even if the Bubble Tea event loop is busy processing a complex view update.

### Backend Communication

The TUI makes HTTP requests to the same server that serves the Web UI. It uses a custom `http.RoundTripper` that:

1. Injects the `X-Internal-Token` header on every request. This token (a 16-byte random hex string generated at server startup) bypasses CSRF validation, since the TUI cannot perform origin/referer checks.
2. Adjusts the base URL to match the server's actual bound port and TLS configuration. If the server is configured for HTTPS, the TUI uses `https://`.

This means the TUI has the same API surface as the Web UI — it calls the same `/api/` endpoints. The only difference is the CSRF bypass via the internal token header.

### Status Bar

The status bar occupies the bottom row of the terminal. It displays at-a-glance system health and state:

The left side shows chord hints (key labels for A, R, O, F, M, Tab, backtick, ?) — compact mode when width < 100 chars. The right side shows metrics and status.

**Everything on the right is rendered against a width tier** (`barTier` in `internal/tui/status_bar.go`): `tierFull` → `tierCompact` → `tierKeys` → `tierTight` → `tierEssential` → `tierNone`. The tier decides both the label length and whether an element appears at all, and the rule the cookie indicators follow is that **reassurance is dropped before an alarm is**.

| Element | Content | Behavior |
|---------|---------|----------|
| Connectivity | `OFFLINE`, abbreviated to `OFF` at `tierTight` | An alert, so it outlives every counter and only ever abbreviates |
| Batch selection | `N selected`, `N sel` at `tierKeys` | Shown when > 0 and only at `tierKeys` or wider; the count is the point, so it abbreviates before disappearing |
| Backfill scan | `Backfill <channel>: <tab> p<n>`, `BF:<tab> p<n>` at `tierCompact` | Routine background activity, so it is the first thing dropped (gone at `tierKeys`). One in-flight scan at a time — scans are serial — and the display name is clipped to 16 runes. |
| Disk Usage | `Disk 45% (120G free)` → `D:45% 120G` → `D:45%` | Green normally, yellow (`warn`), red (`critical`). Survives `tierEssential` only when warn/critical — a healthy disk says nothing there. |
| Active Downloads | Count of `Downloading` / `Live` / `Muxing` jobs | Shown when > 0 and only at `tierKeys` or wider. `Queued` is deliberately excluded — a queued job is waiting for an archive slot, not downloading. |
| Cookie Status | Per-platform `YT` / `TW` indicators | `renderCookieStatus`. See below. |

**Cookie indicators.** One indicator per *active* platform (`SetActivePlatforms`, fed from `config.GetActivePlatforms`); an inactive platform renders nothing. Each is a `CookieStatus` (`internal/tui/status_bar.go`), projected from one `cookies.AuthStatus` triple by `cookieBadgeFor` in `cmd/moombox/tui_wiring.go` — `authenticated` wins outright, then "no cookies at all" reports `None` whatever the verdict says, then `RefreshFailed` with cookies present is `CookiesOnly`, and everything else is `Unknown`. `cookieBadgeFor` never returns `Relogin` — that state is applied one level up, where `authStatusToTUI` (same file) overwrites the badge whenever `AutoCookieService.ReloginStatus()` flags the platform, unconditionally and not gated on `auto_enabled`, matching the Web badge's first arm. `hasCookies` is the loose predicate on both sides, so a half-cleared jar reads as configured rather than as never-set-up (see `data-and-storage.md §Cookie Jar`). **At `tierFull` a flagged badge also names `R L`**, the chord that opens the login, once for the bar however many platforms are flagged. It is dropped at the first squeeze on purpose: the alert is the information, the remedy is also in the menu and in help, and the hint is the widest thing this section can add — `metricTiers` must narrow monotonically or `fitTiers`' first-fit-is-richest-fit scan stops holding. The badge and the chord read one predicate, `ReloginPlatform`, so the platform named in the bar is the platform the overlay opens on.

| State | Render | Colour | Tier behavior |
|-------|--------|--------|---------------|
| `CookieStatusRelogin` | `YT: Re-login` / `TW: Re-login`, abbreviated to `YT!` / `TW!` at `tierTight` | Red | Survives to `tierEssential` |
| `CookieStatusCookiesOnly` | `YT` / `TW` | Red | Survives to `tierEssential` |
| `CookieStatusUnknown` | `YT: Unknown` / `TW: Unknown`, abbreviated to `YT` / `TW` at `tierTight` | Warning | **Dropped** at `tierEssential` |
| `CookieStatusNone` | `YT` (YouTube only) | Yellow | Dropped at `tierEssential` |
| `CookieStatusOK` | `YT` / `TW` | Green | Dropped at `tierEssential` |
| default — Twitch `None`, or any unmapped value on either side | `YT` / `TW` | Dim | Dropped at `tierEssential` |

`CookieStatusUnknown` sits in the dropped group **on purpose**. "The last check could not reach YouTube" is not something the operator can act on; it used to render as the always-visible red `CookiesOnly` alert, so a DNS blip shouted at the volume of a dead session for as long as it lasted. Only a conclusive rejection and a re-login prompt earn the surviving red. Twitch without cookies is ordinary anonymous mode and takes the neutral dim indicator, unlike YouTube's yellow.

**A job parked in `COOKIES?` escalates its own platform's indicator to the surviving red**, ranked immediately below `Relogin` and above every check-derived state. `parkedCookieJobs` attributes the park per platform — an absent `Platform` counts as YouTube, matching every other platform test in the TUI — and deliberately does **not** filter on `ParkReason`: membership parks, auth parks and the pre-v18 zero value all escalate, because in all three the remedy is credentials of some kind. What the red badge means is therefore "a download stopped for want of usable credentials", not "your cookies expired"; the job detail panel carries the difference. A park is evidence from a real download attempt, so it outranks a check that merely asked.

**The reason strings are deliberately absent from this bar.** `cookies.AuthStatus` carries `YouTubeError` / `TwitchError` — why a check reached `Unknown`, or, for Twitch, which of `NoteTwitchAuthLoss`'s five routes marked the platform — the four chat-downgrade routes plus the playback-token route — each a fixed sentence — and they have readers on the two **per-request** paths only: the REST cookie-status payload and the TUI's `R C` result line. This panel is push-driven, fed from `RefreshService.OnAuthChange`, and `authStatusChanged` (`internal/cookies/refresh.go`) excludes the two strings from its change-detection gate. That exclusion is a **contract**, not an oversight: no `OnAuthChange`-driven surface may render them, because a reason-only change produces no push and the line would sit stale beside a verdict that is still correct. Widening the gate is the precondition for putting a reason here, and it is a later owner's code change. Until then the operator gets the reason on the next `R C`.

---

## WebSocket Protocol

### Connection Lifecycle

The Web UI establishes a WebSocket connection to the server on page load. The server uses `nhooyr.io/websocket` for WebSocket handling.

**Upgrade:** The WebSocket upgrade handler is registered as an interceptor on the main HTTP handler. Any request with an `Upgrade: websocket` header is routed to the WebSocket handler regardless of the URL path. Origin validation checks that the request comes from the same origin or a loopback/LAN alias.

**Authentication:** For external (non-loopback, non-private-network) connections when auth is configured, the `AuthCheck` function validates the upgrade request before accepting. Unauthenticated external WebSocket upgrades are rejected.

**Detached context:** The accepted WebSocket connection uses `context.Background()` rather than the HTTP request context. This prevents the connection from being killed by the server's `ReadTimeout`, which would otherwise close long-lived connections.

**Initial state:** Immediately after connection, the server sends an `initial_state` message containing the full current state: all jobs, buffered log lines (up to 200 from the ring buffer), and monitor check schedule (next check times for Feed, DECAPI, and Twitch monitors). This allows the client to hydrate without making separate REST calls.

### Message Format

All messages use JSON with this structure:

```json
{
    "type": "<message_type>",
    "payload": <any>
}
```

The `type` field is a string discriminator. The `payload` field varies by type.

### Server-to-Client Message Types

| Type | Payload | When Sent |
|------|---------|-----------|
| `initial_state` | `{ jobs, logs, config, monitors, hideFinishedAgeDays, ... }` | Once, immediately after WebSocket connection is accepted |
| `job_update` | Single job object | When any field of a single job changes |
| `jobs_update` | Full job array | When a job is added or deleted (full list, not incremental) |
| `job_deleted` | `{ id }` | When a job row is removed from the database |
| `config_update` | Partial config (currently `{ hideFinishedAgeDays }`) | When a config setting that affects client-side rendering changes |
| `log` | Log line string | When a new log line is emitted |
| `check_timers` | `{ feed, decapi, twitch }` timestamps | When monitor check schedules change |
| `backfill_status` | `{ channel, tab, pages, state }` | Feed-history backfill scan progress per channel (`state`: scanning / error / done / idle). Active scans are also seeded via `initial_state`. |
| `pong` | Empty | Response to client `ping` messages |

### Client-to-Server Message Types

| Type | Payload | Purpose |
|------|---------|---------|
| `ping` | Empty | Client-initiated keepalive. Server responds with `pong`. |

### Broadcast Rate

Job update broadcasts are not throttled in the WebSocket hub. The only high-frequency caller is `OnJobChange` driven by `ProgressTracker.maybeUpdate`, which is already gated to ~60 Hz per job by `progressUpdateInterval = 16ms` (see `internal/worker/progress.go`); every other `UpdateJobFields` caller is event-driven (state transitions, not loops). A previous per-job throttle in the hub created an ordering race — because `BroadcastJobDeleted` is not throttled, the trailing edge could arrive after a delete and resurrect the row via the client's upsert handler.

### Connection Parameters

| Parameter | Value |
|-----------|-------|
| Ping interval | 30 seconds (server-initiated) |
| Write timeout | 10 seconds per message |
| Max message size (read limit) | 1 MB |
| Backpressure limit | 256 KB per client (messages dropped if write buffer exceeds this) |
| Log ring buffer | 200 lines (oldest evicted when full) |

---

## REST API Routes — Complete Catalog

All routes use the `/api/` prefix unless otherwise noted. PO Token routes use bare paths for yt-dlp plugin compatibility.

### Authentication

This is **dashboard** authentication — the operator's password and session. It is unrelated to the platform credentials in §Cookies below, which is why the two never share a payload.

| Method | Path | Rate Limit | Notes |
|--------|------|:----------:|-------|
| `GET` | `/api/auth/status` | — | Public. Returns `{ authRequired, authenticated, hasPassword, passwordlessExternal }` (`AuthRoutes`, `internal/web/routes/auth.go`). `passwordlessExternal` is `network_access` of `external`/`public` with no password hash — a state only a hand-edited config file can produce, and it drives the Web UI's persistent security banner. |
| `POST` | `/api/auth/login` | 5 req / 60s | `{ password }` body, max 128 chars. Sets the session cookie and — when a database is wired — issues a persistent `moombox_client` token cookie, revoking any previous one from the same browser. Returns `{ success: true }`; the token itself is never in the body. |
| `POST` | `/api/auth/logout` | — | Invalidates the session and revokes the presented client token, then clears both cookies. |
| `POST` | `/api/auth/set-password` | 3 req / 60s | Sets or changes the password. Requires a valid session **or** a loopback/private-network origin. |
| `POST` | `/api/auth/remove-password` | 3 req / 60s | Removes password (disables auth). Same session-or-local gate. |

The two limiters are per-IP and separate from the shared API limiter: `rateLimitLoginPerMinute = 5` and `rateLimitPasswordPerMinute = 3` in `cmd/moombox/main.go`. A refused request answers `429` with `Retry-After` and `{ error, retryAfter }`.

### Client Tokens

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/client-tokens` | List all persistent client tokens. |
| `DELETE` | `/api/client-tokens/{id}` | Delete a specific client token. |

### Jobs

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/jobs` | List all active (non-archived) jobs. |
| `GET` | `/api/jobs/archived` | List archived jobs. |
| `GET` | `/api/jobs/{id}` | Get a single job by ID. |
| `GET` | `/api/jobs/{id}/video` | Stream the job's output video file. Supports HTTP Range requests for seeking. |
| `GET` | `/api/jobs/{id}/segments` | List segments for a multi-segment recording. |
| `GET` | `/api/jobs/{id}/segments/{index}/video` | Stream a specific segment's video file. Supports Range. |
| `GET` | `/api/jobs/{id}/chat` | Get the chat log file for a job. |
| `GET` | `/api/jobs/{id}/trims` | List trim clips created from this job. |
| `GET` | `/api/jobs/{id}/logs` | Get per-job log lines (worker-level logs specific to this job). |
| `POST` | `/api/jobs` | Create a new job. Rate limited. Body contains URL, format preferences, timestamps. |
| `POST` | `/api/jobs/{id}/cancel` | Cancel an active job. |
| `POST` | `/api/jobs/{id}/retry` | Retry a failed/cancelled job. |
| `POST` | `/api/jobs/{id}/open-folder` | Open the job's output folder in Windows Explorer. **Loopback only.** |
| `DELETE` | `/api/jobs/{id}` | Delete a job and optionally its files. |

### Watch Tracking

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/jobs/{id}/watch-state` | Get mutable player state (watched, resumePosition, chatOffset). Uncached — separate from the immutably-cached job endpoint. |
| `PUT` | `/api/jobs/{id}/resume-position` | Save playback resume position (lightweight, no WebSocket broadcast). |
| `POST` | `/api/jobs/{id}/resume-position` | Same as PUT — sendBeacon fallback (beacon only sends POST). |
| `POST` | `/api/jobs/{id}/watched` | Mark job as watched, clears resume position. Returns updated job. |
| `DELETE` | `/api/jobs/{id}/watched` | Mark job as unwatched, clears resume position. Returns updated job. |
| `POST` | `/api/jobs/batch/watched` | Batch mark jobs as watched. Body: `{ jobIds: [...] }`. |
| `DELETE` | `/api/jobs/batch/watched` | Batch mark jobs as unwatched. Body: `{ jobIds: [...] }`. |
| `PUT` | `/api/jobs/{id}/chat-offset` | Save chat timing offset. Body: `{ chatOffset: <number> }`. |
| `DELETE` | `/api/jobs/{id}/chat-offset` | Clear chat timing offset (reset to 0). |

### Formats

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/formats/{videoId}` | Fetch available formats for a YouTube/Twitch video or stream. Used by the Add Video flow. |

### Status

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/status` | Aggregate status. `StatusRoute` in `internal/web/routes/jobs.go`, wired in `cmd/moombox/routes_wiring.go`. |

**The complete key set.** Every key below `version` is conditional — on an atomic having been populated, or on the matching dependency being wired — so a consumer must treat each as optional rather than assume the full shape:

| Key | Shape | Source |
|-----|-------|--------|
| `status` | `"running"` | Constant |
| `uptime` | seconds since start | `deps.StartTime` |
| `timestamp` | RFC 3339 UTC | Request time |
| `memory` | `{ rss, heapUsed, heapTotal, external }` in MiB (`Sys`, `HeapAlloc`, `HeapSys`, `MSpanSys` / 1048576) plus `goroutines`, a count (`runtime.NumGoroutine`) | `runtime.ReadMemStats` (`internal/web/routes/jobs.go:1362-1368`) |
| `version` | string | `deps.Version` |
| `updateAvailable` | `{ version, tagName, releaseNotes, releaseNotesHtml, publishedAt }` | `SharedUpdateInfo` atomic; absent when no update is pending |
| `disk` | `{ free, total, usedPct, warnLevel }` | `SharedDiskStatus` atomic; absent until the first disk sample |
| `activePlatforms` | `{ youtube, twitch }` booleans | `config.GetActivePlatforms` |
| `cookieStatus` | `{ found, authenticated, verification, youtubeError }` | `routes.CookieStatusPayload(cookieRefresh.GetStatus())` |
| `twitchAuthStatus` | `{ found, authenticated, verification, twitchError }` | `routes.TwitchAuthStatusPayload(cookieRefresh.GetStatus())` |
| `autoCookieReloginRequired` | `{ youtube, twitch }` booleans | `AutoCookieService.ReloginStatus()` |
| `nextFeedCheck` / `nextDecapiCheck` / `nextTwitchCheck` | epoch ms | Monitor `GetNextCheckAt` |
| `channelHealth` | `{ youtube, twitch }` — per-channel last check, last error, consecutive failures | Feed and DECAPI health merged per YouTube channel (fresher last-check wins), plus Twitch |

The two cookie blocks come from `routes`' own projections rather than being rebuilt here, and that is load-bearing: three hand-written copies of the `cookieStatus` map existed across two packages, and a field added to two of them left this endpoint — the one the dashboard reads on every load and reconnect — quietly serving the old meaning. Their field contract is documented under §Cookies.

`autoCookieReloginRequired` calls `ReloginStatus()` and **not** `GetStatus()`, deliberately: the closure reads nothing but the relogin map, and `GetStatus`'s browser/registry detection scan would otherwise run on the dashboard's most frequent request for a field it never uses.

**There is no cookie-status WebSocket event.** The Web UI's cookie state arrives only in responses the page asked for: this endpoint, plus the two manual triggers, which return the same two blocks. `loadStatus()` is not polled on a timer — it runs on page init, on every WebSocket (re)connect, after a settings save, and after an interactive setup finishes or aborts (both the settings dialog's paths and the first-run wizard's). The two manual triggers do not re-fetch it at all: they assign `cookieStatus` / `twitchAuthStatus` / `autoCookieReloginRequired` straight off their own response bodies and call `updateStatusBar()`. Status and reason therefore always arrive together in one fetch, which is why the header badge may render `youtubeError` / `twitchError` in its tooltip while the push-driven TUI status bar may not (see §Status Bar).

### Backfill

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/backfill/rescan` | Force a feed-history backfill re-scan of every configured YouTube channel (same operation as the TUI `R B` chord). Debounced to one accepted run per 30s — a call inside the window returns 200 with `{"success":false,"debounced":true,"retryAfterMs":N}`. |

### Configuration

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/config` | Get current configuration. |
| `PUT` | `/api/config` | Update configuration. Triggers config save and may trigger restart. |
| `POST` | `/api/config/channels` | Add a monitored channel. |
| `DELETE` | `/api/config/channels/{id}` | Remove a monitored channel. |
| `POST` | `/api/resolve-channel` | Resolve a channel URL or name to a canonical channel identifier. |

### Cookies

`CookieRoutes` in `internal/web/routes/cookies.go`. The mechanisms behind these endpoints — the jar, the in-process `RefreshService`, and the `AutoCookieService`'s browser and profile-import paths — are specified in `data-and-storage.md §Cookies`; this section is what reaches the wire and what each UI does with it.

| Method | Path | Rate limit | Notes |
|--------|------|:----------:|-------|
| `POST` | `/api/cookies/recheck` | — | The in-process Go refresh + check (`RefreshService.CheckNow`, then `GetStatus`). Always 200. |
| `POST` | `/api/cookies/auto-refresh` | shared API | `AutoCookieService.RefreshCookiesDetailed` — headless browser when the gate allows one and `cookies.acquisition` is `auto`, otherwise an immediate browser-profile import. Discriminated error codes below. |
| `POST` | `/api/cookies/import` | shared API | Ingest an operator-supplied Netscape cookie file: `text/plain` body or multipart `cookies` part, 512 KiB cap. Merges into `cookies.txt`, reloads the jar, verifies, and returns `cookieSetupOutcome`'s exact key set. 400 empty/no part, 413 over the cap, 415 wrong content type, 422 for the three refusals and for an unreadable existing file, 409 for a failed write. **No GET.** |
| `POST` | `/api/cookies/auto-setup/start` | shared API | Begin interactive setup. Body `{ platform }`, defaulting to `"youtube"` on an absent or unparseable body. Returns `{ success: true }`. |
| `POST` | `/api/cookies/auto-setup/finish` | shared API | `FinishSetupDetailed` — extract, merge, write, then verify. |
| `POST` | `/api/cookies/auto-setup/cancel` | shared API | Cancel and close the setup browser. `{ success: true }`, or 404 when there is nothing to cancel. |
| `POST` | `/api/cookies/auto-setup/abandon` | shared API | The dashboard's unload beacon, and deliberately **not** `/cancel`: a user pressing Cancel consents to the setup window closing, a tab unloading does not. Releases the slot without killing the browser. Returns `{ success, released }`; 404 when there is no setup. |
| `POST` | `/api/auto-cookies/validate-browser-path` | shared API | Validates a user-supplied browser executable (spawns `--version`). 400 on an undecodable body; otherwise 200 with `{ valid: true }` or `{ valid: false, error }` — a rejected path is a *verdict*, not a transport failure, so it does not get an error status. A successful validation calls `InvalidateBrowserDetection()` so the next status poll sees the new browser instead of riding out the 60s detection TTL. |
| `GET` | `/api/cookies/auto-status` | — | `AutoCookieService.GetStatus()`. |

"shared API" is the single per-IP `apiRL` limiter (20 requests / 60s, `rateLimitAPIPerMinute`), shared with job creation. (The archive import at `/api/import` is NOT on it: it builds its own tighter 5/min limiter.) It wraps the endpoints that spawn or steer a browser — `AutoCookieService` already serialises them, but rate-limiting the request flow stops a caller burning CPU on the fast-fail path — and `POST /api/cookies/import`, which spawns nothing and is on it for a different reason: each request rewrites the credential file and makes up to four live auth round-trips.

`/auto-refresh`, `/import` and the four `/auto-setup/*` endpoints answer `503 auto-cookie service not configured` when no `AutoCookieService` is wired. `/recheck` needs only the `RefreshService`, `/auto-cookies/validate-browser-path` calls a package-level function and needs neither, and `/auto-status` answers a full zero-value body instead (below).

#### The two auth-status payloads

`CookieStatusPayload` and `TwitchAuthStatusPayload` (`internal/web/routes/cookies.go`) are the **only** two projections of `cookies.AuthStatus` onto the wire. `/api/status`, `/api/cookies/recheck` and `/api/cookies/auto-refresh` all render through them.

| Key | YouTube payload | Twitch payload | Meaning |
|-----|-----------------|----------------|---------|
| `found` | `HasYouTubeCookies` | `HasTwitchCookies` | Was this install ever **configured** for the platform — the loose predicate, not "is the cookie set complete". A Twitch session whose `auth-token` was pruned on expiry is a configured session with no credential, which is a different thing to say than "no cookies". |
| `authenticated` | `YouTubeAuthenticated` | `TwitchAuthenticated` | "Can we do authenticated work right now." Unchanged in meaning on purpose — it is the one key a pre-existing frontend reads — and **false on an inconclusive check**. |
| `verification` | `YouTubeVerification.String()` | `TwitchVerification.String()` | What the check **concluded**: `"ok"`, `"failed"` or `"unknown"`. Only `"failed"` is a conclusive negative and only it may be worded as one. |
| `youtubeError` / `twitchError` | `YouTubeError` | `TwitchError` | **Why** the check could not conclude. Empty whenever it did. |

`verification` is `cookies.RefreshVerdict` rendered through `String()` — never as an ordinal; the enum is an `int` and its field carries `json:"-"` for exactly that reason. `AuthStatus` has no `lastCheck`: the field existed, was written on every pass, was read by nothing, and was removed rather than wired — a timestamp from a pass that may have concluded nothing does not say "the credentials were valid as of this time".

The reason strings answer the half `verification` cannot carry. Without them the UI could say "could not check" and never say what stopped it, so an install behind a captive portal, one being rate-limited, and one behind an intercepting proxy all rendered identically and none named the thing to fix. They are **safe to render** because of a rule at the producers, not at this projection: every string that can reach them names a status code, a scheme+host, a header *name*, a transport error over a constant URL, or one of two static sentinels, and no branch interpolates a response body.

`verification`, `found` (Twitch) and the reason strings are all **additive**: an older frontend ignores them and behaves as before, and the current frontend branches *positively* on the strings, so against an older binary that omits them it degrades to the unqualified copy rather than to the hedged one.

#### Per-endpoint response bodies

**`POST /api/cookies/recheck`** — a status *snapshot*, not a claim that this request produced it. `CheckNow`'s "did a pass actually run" bool is deliberately ignored: a collision with the 30-minute ticker costs at most one snapshot of freshness, and every field is still a true statement about the credentials.

| Key | Value |
|-----|-------|
| `success` | `youtubeAuthenticated \|\| twitchAuthenticated` |
| `cookieStatus` | `CookieStatusPayload` |
| `twitchAuthStatus` | `TwitchAuthStatusPayload` |
| `autoCookieReloginRequired` | `ReloginStatus()`, or `{youtube:false, twitch:false}` when no auto-cookie service is wired — both platforms always present, so the frontend needs no missing-key fallback |
| `activePlatforms` | present when the callback is wired |

**`POST /api/cookies/auto-refresh`** on success adds the five `cookieRefreshOutcome` keys to the same status block. Three of them are independent facts and none can be derived from another:

| Key | Question it answers |
|-----|---------------------|
| `success` | `RefreshResult.AnyVerified()` — can we do authenticated work at all? The legacy alias for `verdict === "ok"`, kept because it is the only key a pre-existing caller reads. |
| `renewed` | Did **this** pass produce the credentials it verified? False means "could not confirm", never "the browser failed" — a working `cookies.txt` outlives a browser refresh that did nothing, because the independent 30-minute refresh keeps the session alive. |
| `verdict` | What the pass **concluded**: `"ok"`, `"failed"` or `"unknown"`. |
| `ran` | Did the pass do any work at all? This splits the two very different events inside `"unknown"`. |
| `mechanism` | WHICH cookie source ran: `"browser"`, `"profile-import"`, or `""` when the pass declined before it chose one. **Wording only** — the toast's subject, so an import stops rendering as a browser refresh. Additive: an older frontend ignores it, and a newer frontend against an older binary reads `undefined` and falls back to `cookies.acquisition`, the same value it used for the pre-flight toast. |

plus `cookieStatus`, `twitchAuthStatus`, `autoCookieReloginRequired` and `activePlatforms`. The status block is re-read after the browser pass, and it can lag: a refresh already in flight read the cookie file *before* this pass rewrote it. The refresh's own outcome comes from the five keys above and is unaffected.

Its error arms are discriminated so the frontend can both branch and show something actionable:

| Sentinel | Status | Body |
|----------|:------:|------|
| `ErrBrowserLadderBlocked` | 409 | `{ error, cause: "browser-ladder-blocked" }` |
| `ErrBrowserReadUnanswered` | 502 | `{ error, cause: "browser-read-unanswered" }` |
| `ErrNoBrowserFound` | 424 | Message **verbatim**, not a static "no supported browser installed": two states reach this sentinel on a refresh — no browser is installed, or one is and `auto_enabled` has switched headless runs off — and only the first can support that sentence. |
| `ErrProfileNotFound` | 404 | `browser profile not found — run setup first` |
| `ErrProfileDirUnreadable`, `ErrProfileNotADirectory`, `ErrProfileDirNotOptedIn`, `ErrCookieDBNotFound`, `ErrNoCookiesInProfile`, `ErrCookieDBUnreadable`, `ErrCookieFileUnreadable` | 422 | Message verbatim — these carry the only actionable detail the operator has, and there is no browser UI in a container. |
| `ErrCookieDBLocked` | 409 | Message verbatim |
| anything else | 500 | `cookie refresh failed` |

**`POST /api/cookies/auto-setup/finish`** returns `cookieSetupOutcome`: `{ success: true, authenticated, twitchAuthenticated, youtubeVerification, twitchVerification }`. The two facts per platform exist because they can disagree — `authenticated` is whether the setup **accepted** the sign-in (a login the user completed thirty seconds ago is accepted even when the site could not be reached to confirm it), and `*Verification` is what the check **concluded**. The pair `(accepted, "unknown")` is the state this exists for: the cookies are saved and in use, and Moombox could not reach the site to confirm them. It was computed long before it was rendered and survived only as a server log line, so a user whose network blipped during the check was told their login failed.

Its errors: `writeBrowserReadError` runs first, so the two browser-read sentinels answer 409/502 with a `cause` here too. Then `ErrNoSetupInProgress` → 404, `ErrSetupCancelled` and `ErrCookieDBLocked` → 409, `ErrCookieDBNotFound` / `ErrCookieDBUnreadable` / `ErrCookieFileUnreadable` → 422 verbatim, everything else → 500 `failed to finish setup`. An **empty** profile is not an error at all for either browser family: `FinishSetup` translates it to "no login detected" and returns a 200 the dialog renders inline.

`POST /api/cookies/auto-setup/start` keeps the static `no supported browser installed` for `ErrNoBrowserFound`, and correctly — `StartSetup` is never gated, so there the sentinel means exactly one thing. `ErrSetupInProgress` / `ErrRefreshInProgress` → 409; `ErrServiceStopped` → 503, because that one never clears.

`cause` is a **short stable token**, never the sentinel's message: prose gets reworded, and a frontend branch keyed on prose breaks silently the first time it is. The wording still rides along as `error`, which is the half a human reads. The two tokens must stay distinct because the operator's next move differs — a blocked ladder is a condition on this machine to change (something is holding or intercepting the debugging port), an unanswered read is the browser side having produced nothing at all. **Nothing branches on `cause` today** — the dashboard renders `error` (directly on `/auto-refresh`, through `serverErrorMessage` on `/auto-setup/finish`) and the TUI never sees these responses at all. It is emitted for the machine reader that does not exist yet, and is pinned by `internal/web/routes/cookies_browserread_test.go`.

**`GET /api/cookies/auto-status`** marshals `cookies.AutoCookieStatus`: `setupInProgress`, `browser`, `availableBrowsers`, `configuredBrowserPath` (`omitempty`), `configuredBrowserType` (`omitempty`), `lastRefresh`, `lastError`, `needsManualRelogin`. There is deliberately no `configured` flag — one existed, computed as `profileDir != ""`, could never be false, and was read by nobody.

When no auto-cookie service is wired the handler answers a hand-built object that must match the real one key for key, or this branch teaches the frontend a field the real service never sends. `availableBrowsers` is `[]` and **not** `null`, because `AvailableBrowsers` has no `omitempty` and `DetectBrowsers` never returns a nil slice — the frontend iterates it unconditionally. `configuredBrowserPath` / `configuredBrowserType` are *omitted* here for the mirror-image reason: both carry `omitempty`, so a zero-value `AutoCookieStatus` omits them too. `needsManualRelogin` always carries both supported platforms.

#### What the Web UI renders

| Surface | Reads | Behavior |
|---------|-------|----------|
| Header platform badges | `/api/status`'s `cookieStatus` / `twitchAuthStatus` / `autoCookieReloginRequired`, plus the in-memory job list | `cookieIndicatorState` in `web/public/modules/utils.js`, called from `updateStatusBar` in `app.js` |
| Header warnings (`YT: Re-login` / `TW: Re-login`) | `autoCookieReloginRequired`, then `/api/cookies/auto-status` on the click | Text on desktop, a collapsed `exclamation-triangle` icon on mobile. Clicking asks `reloginPromptTarget` (`web/public/modules/utils.js`) which remedy is actually available to THIS viewer: interactive setup only for a loopback viewer of a host that has a browser, because the wizard opens a login window ON THE HOST and nothing server-side stops a remote client from triggering that (the loopback gate covers `/api/setup/complete`, not the cookie setup trio) — and the cookies panel's import box for everyone else, including every LAN or tunnelled client, every browserless host, and any install whose status could not be read (that panel holds both controls, so the fallback costs a local operator one click). **Known limit:** an SSH local port-forward makes the browser's `window.location.hostname` the local end of the tunnel while the server sees a loopback `RemoteAddr`, so `reloginPromptTarget` answers "wizard" and the server would let the wizard run — a viewer tunnelling in is offered the wizard and the window opens on the host, by construction of the two checks (`reloginPromptTarget`, `web/public/modules/utils.js`; `isLoopback`, `internal/web/middleware.go`), not observed in the field; the import box beside it is the working path |
| Settings → paste/upload cookies (`cookie-import-text`, `cookie-import-file`, `btn-cookie-import`) | `POST /api/cookies/import` | The textarea wins when it has content; otherwise the chosen file goes up as multipart with no explicit `Content-Type` so the browser sets its own boundary. Accepted platforms toast through `cookieSetupAcceptedToast`, a rejection renders inline through `cookieSetupRejectedMessage`, and a non-200 renders the server's own sentence through `serverErrorMessage`. Both controls are cleared on success |
| Refresh-cookies button | — | Plain click → `recheckCookies()`; **shift+click** → `autoCookieRefresh()` |
| Settings → "Refresh cookies from browser profile" button (`btn-import-browser-profile`) | — | Calls `app.autoCookieRefresh()` — the same method and endpoint as shift+click, existing because a modifier key does not exist on a phone |
| Settings auto-cookie panel | `/api/cookies/auto-status` | Browser selector, `Last refresh: …`, and a `Last cookie error: …` line shown only when `lastError` is non-empty |
| Setup dialog | `/auto-setup/finish` | `cookieSetupAcceptedToast` per accepted platform, `cookieSetupRejectedMessage` inline when neither was accepted |

`cookieIndicatorState` decides one platform's badge in a fixed order, and the order is the contract:

1. **re-login required** → red, `<Platform>: Re-login required`. Not gated on `auto_enabled` — "a human must sign in again" is exactly as true, and exactly as actionable, for an install that maintains `cookies.txt` by hand. Do not reintroduce the gate here or at the call site; the TUI has never had one.
2. **a job parked in `COOKIES?` for this platform** → red, `<Platform>: A download stopped for want of usable credentials`. `parkedCookiePlatforms` is the Web half of the TUI's `parkedCookieJobs`, ported deliberately rather than re-derived, with the same per-platform attribution, the same absent-platform-counts-as-YouTube rule, and the same absence of a `ParkReason` filter.
3. `authenticated` → green, `Authenticated`.
4. `!found` → the platform's absent state, and the asymmetry mirrors the TUI's yellow/dim split: `YouTube: No cookies` is a warning because almost everything Moombox does with YouTube wants them, `Twitch: Anonymous` is the neutral off dot because that is the ordinary mode.
5. `verification === "unknown"` → warning, `Cookies saved — Moombox could not establish whether they work`, with `(reason)` appended from `youtubeError` / `twitchError` when present. The reason is now appended to the **conclusive-refusal** arm too (`Not authenticated (…)`), because TWO producers write `failed` with a reason: the unsignable-jar sentinel (`ErrAuthCheckNotAttempted`, whose cause the old gate silently dropped) on YouTube, and `NoteTwitchAuthLoss` on Twitch, whose reason is one of five fixed sentences — the four chat-downgrade routes plus the playback-token route — naming which broke; that is the only thing distinguishing a missing `login` cookie from a login Twitch refused. `"ok"` still shows nothing, by construction rather than by convention: `verdictFromCheck` returns OK only for a nil error and the reason string IS that error, so the two cannot co-occur. The gate is therefore on the STRING, not the verdict.
6. otherwise → red, `Not authenticated`.

`authenticated` is tested **before** `found`, and the `"unknown"` comparison is positive rather than `!== "ok"`. Both are the additive contract in the other direction: an older binary sends no `verification` and no Twitch `found`, and either inversion would render a healthy session as broken.

The badge is repainted from four job events — `job_update`, `jobs_update`, `initial_state`, `job_deleted` — through `_syncParkedBadge`, which is **change-gated**: the scan over jobs already in memory runs every time (it is cheap and stops at the second platform) and only the DOM write is conditioned, so a 60 Hz progress tick never repaints. `job_deleted` matters because deleting the last parked job is the one gesture that *clears* the escalation. `updateStatusBar` re-computes `parkedCookiePlatforms` fresh rather than reading the memoised value, so the four pre-existing triggers (config load, status load, manual recheck, manual browser refresh) paint the same badge.

The **recheck toast** is worded by `cookieRecheckToast` from the two `verification` fields, filtered to the active platforms — deliberately not from `success`, which is `youtubeAuthenticated || twitchAuthenticated` and therefore false for a check that never reached the site. Its `message` is reproduced character for character from `cookies.RecheckReport` in Go and pinned by a test that runs both; only the Shoelace `variant` is web-only, and it ranks danger (a conclusive failure) over warning (nothing established) over success.

The **browser-refresh toast** has five branches over `ran`, `verdict` and `renewed`, and both un-concluded arms stop short of asserting failure: `!success && ran === false` → neutral, using `cookies.RefreshDeclinedCauses` verbatim; `!success && verdict === "failed"` → danger; `!success` → warning ("ran but could not establish"); `renewed === false` → warning ("cookies still work — but this pass could not confirm the browser refreshed them"); otherwise success. A 404 or 424 is **not** an error here: it is the bottom rung of the ladder, and the dashboard falls through to `recheckCookies()` after toasting `No browser profile found, running a normal cookie refresh instead...`. It branches on the status code, not on the message.

#### What the TUI renders

The TUI's cookie chords do **not** go through these REST endpoints. `OnRecheckCookies`, `OnAutoCookieLastError` and `OnForceRefreshCookies` — and, behind `R L`, the wizard's `OnStartAutoCookie` / `OnFinishAutoCookie` / `OnCancelAutoCookie` (all bound in `cmd/moombox/tui_wiring.go`) — call `RefreshService` and `AutoCookieService` in-process, so both surfaces exercise the same services but not the same handlers — which is why every shared sentence here is held by an executed test rather than by the transport. The TUI also has no paste/upload affordance for `POST /api/cookies/import`: Arc 11 skipped TUI parity deliberately (spec R7 left it optional) — a multi-kilobyte credential paste into a terminal is a worse path than the file copy a TUI user already has — so the dashboard's Settings → Cookies panel is the only paste/upload surface.

**`R C` — Recheck Cookies.** Never gated by anything. `recheckCookiesCmd` (`internal/tui/app_actions.go`) collects four values — the two verdicts and the two reason strings — plus `AutoCookieStatus.LastError` from a *different service*, and `cookieRecheckFeedback` (`internal/tui/app_update.go`) composes one line:

```
Cookies: YouTube OK, Twitch — could not establish (Twitch: <reason>) | Last cookie error: <lastError>
```

The verdict clause is `cookies.RecheckReport`, shared with the Web toast. A reason is appended for any *active* platform that has one, whatever its verdict — `RefreshUnknown`, and now `RefreshFailed` as well, which is how two producers reach the operator: the YouTube-side unsignable-jar sentinel, and the Twitch mark's five fixed sentences. An `RefreshOK` platform never has one to append. The `LastError` clause is ungated by any verdict and goes last, so the width clamp eats it before it eats the verdicts: it belongs to the auto-cookie service, not to the check this line reports, and it can be non-empty while both verdicts are OK — a browser refresh that has been failing for days behind a `cookies.txt` the 30-minute session refresh is still renewing. This is the TUI's only surface for that fact; it has no auto-cookie status panel, where the Web UI has the settings panel.

**Severity is stated by the composer, never inferred from the finished sentence.** `cookieRecheckFeedback` returns a `feedbackSeverity` beside the line and `feedbackColor` obeys a stated severity outright. The line is clamped to the pane width by `fitFeedback` *before* the colorizer sees it, so at 40 columns `"… | Last cookie err…"` loses the marker the warning branch matched on and a line announcing a recorded failure rendered **green**; at 30 columns `"Cookies: YouTube not authen…"` lost `not authenticated` and a conclusive refusal rendered green too. Each contributing fact raises the severity independently and none can lower it: `RefreshFailed` → error, `RefreshUnknown` → warning, no configured platforms → warning, a non-empty `LastError` → at least warning (never more — what was recorded is a fact about a *previous* pass).

`not authenticated` is **red** on both `R C` and `R F`. Red is the actionable end — the remedy is to re-export credentials — and yellow is reserved for "we could not check", which asks for nothing. A mixed line, one platform refused and the other unreachable, is red: the conclusive half is the half to act on, which is the same precedence the badge and the dashboard toast apply.

**`R F` — Force Cookie Refresh.** Wired unconditionally; do not put an `auto_enabled` gate back, in either shape. A nil `OnForceRefreshCookies` does not make the chord inert, it *deletes* it — `dispatchAction`, `buildMenuItems` and the help overlay all test the field — so on an install with the flag off, an operator told their cookies were dead had no key to press and no entry naming one. It is a three-rung ladder; the rungs are chosen inside `RefreshCookiesDetailed` (see `data-and-storage.md §Auto-Cookie Service`) and the TUI only renders the outcome:

| Outcome | Line |
|---------|------|
| `cookies.IsNoBrowserProfile(err)` (rung 3) | `No browser profile found, running R C instead...` — then it dispatches `R C`'s own command, so the sentence leads a real refresh and is replaced by that refresh's report a moment later |
| any other error | `Browser cookie refresh failed: <err>` |
| `!Ran` | `Browser cookie refresh declined to run (<RefreshDeclinedCauses>) — nothing was learned about these cookies` |
| `Overall() == RefreshFailed` | `Browser cookie refresh ran and auth verification failed` |
| `Overall() == RefreshUnknown` | `Browser cookie refresh ran but could not establish whether these cookies work` |
| `!Renewed` | `Cookies still work, but this pass could not confirm the browser refreshed them` |
| otherwise | `Browser cookie refresh successful` |

The rung-3 sentence and its Web twin (`No browser profile found, running a normal cookie refresh instead...`) **diverge by design**: each surface names its own affordance for the in-process refresh, and a dashboard user has no `R C` to press. Both are pinned exactly, and their difference asserted, by `TestRungThreeSentencesDivergeByDesign`.

**`R L` — Cookie Login.** The TUI's entrance to the interactive browser login, and the answer to a `Re-login` badge. It opens `SetupWizardModel` at its cookie step in cookie-only mode (`OpenCookieLogin`, `internal/tui/setup_wizard.go`) — the *same* state machine the first-run wizard drives, not a second one: `OnStartAutoCookie` → `AutoCookieService.StartSetup`, the 300 s countdown (`cookieSetupCountdownSeconds`), `Enter` → `OnFinishAutoCookie` → `FinishSetupDetailed` under `cmd/moombox`'s 60 s ctx, and the same four-arm verdict rendering (`case setupCookieFinishMsg`, `internal/tui/app_update.go`) that distinguishes *accepted* from *verified* — with one caveat the first-run flow shares: the error and rejected arms render inline in the overlay (`errorMsg`), but the accepted arm is written to the App feedback line (`setFeedback`, 3 s), which the overlay covers for the whole of that life — `View` (`internal/tui/app_layout.go`) returns the wizard alone while it is visible — so what the operator sees is the ✓ on the platform row, and the accepted-but-unverified distinction reaches a terminal operator only through the dashboard today. What cookie-only mode changes is the two exits: `Esc` at the picker and the third list row (`Close`, not `Skip / Next`) close the overlay rather than walking into the first-run channel editor, whose `Tab` would rewrite `config.toml` on a configured install. **Every exit funnels through `closeCookieLogin`, which cancels an in-flight setup first**, so an abandoned overlay releases the acquisition slot instead of leaving it for the server-side reap; `Esc` while the browser is open cancels and returns to the picker rather than closing. The chord is gated on the callback exactly as `R F` is — a nil callback deletes a chord rather than making it inert — and `cmd/moombox` binds it unconditionally, so `R L` exists with `cookies.auto_enabled` off: `StartSetup` is acquisition and is never gated. With no interactive-setup callback at all the chord does not exist: `R L` reports `Invalid Chord: R L` like any unregistered pair and is absent from the menu and from help (the `dispatchAction` nil-guard behind it is defensive and unreachable from the keyboard); `StartSetup`'s own refusals — stopped service, a setup or refresh already running, no supported browser — arrive on the operator's `Enter` and render inline in the wizard, where the dashboard puts them too.

#### Restart-required cookie settings

Three cookie keys are labelled restart-required in **both** settings UIs — `cookie_file`, `auto_enabled`, `browser_profile_dir`. The Web UI inserts a `Restart` badge after the named element (`RESTART_REQUIRED_FIELDS` in `web/public/modules/settings.js`) and offers a restart on save; the TUI colours the change marker yellow instead of green for these keys (`restartRequiredKeys` in `internal/tui/settings.go`, rendered in `settings_view.go`). The two lists are pinned against each other by `TestRestartRequiredListsAgree`. What `auto_enabled` does **not** need a restart for is the manual triggers: `R F` and the dashboard's shift+click read it live. See `data-and-storage.md §[cookies]` for why the three are restart-required at all.

#### Facts these surfaces deliberately do not carry

- **Cookie expiry has no UI field**, and the jar's expiry accessors reach the log and nothing else. `ExpiredAuthCookiesFor` has exactly one production consumer — the `Cookies loaded` startup log line in `cmd/moombox/services.go`, emitted once per boot when a cookie file is configured and loads, which prints `expiredYouTubeAuth` and `expiredTwitchAuth`, both platforms always, neither implied by the other's silence. `AuthCookieHorizonFor` and `TwitchLoginExpiry` reach the LOG and nothing else: `youtubeAuthHorizon` / `twitchAuthHorizon` / `twitchLoginExpiry` ride the startup `Cookies loaded` line and the `cookie refresh succeeded` line (`data-and-storage.md §Cookie Jar`), as ISO-8601 UTC or `none`. **No badge and no payload key carries a horizon** — nothing on either UI reads any of the three accessors, and that is the deliberate half. An expired Twitch `auth-token` still has no UI warning: `RefreshService` rotates YouTube in-process but only *checks* Twitch, and an expired token downgrades chat capture to anonymous instead of failing. The Twitch `login` row has one more: when the merge prunes it on expiry while the `auth-token` survives, the refresh logs a single Warn naming the degradation (anonymous chat, no subscriber-only messages, no badges) and no value.
- **The chat-downgrade notification is not a UI surface**, but it is how one credential failure reaches an operator who is looking at neither dashboard — previously a state neither badge could show, because the download itself is healthy. The same report now also marks the PLATFORM (`NoteTwitchAuthLoss`, resolved through `twitchChatDowngradeCallback` in `internal/worker/stream_processor_twitch.go`), so both badges go red on the verdict flip and the two per-request surfaces (the Web tooltip, the TUI's `R C`) name the route; the notification remains the only thing that names the JOB. When a job that **had** Twitch credentials falls back to the anonymous IRC login, the worker sends exactly one notification per CREDENTIAL PAIR (once per job until the cookie file's Twitch pair changes, since `Reauthenticate` resets the report latch) — title `Twitch chat is anonymous for <channel>`, `TypeWarning`, event `"auth"` so it filters alongside the worker's and monitor's other credential alerts. The reason comes from a closed FIVE-value vocabulary in `internal/twitch/chat.go`, four of which reach this notice (the fifth, `playback-token-anonymous`, marks the platform and logs but never notifies), with no format verb to interpolate a token, a login or a chat line into, and `twitchChatDowngradeReason` in `internal/worker/stream_processor_twitch.go` turns it into the operator's sentence. The description names the **next-capture** consequence, not just the chat one: this download keeps the entitlements its playback token was issued, but the next starts anonymous — ad-break gaps in the archive, and outright failure on subscriber-only content. A notice that said "chat only" would read as "no rush". A job with chat recording disabled gets no such NOTIFICATION — its detector is the playback token (`Service.GetHLSMasterPlaylist` / `StreamProcessor.noteAnonymousPlayback`, see `platform-services.md` § IRC Chat), which marks without notifying — and neither does a cookieless install: the callback is only wired when a live chat downloader is created, and it fires only when the job had credentials to lose.

### Trims

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/jobs/{id}/trims` | Create a trim clip. Body contains start/end seconds. |
| `DELETE` | `/api/jobs/{id}/trims/{trimId}` | Delete a trim clip. |

### Files

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/files/orphaned` | List files in the output directory with no corresponding job. |
| `DELETE` | `/api/files/orphaned` | Delete specified orphaned files. |

### History

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/history/orphaned` | List processing-history rows with no matching job (job deleted, or the video was skipped and never jobbed). While the row remains, the monitor treats the video as already-processed and won't re-discover it. Keyed by job ID, so the match is against `jobs.id`. |
| `DELETE` | `/api/history/orphaned` | Remove the given history video IDs (JSON body `{"videoIds":[...]}`), unblocking re-discovery. |

### Updates

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/update/status` | Get current update status (available version, if any). |
| `GET` | `/api/update/release-notes` | Fetch release notes for a version. Query param `version=X.Y.Z`; defaults to current version. Returns sanitized HTML rendered from GitHub release body via goldmark + bluemonday (download-link section stripped). Used by the Web UI "View Release Notes" button. |
| `POST` | `/api/update/check` | Manually check for updates. |
| `POST` | `/api/update/apply` | Download and apply an available update. Triggers restart. |
| `POST` | `/api/update/verify` | Verify the Ed25519 signature of the current binary. |
| `POST` | `/api/update/dismiss` | Dismiss the update notification. |

### FFmpeg

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/ffmpeg/check` | Check if FFmpeg is on PATH and return version info. |
| `GET` | `/api/ffmpeg/install-suggestion` | Returns the distro-appropriate package manager command for FFmpeg installation (e.g., `apt install ffmpeg`, `dnf install ffmpeg`, `pacman -S ffmpeg`). Linux only; returns empty on Windows. |
| `POST` | `/api/ffmpeg/check` | Re-check FFmpeg availability. Rate limited. |
| `GET` | `/api/ffmpeg/install-options` | Get available FFmpeg installation options (download sources). |
| `POST` | `/api/ffmpeg/install` | Begin FFmpeg download/installation. Rate limited. |
| `POST` | `/api/ffmpeg/install/confirm` | Confirm FFmpeg installation to a specific location. Rate limited. |
| `POST` | `/api/ffmpeg/install/reject` | Reject/cancel FFmpeg installation. Rate limited. |

### Statistics

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/stats` | Aggregate statistics: job counts by status, total sizes, durations, disk usage. |

### yt-dlp Plugin

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/ytdlp-plugin/status` | Check if the yt-dlp PO token plugin is installed. |
| `POST` | `/api/ytdlp-plugin/install` | Install the yt-dlp plugin to the user's yt-dlp config directory. |

### Setup

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/setup/status` | Check if first-run setup has been completed. |
| `POST` | `/api/setup/complete` | Mark setup as complete. |

### Logs

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/logs` | Get recent log lines (from the in-memory ring buffer). |

### Import

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/import` | Upload a zip archive to import as a job. **500 MB body limit** (overrides the default 1 MB). Rate limited. Headers: `X-Import-Title`, `X-Import-Channel` for overrides. |

### PO Token (yt-dlp Plugin Compatibility)

These routes provide PO token generation for external yt-dlp instances. They use bare paths (no `/api/` prefix) for compatibility with the yt-dlp plugin protocol.

| Method | Path | Access | Notes |
|--------|------|--------|-------|
| `POST` | `/get_pot` | Loopback only, rate limited | Generate a PO token for a video. |
| `POST` | `/invalidate_caches` | Loopback only | Invalidate all PO token caches. |
| `POST` | `/invalidate_it` | Loopback only | Invalidate a specific identity token. |
| `GET` | `/ping` | Public | Health check for yt-dlp plugin discovery. |
| `GET` | `/minter_cache` | Public | List cached minter keys (diagnostic). |

### System

| Method | Path | Access | Notes |
|--------|------|--------|-------|
| `POST` | `/api/restart` | Per `network_access` + auth | Trigger application restart (exit code 42). Gated only by the standard stack (IPGate + CSRF + AuthMiddleware), so any connection the operator allows — local, LAN, or authenticated external — may restart. Unlike `/api/update/apply`, it is intentionally **not** loopback-restricted (it relaunches the same binary, not a new one). |

---

## Shared Patterns Between Web UI and TUI

### Real-Time State Synchronization

Both UIs receive the same state updates, but through different transport mechanisms:

- **Web UI:** WebSocket messages (`job_update`, `jobs_update`, `log`, `check_timers`).
- **TUI:** Go channels (`JobUpdateMsg`, `JobsUpdateMsg`, `LogBatchMsg`, `CheckTimersMsg`, `CookieStatusMsg`, `DiskStatusMsg`, `UpdateStatusMsg`).

The data is identical. The TUI receives updates from database subscriber callbacks (the same callbacks that trigger WebSocket broadcasts), so both UIs are always in sync.

### Status Bar Consistency

Both UIs display the same status information in a persistent status bar / footer area:

| Element | Web UI | TUI |
|---------|--------|-----|
| Connection status | WebSocket connected/disconnected indicator | Backend reachability indicator |
| Disk usage | Output drive usage with warning/critical thresholds | Same, with color-coded thresholds |
| Monitor timers | Next check times for Feed, DECAPI, Twitch | Same, countdown format |
| Cookie status | Per-platform badge, `cookieIndicatorState` | Per-platform indicator, `renderCookieStatus` |
| Re-login required | `YT: Re-login` / `TW: Re-login` in the warnings area, clickable to start setup | Folded into the platform indicator as `YT: Re-login` / `YT!`, and at `tierFull` followed by `(R L)` — the chord that opens the same interactive setup the dashboard's click does |
| Update indicator | New version badge | New version indicator |

**Cookie parity, and where it stops.** The two indicators agree on the facts that matter and are held to that by shared code and by tests, not by convention:

| Property | Status |
|----------|--------|
| Escalation order | **Same.** Both rank re-login first and a parked `COOKIES?` job second, above every check-derived state, for the same stated reason: a park is evidence from a real download attempt and outranks a check that merely asked. Below that rank the check-derived states are mutually exclusive by construction, so the two branch orders are not observably different. |
| Parked-job attribution | **Same.** `parkedCookiePlatforms` (`web/public/modules/utils.js`) is a deliberate port of `parkedCookieJobs` (`internal/tui/status_bar.go`), down to the absent-platform rule and the absence of a `ParkReason` filter. The Web side was knowingly divergent before that port — the TUI reflected parked jobs and the Web did not. |
| Re-login gating | **Same, and both ungated.** Neither surface conditions the re-login prompt on `cookies.auto_enabled`. The dashboard used to, in all three places it surfaces the state, and the TUI never has; removing the Web gate is what brought them into step. A manual-cookie install is the audience least able to discover the state any other way. |
| Manual recheck wording | **Same sentence.** `cookies.RecheckReport` is the Go authority and the Web copy is reproduced character for character, pinned by a test that executes both. |
| Manual refresh gesture | **Same gesture, different affordances.** The TUI's `R F` and the dashboard's shift+click (and the Settings page's "Refresh cookies from browser profile" button, which calls the same method) run the same `RefreshCookiesDetailed` ladder — the TUI in-process, the dashboard over `POST /api/cookies/auto-refresh`. Their rung-3 sentences differ **by design** and are pinned apart by `TestRungThreeSentencesDivergeByDesign`. |
| Interactive login | **Same operation, different affordances — and one question only the dashboard has to ask.** Both surfaces drive `StartSetup` / `FinishSetupDetailed` / `CancelSetup`: the TUI in-process through the wizard's three callbacks, the dashboard over the `/auto-setup/*` trio. The dashboard must decide whether *this viewer* may open a browser window on the host (`reloginPromptTarget`, and the import box for everyone else); a TUI session is the host, so `R L` has nothing to route. |
| Check-reason rendering | **Divergent, by contract, and the divergence is PUSH vs PER-REQUEST rather than web vs TUI.** Both per-request surfaces render the reason whenever there is one — the Web badge's tooltip off `/api/status`, and the TUI's `R C` result line — for inconclusive AND conclusively-refused verdicts alike (the Twitch mark writes `failed` with a reason too). Neither push-driven surface renders it: the TUI status bar is fed by `OnAuthChange`, whose `authStatusChanged` gate excludes the two strings, and the web has no cookie-status WebSocket event at all, so its badge is only ever painted from a fetch it asked for. The TUI bar surfaces the reason on the next `R C` instead. |
| `AutoCookieStatus.LastError` | **Divergent surfaces.** The Web UI has a persistent `Last cookie error:` line in the settings auto-cookie panel; the TUI has no such panel and appends the same fact to the `R C` result line. |

A divergence stated here is a specification. A divergence omitted is a bug report waiting.

### First-Run Setup Wizard

Both UIs implement a setup wizard that runs on first launch (before `setup_complete` is set in config):

1. **Basic configuration** — output directory, port, etc.
2. **FFmpeg check** — validate FFmpeg on PATH, offer installation if missing.
3. **yt-dlp plugin** — offer to install the PO token plugin for external yt-dlp usage.
4. **Cookie capture** — guide the user through providing browser cookies for authenticated access.

The Web UI implementation is in `modules/setup.js`. The TUI implementation is in `setup_wizard.go` (using `huh` forms).

### Job Lifecycle Visualization

Job status is visualized consistently in both UIs using the same conceptual model:

| Status | Meaning | Visual Treatment |
|--------|---------|------------------|
| `Upcoming` | Stream is scheduled but not yet live | Neutral/gray, shows scheduled time |
| `Live` | Stream detected, waiting to start download | Highlighted, pulsing or animated |
| `Downloading` | Actively downloading segments | Active color (blue/cyan), progress indicator |
| `Muxing` | FFmpeg muxing in progress | Processing indicator |
| `Finished` | Complete, output file available | Success color (green) |
| `Error` | Failed at some stage | Error color (red), error message displayed |
| `Cancelled` | Manually cancelled by user | Dimmed/muted |
| `COOKIES?` | Authentication required but cookies are missing or expired | Warning color (yellow/orange), actionable prompt |

The specific colors and icons differ between the Web UI (CSS classes, Shoelace icons) and TUI (lipgloss styles, Unicode symbols), but the status-to-visual-treatment mapping is consistent.

### Module/Overlay Feature Mapping

Every major feature exists in both UIs:

| Feature | Web UI Module | TUI Overlay |
|---------|---------------|-------------|
| Add video/stream | `app.js` (inline dialog) | `add_video.go` |
| Video playback | `modules/player.js` | N/A (opens in browser via `O W`) |
| Settings | `modules/settings.js` | `settings.go` |
| First-run setup | `modules/setup.js` | `setup_wizard.go` |
| Trim creation | `modules/trimmer.js` | `trim_dialog.go` |
| Statistics | `modules/stats.js` | N/A (data available via API) |
| Zip import | `modules/imports.js` | `import_dialog.go` |
| Orphaned files | `app.js` (inline) | `files_dialog.go` |
| Client tokens | `app.js` (inline) | `client_tokens_dialog.go` |

**Note on video playback:** The TUI cannot play video inline (it is a terminal). The `O W` chord opens the Web UI in the default browser, where the user can access the player. This is the intended design — video playback is a Web UI strength, and the TUI defers to it rather than attempting a degraded experience.

---

## HTTP Server Middleware Stack

The middleware is applied in this exact order (defined in `server.go`). Order matters — each middleware wraps the next, so the first listed is the outermost:

1. **RecoveryMiddleware** — Catches panics in any handler. Logs the stack trace. Returns 500 to the client. Prevents a single request from crashing the server.
2. **CORSMiddleware** — Handles Cross-Origin Resource Sharing headers based on configuration.
3. **SecurityHeaders** — Sets CSP, X-Content-Type-Options, X-Frame-Options, and other security headers on every response.
4. **CSRFMiddleware** — Validates Origin/Referer headers on state-changing requests (POST, PUT, DELETE). Requests with a valid `X-Internal-Token` header bypass this check (TUI path).
5. **IPGateMiddleware** — IP-based access control. Enforces network access level (loopback only, LAN only, or public).
6. **MaxBodySize** — Default 1 MB body limit. Individual routes (e.g., import at 500 MB) can override.
7. **CompressionMiddleware** — Gzip compression for responses. Skips WebSocket upgrades and responses that should not be compressed (video streams, already-compressed content).

Some routes apply additional per-route middleware:
- **LoopbackOnly** — Restricts access to loopback addresses (127.0.0.1, ::1). Used for `open-folder` and the PO token routes. (`/api/restart` no longer uses it — it relies on the standard IPGate + CSRF + Auth stack so authorized LAN/external clients can also restart.)
- **Rate limiters** — Per-endpoint rate limiting (e.g., 5 login attempts per 60 seconds). Applied via `r.With(rl.Middleware)`.

---

## Cross-References

### Related Spec Documents

- **[architecture.md](architecture.md)** — WebSocket hub details, service initialization order, data flow from monitors through download to UI.
- **[security.md](security.md)** — Auth middleware, CSRF mechanism, internal token, IP access control, session management, client tokens.
- **[design-philosophy.md](design-philosophy.md)** — Dual UI philosophy, Charm ecosystem rule, resource efficiency requirements for UI throttling.
- **[data-and-storage.md](data-and-storage.md)** — Database schema, job status lifecycle, pub/sub system that feeds UI updates.

### Source Directories

- **`internal/web/`** — HTTP server, middleware, WebSocket hub, auth service.
- **`internal/web/routes/`** — All REST API route handlers, organized by domain (auth, jobs, cookies, update, ffmpeg, stats, files, ytdlp).
- **`internal/tui/`** — All TUI source files (21 files, ~12,900 lines).
- **`web/public/`** — All static web assets (HTML, JS, CSS).
- **`web/embed.go`** — `go:embed` directive for static assets.
