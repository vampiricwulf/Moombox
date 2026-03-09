# User Interfaces

## Scope

This document specifies the two user interfaces provided by Moombox — a Web UI (vanilla JavaScript SPA served by the embedded HTTP server) and a TUI (terminal UI built on the Charmbracelet ecosystem). It covers their architectures, component structures, shared patterns, the WebSocket real-time sync protocol, the complete REST API surface, and the TUI chord system. It is the authoritative reference for any work that touches how users interact with Moombox.

## Rules and Constraints

- **Both UIs are first-class.** Neither the Web UI nor the TUI is a secondary or degraded experience. Feature parity is required. Every user-facing capability must exist in both.
- **Parity respects platform strengths.** The TUI emphasizes real-time feedback, keyboard-driven workflows, and dense information display. The Web UI emphasizes rich media playback, dashboards, and accessibility from any device on the network. The same operation may have different UX in each UI, but the capability itself must be present in both.
- **The TUI uses the Charm ecosystem.** Specifically: `bubbletea` (core Elm architecture), `bubbles` (pre-built components), `huh` (form framework), and `lipgloss` (styling/layout). Before building any custom TUI component, always check [Charm's repositories](https://github.com/charmbracelet) for an existing solution. Prefer extending Charm's building blocks over rolling custom implementations. This applies to lists, text inputs, forms, file pickers, tables, progress bars, viewports, spinners, and any other UI primitive.
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
| `web/public/app.js` | ~2,560 | Main SPA module. Job list, log viewer, status bar, WebSocket connection management, settings integration, theme switching, search/filter. |
| `web/public/modules/player.js` | ~900 | Video player with niconico-style scrolling chat overlay. Handles multi-segment seeking (segments are separate video files that the player stitches together into a seamless timeline). |
| `web/public/modules/setup.js` | ~785 | First-run setup wizard. Walks the user through initial configuration, FFmpeg installation, yt-dlp plugin setup, and cookie capture. |
| `web/public/modules/settings.js` | ~1,600 | Settings dialog. Covers full config editing, channel management, cookie management, integration settings. |
| `web/public/modules/trimmer.js` | ~510 | Trim clip creation UI. Lets the user define start/end timestamps on a finished recording and create a trimmed clip. |
| `web/public/modules/stats.js` | ~160 | Statistics dashboard. Displays job counts, sizes, durations, and other aggregate metrics. |
| `web/public/modules/imports.js` | ~210 | Zip archive import. Upload a zip file containing video/chat/metadata to create a job from external content. |
| `web/public/modules/utils.js` | ~70 | Shared formatting helpers (durations, file sizes, dates, etc.). |
| `web/public/moombox.css` | ~2,090 | All styles. Includes desktop layout, mobile responsive breakpoints, dark/light theme variables, and component-specific styles. |

### Embedding and Serving

The `web.PublicFS` embedded filesystem is mounted by the HTTP server. The server handles cache-busting by appending a build commit hash to asset URLs. Gzip compression is applied via `CompressionMiddleware` for responses under 1MB.

The login page (`login.html`) is not served as a separate route. Instead, `AuthMiddleware` intercepts unauthenticated requests and serves the login page inline, preserving the original URL in the browser's address bar. This means users never see a `/login` URL — they see the page they were trying to reach, with the login form overlaid.

### State Management

The Web UI uses a centralized `MoomboxApp` class (defined in `app.js`) as the single state container. It holds the current job list, filter/search state, theme preference, WebSocket connection, and references to loaded modules.

Persistent client-side state is stored in `localStorage`:
- Theme preference (dark/light)
- Search and filter state
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
| `bubbletea` | Core framework. Elm architecture: `Model` (state), `View` (render), `Update` (message dispatch). All state transitions happen through message passing. |
| `bubbles` | Pre-built components: `list` (task list), `viewport` (log viewer, detail scrolling), `spinner` (loading indicators), `paginator` (page navigation), `key` (key binding definitions). |
| `huh` | Form builder framework. Used for the Settings dialog and the Setup Wizard. Provides multi-step forms with inputs, selects, confirms, and validation. |
| `lipgloss` | Styling engine. Colors, borders, padding, margin, alignment, and layout composition. Every visual element in the TUI is styled through lipgloss. |

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

**Logs (bottom, full width):** Real-time log viewer. Lines arrive via batched messages (250ms flush window). Supports level filtering (debug/info/warn/error) and regex search. Auto-scrolls to newest entries unless the user has manually scrolled up.

**Focus navigation:** `Tab` / `Shift-Tab` cycles focus between panels. Mouse click on a panel changes focus. The focused panel receives keyboard input and has a visually distinct border.

### Source Files

| File | Purpose |
|------|---------|
| `app.go` | Main application model. Init, Update, View. Chord state machine. Menu builder. Action dispatcher. Message routing. Tick management. |
| `task_list.go` | Task list panel (top left). Job list rendering, selection, filtering, archive toggle, status icons. |
| `job_details.go` | Job details panel (top right). Metadata display, description toggle, progress rendering. |
| `log_viewer.go` | Log viewer panel (bottom). Log line buffering, level filtering, regex search, auto-scroll logic. |
| `status_bar.go` | Bottom bar. Connection status, disk usage, monitor timers, cookie status, update indicator. |
| `action_menu.go` | Command palette overlay (M key). Searchable list of all available actions. |
| `help.go` | Help overlay (? key). Displays all chords grouped by category. |
| `add_video.go` | Add Video overlay. Multi-step flow: URL input, format selection, timestamp configuration, confirmation. |
| `import_dialog.go` | Import overlay. Zip file upload with title/channel override fields. |
| `trim_dialog.go` | Trim overlay. Start/end time input, async encoding with progress display. |
| `files_dialog.go` | Orphaned files overlay. Browse and delete files that have no corresponding job. |
| `client_tokens_dialog.go` | Client token management overlay. List and delete persistent auth tokens. |
| `settings.go` | Settings overlay. Built with `huh` form framework. Full config editing. |
| `setup_wizard.go` | First-run setup overlay. Built with `huh`. Config, FFmpeg, yt-dlp plugin, cookies. |
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
| `A K` | Manage Client Tokens | No | No | — |
| `A O` | Browse Orphaned Files | No | No | — |

**Request chords (R prefix):**

| Chord | Action | Condition |
|-------|--------|-----------|
| `R C` | Recheck Cookies | Cookie recheck callback is configured |
| `R F` | Force Cookie Refresh | Cookie force-refresh callback is configured |
| `R V` | Check for Updates | Update check callback is configured |
| `R U` | Apply Update | An update is available and apply callback is configured |
| `R S` | Verify Signature | Signature verification callback is configured |
| `R P` | Restart Program | Restart callback is configured. Requires confirmation. |

**Open chords (O prefix):**

| Chord | Action | Requires Job | Job Filter |
|-------|--------|:------------:|------------|
| `O F` | Open Folder (explorer) | Yes | Job has an openable folder |
| `O S` | Open Stream Page (browser) | Yes | Job has a stream URL |
| `O W` | Open Web UI (browser) | No | — |

**Single-key shortcuts:**

| Key | Action |
|-----|--------|
| `F` | Cycle filter mode (All / Downloading / Finished / Error / etc.) |
| `` ` `` | Open Settings dialog |
| `?` | Open Help overlay |

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
| Orphaned Files | `A O` | File browser showing files in the output directory that have no corresponding database job. Delete with confirmation. |
| Client Tokens | `A K` | List of persistent client authentication tokens. Delete individual tokens. |
| Settings | `` ` `` | Full config editor built with the `huh` form framework. |
| Setup Wizard | First run | Multi-step initial setup: configuration, FFmpeg check/install, yt-dlp plugin, cookie capture. Built with `huh`. |
| FFmpeg Check | Setup flow | Validates FFmpeg is on PATH. Offers installation options if missing. |

### Async Message Types

The TUI receives backend state changes via typed messages delivered through Bubble Tea's command/message system. Backend goroutines send to Go channels; the TUI polls these channels via commands and converts received values into Bubble Tea messages.

**Core data messages (from backend channels):**

| Message Type | Source | Content |
|--------------|--------|---------|
| `JobUpdateMsg` | Database subscriber | Single job that changed. Contains the full `*database.Job`. |
| `JobsUpdateMsg` | Database subscriber | Full job list changed (job added or deleted). Contains `[]*database.Job`. |
| `LogBatchMsg` | Logger subscriber | Batch of log lines accumulated over a 250ms flush window. Contains `[]string`. |
| `CheckTimersMsg` | Monitor callbacks | Next check times for Feed, DECAPI, and Twitch monitors. |
| `CookieStatusMsg` | Cookie service | YouTube and Twitch authentication status (logged in, expired, missing). |
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

| Element | Content | Behavior |
|---------|---------|----------|
| Connection | Icon indicating online/offline | Reflects WebSocket-equivalent connection to backend |
| Disk Usage | Percentage of output drive used | Shows as warning (yellow) above threshold, critical (red) above higher threshold |
| Monitor Timers | Next check times for Feed, DECAPI, Twitch monitors | Counts down to next scheduled check |
| Cookie Status | YouTube and Twitch auth status | Shows whether cookies are valid, expired, or missing for each platform |
| Update Indicator | New version available | Appears when an update has been detected |

---

## WebSocket Protocol

### Connection Lifecycle

The Web UI establishes a WebSocket connection to the server on page load. The server uses `nhooyr.io/websocket` for WebSocket handling.

**Upgrade:** The WebSocket upgrade handler is registered as an interceptor on the main HTTP handler. Any request with an `Upgrade: websocket` header is routed to the WebSocket handler regardless of the URL path. Origin validation checks that the request comes from the same origin or a loopback/LAN alias.

**Authentication:** For external (non-loopback, non-private-network) connections when auth is configured, the `AuthCheck` function validates the upgrade request before accepting. Unauthenticated external WebSocket upgrades are rejected.

**Detached context:** The accepted WebSocket connection uses `context.Background()` rather than the HTTP request context. This prevents the connection from being killed by the server's `ReadTimeout`, which would otherwise close long-lived connections.

**Initial state:** Immediately after connection, the server sends an `initial_state` message containing the full current state: all jobs, buffered log lines (up to 200 from the ring buffer), config, and monitor check schedule. This allows the client to hydrate without making separate REST calls.

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
| `initial_state` | `{ jobs, logs, config, monitors, ... }` | Once, immediately after WebSocket connection is accepted |
| `job_update` | Single job object | When any field of a single job changes (throttled) |
| `jobs_update` | Full job array | When a job is added or deleted (full list, not incremental) |
| `log` | Log line string | When a new log line is emitted |
| `check_timers` | `{ feed, decapi, twitch }` timestamps | When monitor check schedules change |
| `pong` | Empty | Response to client `ping` messages |

### Client-to-Server Message Types

| Type | Payload | Purpose |
|------|---------|---------|
| `ping` | Empty | Client-initiated keepalive. Server responds with `pong`. |

### Broadcast Throttling

Job update broadcasts use a **100ms leading/trailing edge** throttle, applied per job ID:

1. **Leading edge:** When a `job_update` arrives and the last broadcast for that job ID was more than 100ms ago, the update is broadcast immediately. The timestamp is recorded.
2. **Trailing edge:** When a `job_update` arrives within the 100ms window, it is stored as "pending." A timer is scheduled for 100ms after the last broadcast. When the timer fires, the most recent pending state is broadcast.
3. **Cleanup:** Throttle state (timestamps, timers, pending data) for a job ID is purged 100ms after the trailing edge send completes.

This ensures:
- The first update is never delayed (immediate leading-edge send).
- Rapid-fire updates (e.g., progress ticks during active download) are collapsed to at most one broadcast per 100ms per job.
- The final state is always delivered (trailing-edge guarantees the last update is not lost).

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

| Method | Path | Rate Limit | Notes |
|--------|------|:----------:|-------|
| `GET` | `/api/auth/status` | — | Returns `{ authRequired, authenticated, hasPassword }`. Public. |
| `POST` | `/api/auth/login` | 5 req / 60s | Password body. Returns session token cookie. |
| `POST` | `/api/auth/logout` | — | Invalidates session. |
| `POST` | `/api/auth/set-password` | 3 req / 60s | Sets or changes the password. |
| `POST` | `/api/auth/remove-password` | 3 req / 60s | Removes password (disables auth). |

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

### Formats

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/formats/{videoId}` | Fetch available formats for a YouTube/Twitch video or stream. Used by the Add Video flow. |

### Status

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/status` | Aggregate status: version, uptime, active platforms, cookie status, Twitch auth, monitor timers, auto-cookie state. |

### Configuration

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/config` | Get current configuration. |
| `PUT` | `/api/config` | Update configuration. Triggers config save and may trigger restart. |
| `POST` | `/api/config/channels` | Add a monitored channel. |
| `DELETE` | `/api/config/channels/{id}` | Remove a monitored channel. |
| `POST` | `/api/resolve-channel` | Resolve a channel URL or name to a canonical channel identifier. |

### Cookies

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/cookies/recheck` | Force a cookie validity recheck. |
| `POST` | `/api/cookies/auto-refresh` | Trigger automatic cookie refresh. |
| `POST` | `/api/cookies/auto-setup/start` | Begin the auto-cookie setup flow (browser automation). |
| `POST` | `/api/cookies/auto-setup/finish` | Complete auto-cookie setup. |
| `POST` | `/api/cookies/auto-setup/cancel` | Cancel in-progress auto-cookie setup. |
| `GET` | `/api/cookies/auto-status` | Get current auto-cookie service status. |

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

### Updates

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/update/status` | Get current update status (available version, if any). |
| `POST` | `/api/update/check` | Manually check for updates. |
| `POST` | `/api/update/apply` | Download and apply an available update. Triggers restart. |
| `POST` | `/api/update/verify` | Verify the Ed25519 signature of the current binary. |
| `POST` | `/api/update/dismiss` | Dismiss the update notification. |

### FFmpeg

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/ffmpeg/check` | Check if FFmpeg is on PATH and return version info. |
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
| `POST` | `/api/restart` | Loopback only | Trigger application restart (exit code 42). |

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
| Cookie status | YouTube/Twitch auth status (valid/expired/missing) | Same, icon-based |
| Update indicator | New version badge | New version indicator |

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
- **LoopbackOnly** — Restricts access to loopback addresses (127.0.0.1, ::1). Used for `open-folder`, PO token routes, and `restart`.
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
