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
| `web/public/app.js` | ~3,840 | Main SPA module. Job list rendering (`renderJobs`/`renderJobItem`/`renderArchivedJobs`), WebSocket connection management, status bar, theme switching. The Files tab, log viewer, version indicator/update dialog, unified filter bar and job details dialog are delegated to controller modules below — each constructed with `this` in the same pattern as `settings.js`/`stats.js` — with a one-line delegating method kept on `MoomboxApp` for any cross-module call site. |
| `web/public/modules/files.js` | ~300 | `FilesController` — the Files tab: orphaned output files and orphaned feed-history entries, their refresh/delete-all actions. |
| `web/public/modules/log-panel.js` | ~270 | `LogPanelController` — the log viewer: level filter buttons, debounced text search with match highlighting, auto-scroll with the resume pill, clear. |
| `web/public/modules/update-indicator.js` | ~170 | `UpdateController` — the header version indicator and the update-available dialog (apply/skip). |
| `web/public/modules/filter-bar.js` | ~380 | `FilterBarController` — the unified filter bar shared by the Tasks and Archived tabs: token parsing/rendering, chip removal, channel/platform pickers. |
| `web/public/modules/job-details.js` | ~900 | `JobDetailsController` — the job details dialog: render, live updates, action buttons, per-job logs. The same information appears in the dashboard's details dialog as a **Set-aside Recordings (N)** section, rendered from the `asides` and `keptChatSidecar` fields of `GET /api/jobs/{id}`, with one **Recover** button — shown only when the job is not active — that POSTs `/api/jobs/{id}/recover-asides` and disables itself once the recovery is accepted. The global Files panel (`web/public/modules/files.js`) and the TUI's orphan overlay (`OrphanedFileEntry`, `internal/tui/files_dialog.go`) both name the set-aside recordings an orphaned staging entry holds, so an operator can tell captured footage from scratch space before deleting it. |
| `web/public/modules/player.js` | ~2,530 | Video player with per-job chat replay. Niconico-style scrolling overlay (`nico-lanes.js` `LaneAllocator` for lane collision, `nico-geometry.js` for the letterbox/row math, `nico-scheduler.js` `NicoScheduler` for the cursor/anchor/pending-list state machine; a two-edge bound covering both collision conditions, 4 s traverse, a pending list with a 2 s lateness bound, a "+N not shown" pill, overlay sized to the video's rendered rect with rows from a measured line box, 120 ms geometry settle, off by default under `prefers-reduced-motion`); sidebar with pre-show/post-end dividers and counts derived from the loaded message array, never a chat file header's own `messageCount` (`chat-timeline.js` `partitionChatByVideo`/`formatChatHeader`/`dividerLabelFor`); chat search; per-job chat offset (positive = chat earlier: effective = video + offset — a positive offset can spawn overlay rows the sidebar already marks `.post`, by design); resume/watched tracking; per-part Twitch chat merge for multi-segment jobs (`chat-timeline.js` `mergePartChats` over `GET /api/jobs/{id}/segments/{index}/chat`); keyboard shortcuts (`Space`/arrows/`F`/`M`/`C`/`S`) work right after selection, and the player's own `F` (fullscreen) overrides the Web UI's global `F` (filter-focus) shortcut while the player tab is active; during a window drag the overlay flies on its previous committed geometry until `NICO_GEO_SETTLE_MS` (120 ms) after the drag stops. A message enters at the right edge one second before its timestamp (`NICO_LEAD_MS`, niconico's lead) and is a quarter of the way across when its timestamp arrives; the cursor takes it up to one tick early (`NICO_TICK_AHEAD_MS`) and the Web Animations `delay` holds the exact entry instant, so the ~4 Hz `timeupdate` rate never shows and messages slide in instead of appearing mid-stage. Multi-segment seeking stitches separate segment video files into one timeline (`segments.js` `SegmentPlayer`). Sidebar chat renders four shapes beyond the flat row (2026-09-25 rulings K1-K4, sidebar only — the overlay keeps plain scrolling text): a YouTube Super Chat or Super Sticker as a two-part card in the tier's header/body colours, taken from the archive when `internal/chat` recorded them and from the seven-tier palette in `web/public/modules/player.js` when it did not (tier 0 = a neutral gray "unresolved" pair), with the text colour derived from the painted colour by WCAG relative luminance at a 0.5 threshold rather than a second table; a membership event (new member, milestone, gift purchase, gift redemption) as a green card carrying the renderer's own header line from `internal/chat/api.go`; a Twitch sub/resub/subgift/raid as a purple notice block whose first line is the wire's `systemMsg` or a rebuild from the archived fields, with every other USERNOTICE (`system`-typed by `internal/twitch/chat_irc.go`: prime and gift upgrades, viewer milestones, rituals, pay-forwards) rendering as the same block at reduced emphasis — half the tint, the accent at half opacity, the wire's `systemMsg` verbatim at normal weight, and nothing to rebuild from, so a `system` message with an empty `systemMsg` keeps the flat row; and a cheer chip coloured by Twitch's bits scale (its ink by the same luminance rule at the 0.18 max-contrast crossover — there is no Twitch ink to be faithful to). Every shape is one direct child of the message list carrying the `chat-msg` class, so the sidebar's active/future/post promotion, region dividers, search filter and scroll maths are unchanged. Clicking a message's timestamp — a `<button>` in every sidebar shape, reached by one delegated listener on the message list — seeks the video to `CHAT_SEEK_LEAD_MS` (3 s, a constant and not a setting) before the message's effective time, i.e. its offset minus the per-job chat offset, clamped at 0, through `seekToGlobalTime` (the multi-segment seek, which falls back to seeking the element directly for a single-file job); the existing `seeked` handler re-syncs the sidebar and re-anchors the overlay, and the play state is untouched. |
| `web/public/modules/segments.js` | ~140 | `SegmentPlayer` — multi-segment playback helper shared by the player and the trimmer: sequential segment sources, cumulative time offsets, cross-segment seeking. |
| `web/public/modules/chat-timeline.js` | ~240 | Pure chat/video timeline math: offset normalization, chat-to-video bias, pre-show/post-end partitioning, per-part chat merge, and offset recovery for legacy files (`deriveMissingOffsets` — a message with no offset of its own is placed from the chat file's header epoch and its `timestampUsec`, per part before the merge; without a header epoch it stays where it was). No DOM, no fetch; covered by `web/tests/chat-timeline.test.mjs`. |
| `web/public/modules/nico-lanes.js` | ~75 | `LaneAllocator` — the niconico lane-collision math (right-to-left constant-traverse scrolling) and seed-cursor selection on reset/seek. Pure; covered by `web/tests/nico-lanes.test.mjs`. |
| `web/public/modules/nico-geometry.js` | ~50 | Pure niconico overlay geometry: the centred-fit `letterboxStage`, `rowsFor` row count, and `sameStage`/`nextGeometry` change detection. No DOM — `player.js` reads the element sizes and writes the styles; covered by `web/tests/nico-geometry.test.mjs`. |
| `web/public/modules/nico-scheduler.js` | ~185 | `NicoScheduler` — the niconico overlay's cursor/anchor/pending-list/drop-count state machine and the `NICO_*` tuning constants (`player.js` imports them). Pure; covered by `web/tests/nico-scheduler.test.mjs`. |
| `web/public/modules/setup.js` | ~1,470 | First-run setup wizard. Walks the user through initial configuration, FFmpeg installation, yt-dlp plugin setup, and cookie capture. |
| `web/public/modules/settings.js` | ~3,220 | Settings dialog. Covers full config editing, channel management, cookie management, integration settings. |
| `web/public/modules/trimmer.js` | ~510 | Trim clip creation UI. Lets the user define start/end timestamps on a finished recording and create a trimmed clip. |
| `web/public/modules/stats.js` | ~190 | Statistics dashboard. Displays job counts, sizes, durations, and other aggregate metrics. |
| `web/public/modules/imports.js` | ~270 | Zip archive import. Upload a zip file containing video/chat/metadata to create a job from external content. |
| `web/public/modules/filter-parser.js` | ~130 | Filter query parser. Booru-style tag syntax: `status:active`, `channel:"name"`, `platform:youtube`, negation (`-tag`), OR groups (`a\|b`), quoting for spaces. Go twin: `internal/jobfilter`. |
| `web/public/modules/filter-engine.js` | ~90 | Filter engine. Evaluates parsed tokens against job objects. AND intersection across tokens, OR union within pipe groups. Go twin: `internal/jobfilter`. |
| `web/public/modules/logout.js` | ~50 | Status-bar logout icon: `logoutVisible` (shown only when `authRequired && authenticated`, read from `GET /api/auth/status` in `checkSecurityBanner`) and `bindLogout` (click → `POST /api/auth/logout` → reload). |
| `web/public/modules/utils.js` | ~980 | Shared formatting helpers (durations, file sizes, dates, etc.). |
| `web/public/moombox.css` | ~3,700 | All styles. Includes desktop layout, mobile responsive breakpoints, dark/light theme variables, and component-specific styles. |
| `web/public/favicon.svg` | — | SVG favicon for the web dashboard. |

### Embedding and Serving

Asset URLs inside `index.html` carry a `?v=<build commit>` cache-buster on a trusted commit:
`MountStaticFiles` rewrites the shell once at mount time and `serveIndex` (`internal/web/server.go`)
serves that copy for `/`, `/index.html` and every SPA-fallback route with `Cache-Control: no-cache` and
an `ETag` — the commit when trusted, otherwise the file's SHA-256 — answering `If-None-Match` with a
304; on an untrusted commit no `?v=` is emitted at all, so the served bytes are the embedded file the
hash describes. A URL that carries one is served `immutable, max-age=1y`, but only
when the commit is TRUSTED: `unknown` (no `-ldflags` stamp and no `vcs.revision`) and a
`<rev>-dirty` suffix (built from a modified working tree) both name bytes that can change under the
same string, so they fall back to the revalidating policy. Every other asset path is served `no-cache`
plus an `ETag` — the build commit when it is trusted, otherwise the file's SHA-256 — so a revalidation
costs a 304 rather than the whole file. `embed.FS` reports a zero `ModTime`, so that `ETag` is the only
validator available (see `staticCacheHeaders` in `internal/web/server.go`). Gzip compression is applied
via `CompressionMiddleware` for responses over 1 KB, except already-compressed bodies (`image/*`,
`video/*`, or a handler-set `Content-Encoding`). Every response that reaches the gzip wrapper — i.e.
one whose client offered gzip and whose path is not skipped — carries `Vary: Accept-Encoding`, so a
shared cache cannot hand a gzipped body to a client that negotiated identity.

The login page (`login.html`) is not served as a separate route. Instead, `AuthMiddleware` intercepts unauthenticated requests and serves the login page inline, preserving the original URL in the browser's address bar. This means users never see a `/login` URL — they see the page they were trying to reach, with the login form overlaid.

### State Management

The Web UI uses a centralized `MoomboxApp` class (defined in `app.js`) as the single state container. It holds the current job list, filter tokens, theme preference, WebSocket connection, and references to loaded modules.

**Unified filter state:** Each panel (Tasks, Archived) maintains an independent array of filter tokens (`tasksFilterTokens`, `archivedFilterTokens`). Tokens are parsed from user input by `filter-parser.js` and evaluated against jobs by `filter-engine.js`. Structured tokens (status/channel/platform) appear as visual chips (`sl-tag`); free text stays in the input. An optgroup dropdown offers clickable options grouped by Statuses, Platforms, and Channels (auto-populated from current jobs). The TUI's `/` box speaks the same language through `internal/jobfilter` (`Parse`, `Match`), and its `F` key cycles the `status:` token of that query.

Persistent client-side state is stored in `localStorage`:
- Theme preference (dark/light)
- Any module-specific preferences

### Module Loading

There is no lazy loading. `index.html` loads `/app.js` as `type="module"`, and every controller module (`setup`, `imports`, `player`, `settings`, `trimmer`, `stats`, `files`, `log-panel`, `update-indicator`, `filter-bar`, `job-details`, `utils`, `logout`) is a static top-level `import` at the head of `app.js`, so the whole module graph is fetched and evaluated on page load; `app.js` contains no dynamic `import()`.

### Mobile Responsiveness

`moombox.css` works through four width breakpoints plus a touch query:

| Breakpoint | Target | Behavior |
|------------|--------|----------|
| `992px` | Tablet | Collapses sidebar, adjusts grid layout |
| `768px` | Phone | Single-column layout, touch-optimized spacing; every `sl-dialog` becomes an edge-to-edge bottom sheet |
| `700px` | Phone (details dialog) | The details dialog's top grid (`.details-top`) drops to one column |
| `576px` | Small phone | The job table collapses to a single column and the details footer stacks |
| `hover: none` media query | Touch devices | Removes hover-dependent interactions, increases touch targets |

---

## TUI Architecture

### Framework and Technology

The TUI is built on the [Charmbracelet](https://github.com/charmbracelet) ecosystem:

| Package | Usage |
|---------|-------|
| `charm.land/bubbletea/v2` | Core framework. Elm architecture: `Model` (state), `View` (render), `Update` (message dispatch). All state transitions happen through message passing. |
| `charm.land/bubbles/v2` | Pre-built components: `list` (task list — page navigation goes through its embedded paginator; the standalone `paginator` package is not imported), `viewport` (log viewer, detail scrolling), `spinner` (loading indicators), `key` (key binding definitions), `textinput`, `table`, `progress`, `filepicker`. |
| `charm.land/huh/v2` | Form builder framework. Used by the Setup Wizard and the FFmpeg check overlay (and by `styles.go`, which supplies both with the Moombox theme) — those three files are the package's only importers. Provides multi-step forms with inputs, selects, confirms, and validation. |
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

**Task List (top left):** Displays all jobs as a scrollable list. Arrow keys navigate one row at a time; `PgUp`/`PgDn` move a page and `Home`/`End` jump to the first/last row, all four through the embedded bubbles list's own paginator (`PrevPage`/`NextPage`/`GoToStart`/`GoToEnd` in `internal/tui/task_list.go`), so the header's `[start-end/total]` range follows the cursor. Enter selects a job and populates the details panel. Status is shown via icons and colors. Divider row separates active from archived jobs; clicking or pressing Enter on the divider toggles archive visibility. A watched job carries a dim `•` between the platform tag and the title (`watchedGlyph`, counted in `titleWidth`).

**Job Details (top right):** Shows full metadata for the selected job: title, channel, platform, status, timestamps, progress, output file, quality, and available actions. Content auto-scrolls to accommodate long descriptions.

A job whose staging directory still holds recordings the engine set aside gets a **Set-aside Recordings (N)** section: one row per restart with its age, size and whether its resume sidecar survived, then a Chat Capture row saying whether the chat archive `keepOnlyChatCapture` preserves is still in there. The summary is pushed in by the App (`SetAsides`, `internal/tui/job_details.go`) rather than probed while rows are built, and the App memoises one job's answer, because `updateSelectedJob` (`internal/tui/app_update.go`) runs on every cursor move and every jobs update. `A S` acts on it.

**Logs (bottom, full width):** Real-time log viewer. Lines arrive via batched messages (250ms flush window). Supports level filtering (debug/info/warn/error) and vim-style search over the literal text typed (`/` to enter search, `n`/`N` to navigate matches, `Esc` to clear; the query is `QuoteMeta`-escaped, so a `.` matches a dot). Matched lines are highlighted in the viewport. **Long lines are hard-wrapped once, at insertion** — `wrapLogLine` and `rebuildFiltered` (`internal/tui/log_viewer.go`) split each survivor of the level filter to the panel's content width and the viewport runs with `SoftWrap` off, and `View` itself is memoised behind a render cache keyed on everything it reads. The viewport's own soft-wrap called `ansi.StringWidth` over every buffered line on every render — 2.35 ms and 6,192 allocations at the 1,000-line cap, 60% of a whole TUI frame — and that pass now runs about ten times a second on the insertion path instead of sixty times a second on the render path (CORE-2). One accepted consequence: search runs `FindAllStringIndex` over the already-wrapped viewport content, so **a term that straddles a wrap boundary no longer matches** — the same term matches on any line that fits, and widening the panel restores it. Auto-scrolls to newest entries unless the user has manually scrolled up.

**Focus navigation:** `Tab` / `Shift-Tab` cycles focus between panels. Mouse click on a panel changes focus. The focused panel receives keyboard input and has a visually distinct border.

**Minimum terminal size:** below 60 columns × 20 rows, `App.View` skips the panel layout entirely and renders a single "Terminal too small" line naming the current and required dimensions, since every panel/overlay computes negative or near-zero content widths under that floor.

### Source Files

All 43 non-test files of `internal/tui/`, grouped by role. Four of them form two build-tagged pairs (`openbrowser_*` and `clipboard_*`), so any single build compiles 41.

**The application model** — `App` is split across seven files rather than one; `app.go` holds the struct and its wiring only.

| File | Purpose |
|------|---------|
| `app.go` | Application model and its wiring: fields, constructor, backend callbacks and channel plumbing, tick scheduling, `View` delegation. |
| `app_update.go` | `Update`: the message switch over every backend, tick, overlay and async-result message, plus the feedback line and the job add/update/delete appliers. |
| `app_keys.go` | Key dispatch: the `Ctrl+C` test that runs ahead of every overlay intercept, overlay routing, the per-panel handlers, and the chord state machine. |
| `app_actions.go` | `buildMenuItems` — the single source of truth for chords, the action menu, the hints and the help overlay — and `dispatchAction`, the one handler every chord lands in. |
| `app_commands.go` | The `tea.Cmd` layer: the internal-token HTTP client and every `/api/` call the TUI makes, plus `Run`. |
| `app_layout.go` | Panel geometry, `View` composition, the minimum-terminal-size floor, the restart and security banners, feedback colouring. |
| `app_mouse.go` | Top-level mouse routing: active overlay first, then the panels. |

**Panels**

| File | Purpose |
|------|---------|
| `task_list.go` | Task list panel (top left). Job list rendering, selection, filtering, paging, archive toggle, status icons. |
| `job_details.go` | Job details panel (top right). Metadata display, description toggle, progress rendering. |
| `log_viewer.go` | Log viewer panel (bottom). Log line buffering, level filtering, insertion-time hard wrapping, literal search, auto-scroll logic, the `View` render cache. |
| `status_bar.go` | Bottom bar. Chord hints (left), disk usage, active download count, cookie status (right), and the `barTier` width ladder both halves descend. |

**Overlays**

| File | Purpose |
|------|---------|
| `action_menu.go` | Command palette overlay (`M`). A categorised list of every available action; selecting an entry executes it. |
| `help.go` | Help overlay (`?`). Displays all chords grouped by category. |
| `add_video.go` | Add Video overlay (`A A`). Multi-step flow: URL input, format selection, timestamp configuration, confirmation. |
| `import_dialog.go` | Import overlay (`A Z`). Zip file upload with title/channel override fields. |
| `cookie_import_dialog.go` | Cookie file import overlay (`E I`). Path prompt with `~` expansion and an existence check, then the per-platform import outcome. Only the path is ever displayed. |
| `trim_dialog.go` | Trim overlay (`A T`). Start/end time input, async encoding with progress display. |
| `files_dialog.go` | Orphaned files and history overlay (`A O`). Browse and delete files and history rows that have no corresponding job. |
| `client_tokens_dialog.go` | Client token management overlay (`A K`). List and delete persistent auth tokens. |
| `stats_dialog.go` | Statistics overlay (`E T`). Renders the `stats.Snapshot` the Web Stats tab reads, with a 60 s refresh tick while open. |
| `ytdlp_dialog.go` | yt-dlp plugin overlay (`E Y`). Renders `ytdlpplugin.Info` verbatim; `I` installs for the live port. |
| `release_notes_overlay.go` | Release notes overlay (`R N`). `glamour`-rendered Markdown in a `bubbles/viewport`; beside a pending update's own notes `U` applies it and `S` skips it. |
| `ffmpeg_check.go` | FFmpeg validation/installation overlay. Built with `huh`. `Esc` quits Moombox here rather than dismissing, because FFmpeg is required for muxing. |
| `setup_wizard.go` | First-run setup overlay, and — via `E L` — the standalone cookie-login step on a configured install. Built with `huh`. Config, FFmpeg, yt-dlp plugin, cookies. |

**The Settings overlay** — eight files, none of which uses `huh`: the editor is built from the package's own section and field tables over `text_input.go`. One row is a hybrid: `max_video_resolution` is a `fieldNumber` that also declares `options` (`resolutionPresets`, `internal/tui/settings.go`), so ←/→ step the preset ladder while typing still enters any custom value — a `fieldCycle` row gets no text input at all, which is why it is not one. The stepper is `cycleNumberPreset` (`internal/tui/text_input.go`), the numeric sibling of `cycleFieldOption`: it moves an off-ladder value to the nearest preset above or below rather than to the end of the list. `settings_components.go` suppresses the arrows for such a row so the step is not also a cursor move, and its `previewFn` names the preset (or `Custom: N`) on the dim line below. The dashboard's twin is an `<sl-select>` over the same ladder plus a Custom entry that reveals the numeric input (`RESOLUTION_PRESETS`, `web/public/modules/settings.js`). Both first-run wizards reach the unbounded mode too: the Web one through a blank-by-default select (`web/public/modules/setup.js`), the TUI one because `finishAdvancedSetup` (`internal/tui/setup_wizard.go`) reads the raw field text, not `vNum`, which cannot tell an empty entry from a typed `0`.

| File | Purpose |
|------|---------|
| `settings.go` | Settings model: the section/field tables, `Open`/`Close`, `loadValues`/`applyValues`, dirty and restart-required tracking. |
| `settings_view.go` | Settings rendering: header, hint line, action buttons, field rows, and each sub-editor's view. |
| `settings_keys.go` | Settings key handling: section and field navigation, edit mode, save/close routing. |
| `settings_channels.go` | Channel sub-editor: add, edit, delete, and the four per-channel overrides. |
| `settings_notifications.go` | Notification sub-editor: webhook list (an edit-mode target's row carries a dim `· one message per job`), per-event toggles, test send, the per-target `enabled` mute, the `mention` text field, the `m` key, which toggles a highlighted event row in the mention (`@`) column, and the Delivery row (Separate messages / One message per job, Space toggles). |
| `settings_security.go` | Security sub-editor: password set/remove, network access, and the external-access predicate `isExternalAccess`. |
| `settings_components.go` | The overlay's `textinput` components, including the decimal-capable fields backed by `config.FlexDuration`. |
| `settings_mouse.go` | Mouse support for the Settings overlay: click tabs, fields, toggles/cycles, and action buttons. |

**The dashboard's twin of that notification sub-editor** is a card per target in the Settings page's Notifications section (`renderNotificationsList`, `web/public/modules/settings.js`). Its header carries the webhook URL, an Enabled `<sl-switch>` (the `enabled` mute — a muted card also shows a "Muted" badge and is dimmed by `.notification-card--disabled`, `web/public/moombox.css`), and the test-send and delete buttons. Below it a Mention `<sl-input>`, and — only while there is a mention to ping with — a row of mention-event chips drawn over the same vocabulary as the event filter and lit from the RESOLVED list (`DEFAULT_MENTION_EVENTS`, `web/public/modules/settings.js`), so an absent `mention_events` shows the defaults without ever writing them down, and a Delivery chip pair (Separate messages / One message per job, `set-mode`). A target with no event filter renders an "All events" chip beside a "Filter..." chip rather than an empty row. All four controls auto-save through one `PUT /api/config` carrying only the notifications array, so none of them raises the page's unsaved-changes banner; a rejected save reverts the stored value and reports the server's per-field reason.

**Shared infrastructure**

| File | Purpose |
|------|---------|
| `styles.go` | Lipgloss style definitions — colors, borders, padding for all visual elements — and the `huh` theme the two form overlays share. |
| `keys.go` | Key name constants shared by the chord system and the panel handlers. |
| `mouse.go` | Mouse event handling. Click-to-focus, scroll delegation, region hit testing. |
| `marquee.go` | Scrolling text animation for long strings that do not fit in available width. |
| `text_input.go` | Custom text input component (extends bubbles). |
| `progress_store.go` | Tracks download progress state for active jobs. |
| `monitor_checking.go` | The "a check is running right now" sentinel (`MonitorCheckingSentinelMs`, rendered as `…`), kept distinct from "no channels" and from a real future timestamp. |

**Platform-specific pairs** — each is a build-tagged two-file pair, so exactly one of each compiles.

| File | Purpose |
|------|---------|
| `openbrowser_windows.go` / `openbrowser_other.go` | Builds the browser-open command. Windows uses `explorer.exe` (not `cmd /c start`) so a cold-started browser is re-parented outside the launcher's kill-on-close Job Object. |
| `clipboard_windows.go` / `clipboard_other.go` | The `O C` system-clipboard backup. Windows spawns `clip.exe` on a local console only; everywhere else there is no dependency-free equivalent and the function reports false, leaving OSC 52 as the sole mechanism. |

### The Chord System

The chord system is the TUI's keyboard shortcut mechanism. It uses a prefix-key pattern where the user presses a prefix key followed by an action key, with an optional third confirmation key for destructive actions.

#### State Machine

The chord system is a three-state finite automaton:

1. **Idle** — No prefix active. Waiting for a prefix key or single-key shortcut.
2. **Prefix active** — A prefix key has been pressed. Waiting for the action key. A feedback message shows the available actions for this prefix.
3. **Confirm active** — A two-key chord was entered for a destructive action (`NeedsConfirm: true`). Waiting for the user to press the action key again to confirm.

**Timeout:** All chord states expire after **3 seconds** of inactivity. If the user presses a prefix key and does nothing for 3 seconds, the chord resets to Idle. If a confirm prompt is pending and 3 seconds pass, it also resets.

**Invalid keys:** If the user presses a key that does not match any valid action for the current prefix, the chord resets to Idle, the feedback line shows `Invalid Chord: <prefix> <key>` for a second, and the key is consumed — it is not re-evaluated as a new prefix or single-key shortcut. A valid second key whose chord needs a job, pressed with no job selected or with a selected job that fails the item's `JobFilter`, resets the same way and shows the item's `DisabledReason` (the words the action menu uses beside a greyed entry) in the advisory colour.

#### Single Source of Truth

`buildMenuItems()` in `internal/tui/app_actions.go` is the **single source of truth** for all chords. It returns a slice of `ActionMenuItem` structs, each defining:

- `Chord` — the key combination (e.g., `"A A"`, `"R C"`, `"F"`)
- `Label` — full description (e.g., `"Add Video"`)
- `HintLabel` — abbreviated label for the status bar hint
- `Category` — grouping for the help overlay and action menu
- `NeedsJob` — whether the action requires a selected job
- `NeedsConfirm` — whether the action requires a third confirmation keypress
- `JobFilter` — optional predicate that filters which jobs the action applies to

`dispatchAction(chord, job)` in `internal/tui/app_actions.go` is the **unified handler** that executes the action for any chord. Adding a new chord requires exactly two changes: one entry in `buildMenuItems()` and one case in `dispatchAction()`.

#### Prefix Keys

| Prefix | Category | Description |
|--------|----------|-------------|
| `A` | Action | Job manipulation: add, import, retry, cancel, delete, trim, orphans, tokens |
| `R` | Request | Backend requests: cookies, updates, signature verification, restart |
| `O` | Open | Open external resources: folder, stream page, web UI |
| `E` | Extras | Side errands that touch no job: yt-dlp plugin, cookie login, cookie-file import, statistics |
| `Q` | Quit | Application exit |

#### Complete Chord Catalog

**Action chords (A prefix):**

| Chord | Action | Requires Job | Confirm | Job Filter |
|-------|--------|:------------:|:-------:|------------|
| `A A` | Add Video dialog | No | No | — |
| `A Z` | Import Archive | No | No | — |
| `A R` | Resume Job | Yes | No | YouTube, staging files present, and status is Error, Cancelled, COOKIES?, or Finished with an incomplete tail |
| `A I` | Reinitialize Job | Yes | No | Status is Error, Cancelled, or COOKIES? |
| `A M` | Mux Job | Yes | Yes | Status is Cancelled or Error, and segment files are present; or Finished while a split part its finalize could not mux is still in staging (`HasUnmuxedParts`) |
| `A S` | Recover Set-aside Recordings | Yes | Yes | Status is not Downloading, Muxing, Live or Upcoming (`JobIsActive`, `internal/tui/app_actions.go`, the twin of `IsActiveJobStatus` in `internal/worker/orphans.go`), and the job's staging directory still holds a recording the engine set aside — the status half answers the menu's "no jobs" question at open, the disk probe runs when the action is chosen |
| `A C` | Cancel Job | Yes | Yes | Status is not Finished, Cancelled, or Error |
| `A D` | Delete Job | Yes | Yes | Status is Finished, Error, Cancelled, or COOKIES? — the Web's `DELETE_STATUSES` (`web/public/modules/utils.js`), so neither UI offers Delete for a running job |
| `A W` | Toggle Watched | Yes | No | Status is Finished |
| `A T` | Trim Video | Yes | No | Status is Finished and has output file |
| `A K` | Manage Client Tokens | No | No | Client tokens callback configured |
| `A O` | Browse Orphaned Items | No | No | — |

**Request chords (R prefix):**

| Chord | Action | Condition |
|-------|--------|-----------|
| `R B` | Re-scan Feed History | Backfill rescan callback is configured. Forces a full-catalog backfill re-scan of every configured YouTube channel. |
| `R C` | Recheck Cookies | Cookie recheck callback is configured |
| `R F` | Refresh Cookies from Browser | Cookie force-refresh callback is configured |
| `R V` | Check for Updates | Update check callback is configured |
| `R M` | Check Monitors Now | Force-check callback is configured. Forces an immediate poll of every configured monitor; debounced against rapid repeats. |
| `R N` | View Release Notes | Always available. Shows pending-update notes when an update is available; otherwise fetches current version's notes from GitHub. From inside the overlay: `Esc`/`Q` closes; beside a pending update's own notes `U` applies it and `S` skips it (`OnDismissUpdate` → `routes.DismissUpdate`, the same helper `POST /api/update/dismiss` uses). |
| `R U` | Apply Update | An update is available and apply callback is configured |
| `R S` | Verify Signature | Signature verification callback is configured |
| `R P` | Restart Program | Restart callback is configured. Requires confirmation. |

**Open chords (O prefix):**

| Chord | Action | Requires Job | Job Filter |
|-------|--------|:------------:|------------|
| `O F` | Open Folder (desktop file manager) | Yes | Job has an openable folder |
| `O S` | Open Stream Page (browser) | Yes | Job has a stream URL |
| `O W` | Open Web UI (browser) | No | — |
| `O C` | Copy Stream URL to clipboard. The OSC 52 write (`tea.SetClipboard`) goes out on **every** press, on every platform — it is the only mechanism that reaches the terminal the operator is actually sitting at, which over SSH is not the machine Moombox runs on — and the feedback line reads `Sent to terminal clipboard (OSC 52): <url>`, claiming nothing more, because conhost and tmux-without-`set-clipboard` drop OSC 52 in silence. On a **local Windows console** a `clip.exe` child fed on stdin runs in addition, inside a `tea.Cmd` so a wedged child cannot freeze rendering or input; if it reports that it took the text the line upgrades to `Copied: <url>`. That backup stands down for Windows Terminal (`WT_SESSION`, whose own OSC 52 handling is authoritative) and for any SSH session (`SSH_CONNECTION`/`SSH_TTY`/`SSH_CLIENT`), where it would write the server's clipboard (`clipboardFeedback` in `internal/tui/app_actions.go`, `osClipboardFallback` in `internal/tui/clipboard_windows.go`). | Yes | Job has a stream URL |
| `O G` | Open GitHub Page (browser) | No | — |

**Extras chords (E prefix):**

The four side errands that used to sit under `R`. None of them acts on a job or asks the running program for anything, and each is registered only when its own callback is wired. `buildMenuItems` emits them after every `Open` item, because the action menu heads a group wherever consecutive `Category` changes.

| Chord | Action | Condition |
|-------|--------|-----------|
| `E Y` | yt-dlp Plugin | Status callback is configured. An async overlay over `routes.YtdlpPluginStatus` — the same computation `GET /api/ytdlp-plugin/status` returns — showing installed / plugin dir / live port / the port the installed file points at / an unrecognised-file row when the installed file's URL line does not parse / mismatch / the plugin's on-disk path when known. `I` rewrites the plugin for the live port through `routes.InstallYtdlpPlugin` (the call the dashboard's Install button makes) and reloads; `R` re-reads; `Esc`/`Q` closes. `I` is gated separately: with a status callback and no install one the overlay still reads, and `I` says the install is unavailable rather than no-opping. |
| `E L` | Cookie Login | Interactive-setup callback is configured (`SetSetupCallbacks`, bound unconditionally by `cmd/moombox`). Opens the setup wizard's cookie step **alone** — pick YouTube or Twitch, sign in in the browser that opens on the host, `Enter` extracts. Preselects the platform the status bar is flagging for re-login. |
| `E I` | Import Cookie File | Import callback is configured (auto-cookie service present). A path prompt (with `~` expansion and an existence check), then `AutoCookieService.ImportCookies` — the same verify-and-roll-back path as the Web import panel — then the per-platform outcome (imported / unchanged / rolled-back / rejected) in the overlay. The file is read in `cmd/moombox`; only the path is ever shown or logged. |
| `E T` | Statistics | Stats callback is configured. Shows the Web Stats tab's figures — disk bar with used/free, six storage and seven activity rows, uptime — from `stats.Build` (`internal/stats`); `R` refreshes, a 60 s tick refreshes while open, `Esc`/`Q` close. |

**Single-key shortcuts:**

| Key | Action |
|-----|--------|
| `F` | Tasks panel: cycle the query's `status:` token — none → `status:active` → `status:issues` → `status:finished` → none, replacing any existing status token in place. Details panel: toggle description expansion. Logs panel: cycle log level. |
| `M` | Open Action Menu (command palette) |
| `` ` `` | Open Settings dialog |
| `?` | Open Help overlay |
| `/` | Tasks panel: open the filter query box, which speaks the dashboard's filter language — free text plus `status:`/`channel:`/`platform:` tokens, `-` negation, `a\|b` OR groups, quoted values. Free text is a case-insensitive substring of the title, channel name or video ID (both UIs). `Enter` applies and closes; losing panel focus closes the box but keeps the applied query. Log panel: enter search mode. `n`/`N` navigate to next/previous match. `Esc` clears search and returns to normal scroll. |
| `Esc` | Clear, in this order: the batch selection, then the active filter (Tasks panel) — the typed text and the `F`-set status token are one state, so this drops both together — then any armed chord. |
| `Space` | Tasks panel: toggle the focused row's batch selection. The status bar shows the count, and every batch-capable chord (`A R`, `A I`, `A C`, `A D`, `A W`) then acts on the selection instead of the cursor row, re-applying its own status filter to it. |
| `PgUp` / `PgDn` | Page scroll: the Details and Logs viewports, and the Tasks panel's list (a whole page of rows per press). |
| `Home` / `End` | Tasks panel: jump to the first / last row. |
| `Ctrl+U` / `Ctrl+D` | Half-page scroll in the focused panel's viewport (Details, Logs) and in the Help and Release Notes overlays. |
| `Ctrl+C` | Quit immediately, from anywhere. bubbletea v2 delivers it as an ordinary key press (`tea.InterruptMsg` arrives only from a real SIGINT), so `handleKey` tests for it **before every overlay intercept** — no overlay, dialog, settings form, search box or armed chord can swallow it. Quitting out of the setup wizard's cookie step cancels the browser it opened first (`OnCancelAutoCookie`), so no headed browser is orphaned holding the acquisition slot. |
| `End` | Log panel: resume auto-scroll (jump to the newest line and follow it again). |
| `c` | Clear the log view (log panel focused only). Drops history, the filtered view, and any active search; the level filter is kept. |

**Quit chord:**

| Chord | Action | Confirm |
|-------|--------|:-------:|
| `Q Q` | Quit application | Yes (the second Q is the confirmation) |

### Overlays (Modal Dialogs)

Overlays are full-screen or near-full-screen modal views that take over keyboard input. When an overlay is active, the panel layout is hidden and all input routes to the overlay. Pressing `Escape` closes most overlays — the FFmpeg-not-found overlay is the exception and QUITS on `Escape`, because FFmpeg is required for the muxing half of the pipeline and an overlay that merely dismissed itself would leave a Moombox that cannot finish a download. `Ctrl+C` is checked ahead of every overlay and always quits (see the single-key table above).

| Overlay | Trigger | Description |
|---------|---------|-------------|
| Help | `?` | Displays all chords grouped by category with descriptions. Read-only. |
| Action Menu | `M` | Command palette. A categorised list of every available action; selecting an entry executes it. Filtering is disabled (`SetFilteringEnabled(false)`) — the list is short enough to scroll, and `PgUp`/`PgDn`/`Home`/`End` page it (and the job picker a job-bound entry opens), landing on an entry rather than a category heading. Both lists page instead of scrolling, so when one spans more than a page its footer carries `‹page›/‹pages› PgUp/PgDn`. |
| Add Video | `A A` | Multi-step form: (1) enter URL, (2) fetch and select format, (3) set timestamps, (4) confirm. Format fetch is async with a spinner. On error, auto-advances past format selection after a timeout. |
| Import | `A Z` | Zip import form with title and channel override fields. |
| Trim | `A T` | Clip creation. Enter start/end seconds. Encoding runs asynchronously with a progress callback that updates the UI. |
| Orphaned Files & History | `A O` | Two sections in one list (with a divider): orphaned files — output files no job's row names (never one a running finalize or part mux is still writing; `claimOutputStem`, `internal/worker/output_claims.go`) and staging directories no active job needs (an Error or Cancelled job's staging is listed under that job's title and status) — and orphaned processing-history rows (history entries with no matching job, which otherwise block re-discovery). Each section loads independently — a failure in one is shown inline without hiding the other. Delete with confirmation. `A` deletes every entry in the half the cursor is in (files or history) after the same two-press confirm as `D`; per-item failures are collected and listed in the dialog. |
| Client Tokens | `A K` | List of persistent client authentication tokens. Delete individual tokens. |
| Settings | `` ` `` | Full config editor built from the package's own section/field tables (`internal/tui/settings.go`) over `text_input.go` — not `huh`. Supports full mouse interaction (click tabs, fields, toggles, cycle options, and action buttons). Action buttons at the bottom: `[ Save & Return ]` / `[ Return Without Saving ]` (when dirty), or `[ Return ]` (when clean). Presents a close confirmation when there are unsaved changes and the user attempts to dismiss. Smart dirty tracking: reverting a field back to its original value clears the dirty flag. Job detail panel renders clickable hyperlinks (OSC 8) for stream URLs and output paths. Both channel editors expose the three per-channel overrides (`output_directory`, `archive_window_days`, `archive_slots`); blank means the global value, and the TUI editor now preserves every field it does not show (it rebuilt the channel from the visible fields before Arc B). The Network section's `public_url` row (`PublicURL`, `internal/config/types.go`) is a plain text field, blank by default; see [operations.md](operations.md) § Target Options for what setting it does to a notification embed's links. |
| Setup Wizard | First run, `E L` | Multi-step initial setup: configuration, FFmpeg check/install, yt-dlp plugin, cookie capture. Built with `huh`. `E L` opens the same overlay in **cookie-only** mode: the cookie step with no stages around it, `Esc` and the third row close it instead of advancing, and leaving cancels any browser it opened. |
| FFmpeg Check | Setup flow | Validates FFmpeg is on PATH. Offers installation options if missing. On Linux, also shows the distro-appropriate package manager command (`apt`, `dnf`, `pacman`, etc.), computed in-process by `linuxFFmpegInstallSuggestion` (`internal/tui/ffmpeg_check.go`) from `/etc/os-release` — the same mapping `GET /api/ffmpeg/install-suggestion` serves the Web wizard; the TUI makes no HTTP call for it. |
| yt-dlp Plugin | `E Y` | Async status overlay for the yt-dlp PO-token plugin (`YtdlpDialogModel`, `internal/tui/ytdlp_dialog.go`). Renders `ytdlpplugin.Info` verbatim rather than re-deriving it for the terminal. `I` installs/reinstalls for the live port and reloads, `R` refreshes, `Esc` closes. |
| Release Notes | `R N` | Shows release notes for the pending update (when an update is available) or the current version (fetched from GitHub). Rendered via `glamour` in the TUI. From inside: `Esc`/`Q` closes. Uses `bubbles/viewport` for scrolling. `U` applies and `S` skips the pending version (`OnDismissUpdate` → `routes.DismissUpdate`); both are offered, and act, only while the notes on screen are a pending update's. |

### Async Message Types

The TUI receives backend state changes via typed messages delivered through Bubble Tea's command/message system. Backend goroutines send to Go channels; the TUI polls these channels via commands and converts received values into Bubble Tea messages.

**Core data messages (from backend channels):**

| Message Type | Source | Content |
|--------------|--------|---------|
| `JobUpdateMsg` | Database subscriber | Single job that changed. Contains the full `*database.Job`. |
| `JobAddedMsg` | Database subscriber | A newly added job (lifecycle event) — appended directly to local state instead of triggering a full job-list rebuild. Contains `*database.JobAdded`. |
| `JobDeletedMsg` | Database subscriber | The ID of a removed job (lifecycle event) — the row is removed from local state directly instead of reloading a fresh full-list snapshot. Contains `*database.JobDeleted`. |
| `TrimsChangedMsg` | Database subscriber (re-fetched via `tui_wiring`) | A refreshed `*database.Job` snapshot after a trim was added or deleted; applied to the cached row and, if selected, the detail panel. |
| `JobsUpdateMsg` | Wiring, resync, and the two bulk writes | Full job list snapshot, sent in three situations: the initial list when the TUI is wired, the catch-up after a dropped update (see §Non-Blocking Channel Communication), and the two remaining `OnJobsChange` producers — `BatchSetWatched` and `DeleteJobsAndHistoryForChannel` (`internal/database/database_jobs.go`), both of which can touch 100+ rows at once. An ordinary add or delete does NOT come this way: `JobAddedMsg` and `JobDeletedMsg` are their own lifecycle events. Contains `[]*database.Job`. |
| `LogBatchMsg` | Logger subscriber | Batch of log lines accumulated over a 250ms flush window. Contains `[]string`. |
| `CheckTimersMsg` | Monitor callbacks | Next check times for Feed, DECAPI, and Twitch monitors. |
| `CookieStatusMsg` | Cookie service | `{YT, TW, YTActive, TWActive}` — one `CookieStatus` per platform (`None`, `OK`, `CookiesOnly`, `Relogin`, `Unknown`, `FileUnreadable`) plus each platform's active flag. There is no *expired* state: expiry has no UI reader at all. See §Status Bar. |
| `DiskStatusMsg` | Disk monitor | Disk usage percentage and warning/critical thresholds. |
| `BackfillStatusMsg` | Feed monitor backfill sweep | One message per completed scan page (`state: "scanning"`) plus one per scan-state change (`"done"`, `"error"`, `"idle"` — those carry `Tab` `""` and `Pages` 0); mirrors the Web's `backfill_status` WebSocket payload. |
| `UpdateStatusMsg` | Updater | New version available (tag name, release notes). An empty `Version` means "cleared" (a Web-side dismiss) and carries the skipped `TagName`: the TUI drops its badge only when that tag is the release it is showing, so a dismiss racing a newer release cannot blank a badge nobody skipped. |
| `ConnectivityMsg` | Connectivity monitor | Online/offline transition (`Online bool`). |
| `channelClosedMsg` | Channel poll commands | Sent when one of the eleven backend channels `listenForUpdates` selects on closes, naming it so the App nils the field and stops polling it: `jobUpdate`, `jobAdded`, `jobDeleted`, `jobTrimsChanged`, `jobsUpdate`, `log`, `checkTimers`, `cookieStatus`, `diskStatus`, `backfillStatus`, `updateStatus`. A name with no case would leave the field set and the select would re-fire on the closed channel forever. |

**Internal tick messages:**

| Message Type | Interval | Purpose |
|--------------|----------|---------|
| `tickMsg` | 1 second | Main tick. Updates clocks, checks chord timeouts, refreshes dynamic content. Once a minute it also runs the archive sweep, which re-reads `hide_finished_age_days` from the config store (`syncHideFinishedAge` in `internal/tui/app.go`) before re-bucketing aged rows — a threshold changed from the dashboard reaches the TUI through no event at all, so this sweep is the whole Web → TUI direction and bounds its latency at 60 s. A change made in the TUI travels the other way immediately, through the shared `broadcastHideFinishedAge`. |
| `progressTickMsg` | 8ms (active) / 500ms (idle) | Progress bar animation. One tick per frame at the 120 fps renderer during active downloads (`tea.WithFPS(tuiTargetFPS)` at the package's one `tea.NewProgram` site), dropping to 2fps when idle to save CPU. A tick that finds nothing new costs no rebuild (`SetProgress` skips an unchanged pointer inside one wall-clock second) and no repaint (the renderer diffs and skips an unchanged frame) — only the memoised `View()` bubbletea runs after every message (the 30-alloc frame the FrameCost pins hold; ~0.25-0.6 ms on a desktop at 20-1,000 jobs), so the extra ticks buy latency for about +2-4% of one core while a download is active. |
| `logFlushMsg` | 250ms | Triggers flushing accumulated log lines from the buffer to the viewport. |
| `marqueeTickMsg` | 150ms | Advances scrolling marquee text for overflowed labels. |
| `cookieCountdownTickMsg` | 1 second, while the setup wizard's cookie step is counting down | Decrements the cookie-capture countdown; ticks from a superseded chain (`gen`) are ignored so a stacked chain cannot drain the countdown faster than one per second. |
| `statsRefreshTickMsg` | 60 seconds, while the `E T` Statistics overlay is open | Refreshes the overlay on the Web Stats tab's own poll cadence; a tick naming an earlier `Epoch` than the current open is dropped. |

**Async operation result messages:**

These are returned by Bubble Tea commands that perform HTTP requests to the backend. Each carries the operation result (success data or error).

| Message | Operation |
|---------|-----------|
| `updateCheckResultMsg` | Manual update check |
| `updateApplyResultMsg` | Update download and apply |
| `dismissUpdateResultMsg` | Skip pending update version (Release Notes overlay's `S` key) |
| `releaseNotesFetchedMsg` | Fetch current version's release notes (`R N` when no update is pending) |
| `signatureVerifyResultMsg` | Ed25519 signature verification |
| `fetchFormatsResultMsg` | Format list fetch for Add Video |
| `fetchFormatsAutoAdvanceMsg` | Timer to auto-skip format selection on error |
| `addVideoResultMsg` | Job creation result |
| `importResultMsg` | Zip import result |
| `cookieImportResultMsg` | Cookie file import (`E I`) |
| `createTrimResultMsg` | Trim creation result |
| `deleteTrimResultMsg` | Trim deletion result |
| `deleteJobsResultMsg` | Job deletion, single or batch (`A D D`) |
| `setWatchedResultMsg` | Toggle watched flag (`A W`) |
| `fetchOrphansResultMsg` | Orphaned file list fetch |
| `deleteOrphanResultMsg` | Orphaned file deletion |
| `fetchOrphanedHistoryResultMsg` | Orphaned processing-history list fetch |
| `deleteHistoryEntryResultMsg` | Orphaned history entry deletion |
| `bulkOrphanResultMsg` | Bulk delete-all sweep over one Orphaned Files/History section (the `A O` overlay's `A` key) |
| `ffmpegCheckResultMsg` | FFmpeg PATH check |
| `ffmpegPrepareResultMsg` | FFmpeg download preparation |
| `ffmpegConfirmResultMsg` | FFmpeg install confirmation |
| `ffmpegMenuActionMsg` | FFmpeg check/install menu action resolved via `huh` form completion |
| `backfillRescanQueuedMsg` | Feed-history re-scan queued (`R B`) |
| `cookieRecheckResultMsg` | Cookie recheck |
| `cookieForceRefreshResultMsg` | Cookie force refresh |
| `channelResolvedMsg` | Channel URL/name resolution |
| `fetchClientTokensResultMsg` | Client token list fetch |
| `deleteClientTokenResultMsg` | Client token deletion |
| `ytdlpStatusMsg` | yt-dlp plugin status fetch (`E Y`) |
| `ytdlpInstallResultMsg` | yt-dlp plugin install/reinstall (`E Y`'s `I`) |
| `statsSnapshotMsg` | Statistics snapshot fetch (`E T` open/refresh) |
| `setupCookieFinishMsg` | Setup wizard cookie step completion |
| `setupSaveResultMsg` | Setup wizard config save |
| `testNotificationResultMsg` | Test notification send (Settings overlay) |
| `panicRecoveryMsg` | Panic recovered from an async `tea.Cmd` closure |

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

If a send is dropped, a drop counter increments **and a replay flag is armed** (`newTUIResync` in `cmd/moombox/tui_wiring.go`), with one warning line per streak. **The next SUCCESSFUL send** — the first event that gets through once the Bubble Tea loop drains a slot — takes the flag and triggers a full state refresh (re-fetching all jobs from the database and pushing that snapshot down the full-list channel), so the TUI catches up on the missed intermediate updates in one pass. A 1 s backstop ticker covers the two cases no successful send reaches: a job that goes quiet after the drop, and a channel that stays full. The replay deliberately does NOT run on the way into a forwarder: the forwarders are called inline on the `UpdateJobFields` writer goroutine, which runs at the configured progress interval (~60 Hz at the 16 ms default), so a replay there would fire once per *dropped* message and charge a full-table read to the download workers for every message the stalled TUI could not take — feeding the backlog it is recovering from. Taking the flag with a compare-and-swap is what makes it one catch-up refresh per streak rather than one per dropped message, and a refresh whose fetch or whose delivery fails re-arms the flag instead of clearing it — a dropped `Downloading → Finished` can therefore never leave a stale row for the rest of the session. A full list delivered by `OnJobsChange` (the two bulk writes) does not clear the flag either: **a delivered list is NOT assumed newer than a pending replay** — it is the bulk write's own time-of-write snapshot, so an update that landed after it and was then dropped is not in it. The four drop-counting forwarders share one body, `forwardOrDrop` in the same file, so the replay, the drop counter and the once-per-streak warning cannot drift between them. This ensures the TUI never blocks a backend goroutine, even if the Bubble Tea event loop is busy processing a complex view update.

### Backend Communication

The TUI makes HTTP requests to the same server that serves the Web UI. It uses a custom `http.RoundTripper` that:

1. Injects the `X-Internal-Token` header on every request. This token (a 16-byte random hex string generated at server startup) bypasses CSRF validation, since the TUI cannot perform origin/referer checks.
2. Adjusts the base URL to match the server's actual bound port and TLS configuration. If the server is configured for HTTPS, the TUI uses `https://`.

This means the TUI has the same API surface as the Web UI — it calls the same `/api/` endpoints. The only difference is the CSRF bypass via the internal token header.

### Status Bar

The status bar occupies the bottom row of the terminal. It displays at-a-glance system health and state:

The left side shows chord hints (key labels for A, R, O, E, F, M, Tab, backtick, ?) — the left side sheds its labels along the same width-tier ladder as the right (see below). The right side shows metrics and status.

**Both halves are rendered against a width tier** (`barTier` in `internal/tui/status_bar.go`): `tierFull` → `tierCompact` → `tierKeys` → `tierTight` → `tierEssential` → `tierNone`. The tier decides both the label length and whether an element appears at all, and the rule the cookie indicators follow is that **reassurance is dropped before an alarm is**.

The two halves do not descend together. `statusBarDescent` is a fixed eleven-step ladder of `(left, right)` tier pairs: the right steps down alone as far as `tierKeys` (status verbosity is the cheapest thing to lose), then the left follows down to `tierKeys` shedding its chord labels, and from there the two alternate, right first, to `tierNone`. `fitTiers` renders both ladders and returns the first pair whose combined width plus the one-column gap fits — which is also the richest pair that fits, because every step lowers exactly one side by exactly one rung and both ladders narrow monotonically (pinned by `TestStatusBarTiersNarrowMonotonically`). There is no width threshold anywhere in this: the bar measures its own rendered content.

| Element | Content | Behavior |
|---------|---------|----------|
| Connectivity | `OFFLINE`, abbreviated to `OFF` at `tierTight` | An alert, so it outlives every counter and only ever abbreviates |
| Batch selection | `N selected`, `N sel` at `tierKeys` | Shown when > 0 and only at `tierKeys` or wider; the count is the point, so it abbreviates before disappearing |
| Backfill scan | `Backfill <channel>: <tab> p<n>`, `BF:<tab> p<n>` at `tierCompact` | Routine background activity, so it is the first thing dropped (gone at `tierKeys`). One in-flight scan at a time — scans are serial — and the display name is clipped to 16 runes. |
| Disk Usage | `Disk 45% (120G free)` → `D:45% 120G` → `D:45%` | Green normally, yellow (`warn`), red (`critical`). Survives `tierEssential` only when warn/critical — a healthy disk says nothing there. |
| Active Downloads | Count of `Downloading` / `Live` / `Muxing` jobs | Shown when > 0 and only at `tierKeys` or wider. `Queued` is deliberately excluded — a queued job is waiting for an archive slot, not downloading. |
| Cookie Status | Per-platform `YT` / `TW` indicators | `renderCookieStatus`. See below. |

**Cookie indicators.** One indicator per *active* platform (`SetActivePlatforms`, fed from `config.GetActivePlatforms`); an inactive platform renders nothing. Each is a `CookieStatus` (`internal/tui/status_bar.go`), projected from one `cookies.AuthStatus` triple by `cookieBadgeFor` in `cmd/moombox/tui_wiring.go` — `authenticated` wins outright, then an unreadable `cookies.txt` reports `FileUnreadable`, then "no cookies at all" reports `None` whatever the verdict says, then `RefreshFailed` with cookies present is `CookiesOnly`, and everything else is `Unknown`. `cookieBadgeFor` never returns `Relogin` — that state is applied one level up, where `authStatusToTUI` (same file) overwrites the badge whenever `AutoCookieService.ReloginStatus()` flags the platform, unconditionally and not gated on `auto_enabled`, matching the Web badge's first arm. `hasCookies` is the loose predicate on both sides, so a half-cleared jar reads as configured rather than as never-set-up (see `data-and-storage.md §Cookie Jar`). **At `tierFull` a flagged badge also names `E L`**, the chord that opens the login, once for the bar however many platforms are flagged. It is dropped at the first squeeze on purpose: the alert is the information, the remedy is also in the menu and in help, and the hint is the widest thing this section can add — `metricTiers` must narrow monotonically or `fitTiers`' first-fit-is-richest-fit scan stops holding. The badge and the chord read one predicate, `ReloginPlatform`, so the platform named in the bar is the platform the overlay opens on.

| State | Render | Colour | Tier behavior |
|-------|--------|--------|---------------|
| `CookieStatusRelogin` | `YT: Re-login` / `TW: Re-login`, abbreviated to `YT!` / `TW!` at `tierTight` | Red | Survives to `tierEssential` |
| `CookieStatusFileUnreadable` | `YT: cookies.txt unreadable` / `TW: cookies.txt unreadable`, abbreviated to `YT?` / `TW?` at `tierTight` | `ColorCookies` (the `COOKIES?` fuchsia) | Survives to `tierEssential` |
| `CookieStatusCookiesOnly` | `YT` / `TW` | Red | Survives to `tierEssential` |
| `CookieStatusUnknown` | `YT: Unknown` / `TW: Unknown`, abbreviated to `YT` / `TW` at `tierTight` | Warning | **Dropped** at `tierEssential` |
| `CookieStatusNone` | `YT` (YouTube only) | Yellow | Dropped at `tierEssential` |
| `CookieStatusOK` | `YT` / `TW` | Green | Dropped at `tierEssential` |
| default — Twitch `None`, or any unmapped value on either side | `YT` / `TW` | Dim | Dropped at `tierEssential` |

`CookieStatusUnknown` sits in the dropped group **on purpose**. "The last check could not reach YouTube" is not something the operator can act on; it used to render as the always-visible red `CookiesOnly` alert, so a DNS blip shouted at the volume of a dead session for as long as it lasted. Only a conclusive rejection, a re-login prompt and an unreadable `cookies.txt` survive to the last tier. The unreadable badge is the one alert here that is **not red**: at `tierTight` and below it renders `YT?`, so it differs from `CookiesOnly`’s red `YT` (remedy: re-export) and `Relogin`’s red `YT!` (remedy: sign in again) in the BYTES and not only in the escape sequence — colour is the one distinction a colour-blind operator, a `NO_COLOR` terminal and a pasted screenshot all lose. It borrows `ColorCookies`, already this program’s “a cookie file is the problem” colour, to reinforce the glyph rather than to replace it. Twitch without cookies is ordinary anonymous mode and takes the neutral dim indicator, unlike YouTube's yellow.

**A job parked in `COOKIES?` escalates its own platform's indicator to the surviving red**, ranked immediately below `Relogin` and above every check-derived state. `parkedCookieJobs` attributes the park per platform — an absent `Platform` counts as YouTube, matching every other platform test in the TUI — and deliberately does **not** filter on `ParkReason`: membership parks, auth parks and the pre-v18 zero value all escalate, because in all three the remedy is credentials of some kind. What the red badge means is therefore "a download stopped for want of usable credentials", not "your cookies expired"; the job detail panel carries the difference. A park is evidence from a real download attempt, so it outranks a check that merely asked. The red persists exactly as long as the parked job does — a stale `COOKIES?` job keeps BOTH surfaces' badge red until it is resumed, retried or deleted, by design; if the field finds that noisy, both surfaces change together, never one.

**The reason strings are deliberately absent from this bar.** `cookies.AuthStatus` carries `YouTubeError` / `TwitchError` — why a check reached `Unknown`, or, for Twitch, which of `NoteTwitchAuthLoss`'s five routes marked the platform — the four chat-downgrade routes plus the playback-token route — each a fixed sentence — and they have readers on the two **per-request** paths only: the REST cookie-status payload and the TUI's `R C` result line. This panel is push-driven, fed from `RefreshService.OnAuthChange`, and `authStatusChanged` (`internal/cookies/refresh_auth_status.go`) excludes the two strings from its change-detection gate. That exclusion is a **contract**, not an oversight: no `OnAuthChange`-driven surface may render them, because a reason-only change produces no push and the line would sit stale beside a verdict that is still correct. Widening the gate is the precondition for putting a reason here, and it is a later owner's code change. Until then the operator gets the reason on the next `R C`.

---

## WebSocket Protocol

### Connection Lifecycle

The Web UI establishes a WebSocket connection to the server on page load. The server uses
`github.com/coder/websocket` for WebSocket handling (the library upstream renamed from
`nhooyr.io/websocket`; the import path in `go.mod` is the coder one).

**Upgrade:** The WebSocket upgrade handler is registered as an interceptor on the main HTTP handler. Any request with an `Upgrade: websocket` header is routed to the WebSocket handler regardless of the URL path. Origin validation runs before the handshake and is the same decision `CSRFMiddleware` makes — the same `network_access` policy, the same `X-Forwarded-Host`-from-a-trusted-proxy rule, the same exact port comparison, and the same certificate-SAN requirement on `external`/`public`. An upgrade with no `Origin` header is accepted, which is how non-browser clients connect.

**Authentication:** For external (non-loopback, non-private-network) connections when auth is configured, the `AuthCheck` function validates the upgrade request before accepting. Unauthenticated external WebSocket upgrades are rejected.

**Detached context:** The accepted WebSocket connection uses `context.Background()` rather than the HTTP request context. This prevents the connection from being killed by the server's `ReadTimeout`, which would otherwise close long-lived connections.

**Initial state:** Immediately after connection, the server sends an `initial_state` message containing the full current state: all jobs, buffered log lines (up to 200 from the ring buffer), and monitor check schedule (next check times for Feed, DECAPI, and Twitch monitors). This allows the client to hydrate without making separate REST calls.

**Backpressure and resync:** each client has a `wsWriteQueueSize = 16` frame outbound queue drained by
its own `writePump`. A broadcast that finds the queue full drops the OLDEST frame and enqueues the new
one, so a slow consumer can never stall a fast one — but drop-oldest discards frames nothing later
restates (`job_deleted`, `jobs_update`, `config_update`), which used to leave a lagging tab showing a
ghost row until it reconnected. A drop therefore arms that client's resync flag, and a later frame is
REPLACED by the full `initial_state` snapshot instead of carrying its incremental update —
byte-for-byte what a fresh connection is sent (`initialStateBytes`, `internal/web/websocket.go`), which
`app.js`'s `initial_state` handler applies as a full-state replace.

That replacement is rate-limited to at most one snapshot per second per client (`wsResyncMinInterval`,
`internal/web/websocket.go`): a client that never drains re-arms on EVERY frame, and a snapshot costs a
`GetAllJobs` plus a whole-state marshal, so the unbounded version meant tens of them a second while a
download ran. A frame that arrives inside that window goes out incremental with the flag left armed for
the next one. On a quiet hub the flag would otherwise sit armed indefinitely — nothing is broadcast, so
nothing consumes it — so the 30 s ping tick flushes a pending snapshot on its own (`flushResync`,
`internal/web/websocket.go`), which makes one ping interval the worst-case ghost-row lifetime. A
snapshot that fails to BUILD (a marshal error, or a panic in the state provider, each logged once at
Error) is not retried on the spot; the next drop re-arms the flag and the rate limit holds the retry —
and so that Error line — to one per second. See `resyncSnapshot` in `internal/web/websocket.go`.

Drops are counted per client; the hub logs `WS client lagging` at Debug at most once per
`wsLagLogInterval` (30 s) per client, and the lifetime total once at teardown. The LEVEL is the first
defence rather than the rate limit: Debug is below the default INFO threshold, so on a default install
the line never reaches the app logger's subscriber, which broadcasts each line back to every client —
including the full queue that just dropped, which drops and logs again. The per-client rate limit is
what bounds that loop for an operator who has turned Debug on.

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
| `initial_state` | `{ jobs, logs, nextFeedCheck, nextDecapiCheck, nextTwitchCheck, connectivity, hideFinishedAgeDays, backfill }` | Once, immediately after WebSocket connection is accepted |
| `job_update` | Single job object | When a job changes in any way a progress tick does not: a status transition, an error, a chat-status change, the mux naming its output, a new job, a trim edit. Progress-only ticks take `job_progress` instead. |
| `job_progress` | `{ id, status, progress, percent, speed, eta, lastVideoSeq, lastAudioSeq, totalVideoSeq, totalAudioSeq, totalChatMessages, updatedAt }` | Every progress report of an active download — one per configured progress interval per job, ~60 Hz at the 16 ms default. Carries only the columns the tick writes; the client MERGES it onto the row it already holds (`{...old, ...patch}`) rather than replacing it. Sent when `JobChange.Changes` names progress columns ONLY — anything else, `status` included, goes out as `job_update`. The frame is a tenth of the row it replaces; the cadence is identical (`isProgressOnlyChange` / `newJobProgressFrame`, `cmd/moombox/job_progress.go`). On the client (`case "job_progress"`, `web/public/app.js`) a frame whose `id` the tab does not hold is DROPPED rather than upserted the way `job_update` upserts one — a twelve-field frame would draw an untitled, thumbnail-less row, and the next `job_update` or `jobs_update` carries the whole row anyway — and a frame whose `status` differs from the held one takes `job_update`'s full-render path (re-sort, archive boundary, parked badge) instead of the single-card fast path. |
| `jobs_update` | Full job array | When a job is added or deleted (full list, not incremental) |
| `job_deleted` | `{ id }` | When a job row is removed from the database |
| `config_update` | Partial config (currently `{ hideFinishedAgeDays }`) | When a config setting that affects client-side rendering changes — from the dashboard's own `PUT /api/config` or from a TUI settings save, both through one `broadcastHideFinishedAge` in `cmd/moombox/routes_wiring.go`, which gates on the value actually having moved since the last broadcast, sends this first, and then sends a `jobs_update` re-filtered with the same captured threshold (skipped, with a warning, if the jobs read fails — an empty list would blank every dashboard) |
| `log` | Log line string | When a new log line is emitted |
| `check_timers` | `{ feed, decapi, twitch }` timestamps | When monitor check schedules change |
| `backfill_status` | `{ channel, tab, pages, state }` | Feed-history backfill scan progress per channel (`state`: scanning / error / done / idle). Active scans are also seeded via `initial_state`. |
| `pong` | Empty | Response to client `ping` messages |

### Client-to-Server Message Types

| Type | Payload | Purpose |
|------|---------|---------|
| `ping` | Empty | Client-initiated keepalive. Server responds with `pong`. |

### Broadcast Rate

The WebSocket hub throttles nothing. The only high-frequency caller is `OnJobChange` driven by `ProgressTracker.maybeUpdate`, which is already gated to one report per job per configured progress interval — `progressUpdateInterval` (see `internal/worker/progress.go`) is the 16ms default, overridable per install — and now broadcasts the slim `job_progress` frame; `job_update` carries the state transitions, and every other `UpdateJobFields` caller is event-driven (state transitions, not loops). A previous per-job throttle in the hub created an ordering race — because `BroadcastJobDeleted` is not throttled, the trailing edge could arrive after a delete and resurrect the row via the client's upsert handler.

### Connection Parameters

| Parameter | Value |
|-----------|-------|
| Ping interval | 30 seconds (server-initiated) |
| Client data frame | At least one within 90 s (`wsReadIdleTimeout`), or the server closes the socket — its own pings and the client's pongs do not reset this deadline. The dashboard sends `{"type":"ping"}` every 15 s; any other consumer of the stream must do the same |
| Write timeout | 10 seconds per message |
| Max message size (read limit) | 4 KiB — a client only ever sends `{"type":"ping"}`, and on a `lan` install the upgrade needs no credential |
| Backpressure limit | 16 queued frames per client (`wsWriteQueueSize`); on overflow the oldest is dropped and a later frame is replaced by a full `initial_state` snapshot — at most one per second per client (`wsResyncMinInterval`), flushed by the ping tick if no broadcast comes (`flushResync`) |
| Log ring buffer | None in the hub — the logger owns the only ring (`GetRecentLines`), which `ws_wiring.go` puts in every `initial_state`. A single broadcast line is clipped at 4096 characters (`clipLogLine`, `internal/web/websocket.go`) |

---

## REST API Routes — Complete Catalog

All routes use the `/api/` prefix unless otherwise noted. PO Token routes use bare paths for yt-dlp plugin compatibility.

### Authentication

This is **dashboard** authentication — the operator's password and session. It is unrelated to the platform credentials in §Cookies below, which is why the two never share a payload.

| Method | Path | Rate Limit | Notes |
|--------|------|:----------:|-------|
| `GET` | `/api/auth/status` | — | Public. Returns `{ authRequired, authenticated, hasPassword, passwordlessExternal }` (`AuthRoutes`, `internal/web/routes/auth.go`). `passwordlessExternal` is `network_access` of `external`/`public` with no password hash — a state only a hand-edited config file can produce, and it drives the Web UI's persistent security banner. |
| `POST` | `/api/auth/login` | 5 req / 60s | `{ password }` body, max 128 chars. Sets the session cookie and — when a database is wired — issues a persistent `moombox_client` token cookie, revoking any previous one from the same browser. Returns `{ success: true }`; the token itself is never in the body. |
| `POST` | `/api/auth/logout` | — | Invalidates the session and revokes the presented client token, then clears both cookies. The Web UI's status bar shows a logout icon (`btn-logout`, beside the theme toggle) only while `authRequired && authenticated`; its click is this POST followed by a reload. |
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
| `GET` | `/api/jobs/{id}` | Get a single job by ID, with the staging fields the details dialog's buttons read (`enrichJob`, `internal/web/routes/jobs.go`): `hasStaging`, `hasSegments`, `asides`, `keptChatSidecar` and `unmuxedParts`. |
| `GET` | `/api/jobs/{id}/video` | Stream the job's output video file. Supports HTTP Range requests for seeking. `Cache-Control: private, no-cache` + `Last-Modified` — retry/reinit, incomplete-tail resume and part merges rewrite the file behind the same URL, so it revalidates rather than caching immutably. |
| `GET` | `/api/jobs/{id}/thumbnail` | Serve the locally stored thumbnail so the dashboard stops re-fetching `i.ytimg.com` / `static-cdn.jtvnw.net` for an asset already on disk. `404` when the job has no `thumbnail_file` or the file is gone, and the grid's `onerror` then falls back to the remote `thumbnailUrl`; `403` when the resolved path escapes the output directory. `Cache-Control: public, max-age=86400` — the only long-lived cache in the catalog, because a new thumbnail is a new file at the same path only when the job itself is re-run. |
| `GET` | `/api/jobs/{id}/segments` | List segments for a multi-segment recording. `Cache-Control: private, no-cache` — part merges and incomplete-tail resume rewrite these rows behind the same URL even for a Finished job, same as `/video`. |
| `GET` | `/api/jobs/{id}/segments/{index}/video` | Stream a specific segment's video file. Supports Range. `Cache-Control: private, no-cache` + `Last-Modified` (same revalidation rationale as `/video`). |
| `GET` | `/api/jobs/{id}/segments/{index}/chat` | Get one part's chat file for a multi-segment (Twitch live) recording — the job-level `/chat` only ever covers part 1. Twitch rolls the chat at every part boundary with offsets rebased to that part's recording start; the player fetches each part and shifts by the part's start offset on the global timeline. Same 404/403/422 semantics as `/chat`, plus 404 when the segment has no `chatFile`. |
| `GET` | `/api/jobs/{id}/chat` | Get the chat log file for a job. `Cache-Control: private, no-cache` + `Last-Modified`; answers `If-Modified-Since` with a body-less 304 so a re-selection of the same job doesn't re-read and re-gzip a 50-100 MB file. |
| `GET` | `/api/jobs/{id}/trims` | List trim clips created from this job. |
| `GET` | `/api/jobs/{id}/logs` | Get per-job log lines (worker-level logs specific to this job). |
| `POST` | `/api/jobs` | Create a new job. Rate limited. Body contains URL, format preferences, timestamps. |
| `POST` | `/api/jobs/{id}/cancel` | Cancel an active job. |
| `POST` | `/api/jobs/{id}/retry` | Retry a failed/cancelled job — backward-compatible alias that delegates to `ReinitializeJob`, so it DELETES the staging directory. Allowed from Error / Cancelled / COOKIES? only; a Finished job flagged `incompleteTail` is deliberately refused here, because the redownload would destroy the preserved staging and resume sidecar that flag exists to protect. |
| `POST` | `/api/jobs/{id}/resume` | Resume a YouTube job, PRESERVING its staging files — the counterpart to `/retry` and `/reinitialize`, which delete them. Allowed from Error / Cancelled / COOKIES?, and from Finished when `incompleteTail` is set. `400` when the job is not YouTube, and `400` "No staging files found — use Reinitialize instead" when nothing survives in staging. |
| `POST` | `/api/jobs/{id}/reinitialize` | Reset a job to a fresh state and re-enqueue it, DELETING its staging files. Allowed from Error / Cancelled / COOKIES? only. |
| `POST` | `/api/jobs/{id}/mux` | Force a mux from the segment files already in staging, without re-downloading. Allowed from Error / Cancelled, and from Finished while a split part its finalize could not mux is still in staging (`HasUnmuxedParts`, `internal/worker/staging.go` — the recovery `cleanupStagingAfterMux` names when it keeps that job's staging; `GET /api/jobs/{id}` reports it as `unmuxedParts`, which the details dialog's Mux button follows). `400` for any other status or when staging holds no segment files, `409` while another off-queue operation holds that job's staging directory (the shared claim — a set-aside recovery in flight), `500` for any other refusal from the worker. |
| `POST` | `/api/jobs/{id}/recover-asides` | Mux the recordings the engine set aside on a mid-stream restart into their own files beside the archive, carrying a kept chat capture beside the first of them (unless the job's own finalize already wrote that archive), then reclaim the staging directory — only when it holds no recognised media and no unmuxed part, so a recovery can never delete the recording `/mux` exists to rescue. Its own verb, NOT a widening of `/mux`: an aside overlaps the recording from sequence 0 and can only ever be a sibling, so `/mux`'s `HasSegmentFiles` gate deliberately does not see an aside-only staging directory. `404` for an unknown job; `409` when the job is active, when nothing was ever set aside, or when another off-queue operation already holds that job's staging directory (the claim `MuxJob` and `RecoverAsides` share). The job's status is never written; the run brackets itself with `TrackJobForLogs` so its progress and failures reach the operator through the job's log lines. |
| `POST` | `/api/jobs/{id}/open-folder` | Open the job's output folder in the desktop file manager — Explorer on Windows, `xdg-open` on Linux, through the one switch `OpenPathCommand` (`internal/web/server.go`) that the browser-open path and the TUI's `O F` chord also use — on Windows the command line is force-quoted by `forceQuoteCmdLine` (`internal/web/openpath_windows.go`), because Go quotes only arguments containing a space, a tab or a quote and explorer's legacy parser splits a bare `=`, so an output directory whose path contains one used to open nothing (W R-3), and started through `StartDetached` (`internal/web/server.go`) — Windows releases the process handle (audit Q-6), every other platform reaps the child with `Wait`, which `Release` does not do there. A host with no file manager answers `501` naming the missing program, which the dashboard shows as a toast rather than swallowing. **Loopback only.** |
| `DELETE` | `/api/jobs/{id}` | Delete a job and optionally its files. |

A notification embed's title can carry a `{public_url}/#job=<id>` deep link (`JobDeepLink`, `internal/notifications/mentions.go`) instead of one of the routes above — a browser URL fragment, not an `/api/` call. The dashboard's `_consumeJobHash` (`web/public/app.js`) parses it on load and on `hashchange`, clears the hash immediately via `history.replaceState` (so it never re-opens on a reconnect's second `initial_state`), and shows that job's details from memory or, for a Finished job past the archive boundary, a `GET /api/jobs/{id}` fetch; an id naming no job (a 404) shows a "Job not found" toast instead, while any other failure shows "Could not load job" — a server that could not answer is not the same as an archive that is gone.

### Watch Tracking

Every route below that takes `{id}` in its path 404s with `{ error: "job not found" }` (or a body-less `404` for the beacon-fallback POST) when `id` names no job; the two batch watched routes take a `jobIds` body instead, accept unknown ids silently and always answer `{ success: true }` (`WatchRoutes`, `internal/web/routes/watch.go`).

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/jobs/{id}/watch-state` | Get mutable player state (watched, resumePosition, chatOffset). Uncached — like `/api/jobs/{id}` itself (`Cache-Control: no-cache, must-revalidate`, `jobs.go:247`), not immutably cached. The media and chat file routes revalidate too (`private, no-cache` plus `Last-Modified`, so a repeat request answers `304`); `/api/jobs/{id}/thumbnail` (`jobs.go:399`, `public, max-age=86400`) is the only long-lived cache left. |
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
| `memory` | `{ rss, heapUsed, heapTotal, external }` in MiB (`Sys`, `HeapAlloc`, `HeapSys`, `MSpanSys` / 1048576) plus `goroutines`, a count (`runtime.NumGoroutine`) | `runtime.ReadMemStats` (`internal/web/routes/jobs.go:1426`), assembled into the `memory` map at `jobs.go:1432-1438` |
| `version` | string | `deps.Version` |
| `updateAvailable` | `{ version, tagName, releaseNotes, releaseNotesHtml, publishedAt }` | `SharedUpdateInfo` atomic; absent when no update is pending |
| `disk` | `{ free, total, usedPct, warnLevel }` | `SharedDiskStatus` atomic; absent until the first disk sample |
| `activePlatforms` | `{ youtube, twitch }` booleans | `config.GetActivePlatforms` |
| `cookieStatus` | `{ found, authenticated, verification, youtubeError, fileError }` | `routes.CookieStatusPayload(cookieRefresh.GetStatus())` |
| `twitchAuthStatus` | `{ found, authenticated, verification, twitchError, fileError }` | `routes.TwitchAuthStatusPayload(cookieRefresh.GetStatus())` |
| `autoCookieReloginRequired` | `{ youtube, twitch }` booleans | `AutoCookieService.ReloginStatus()` |
| `nextFeedCheck` / `nextDecapiCheck` / `nextTwitchCheck` | epoch ms | Monitor `GetNextCheckAt` |
| `channelHealth` | `{ youtube, twitch }` — per-channel last check, last error, consecutive failures | Feed and DECAPI health merged per YouTube channel (fresher last-check wins), plus Twitch |
| `botguardSidecar` | `{ healthy, reason, restarts }` | `sidecar.CurrentHealth()`, the package snapshot (read the way `SharedDiskStatus` is, because the producer is `cmd/moombox` and both UIs consume it). **ABSENT — not `false` — when nothing was ever published**, which is what `[bgutils] use_sidecar = false` looks like; the dashboard reads absence as no opinion and only an explicit `healthy: false` paints a warning (`this.sidecarHealthy` is tri-state in `web/public/app.js`). |

The two cookie blocks come from `routes`' own projections rather than being rebuilt here, and that is load-bearing: three hand-written copies of the `cookieStatus` map existed across two packages, and a field added to two of them left this endpoint — the one the dashboard reads on every load and reconnect — quietly serving the old meaning. Their field contract is documented under §Cookies.

`autoCookieReloginRequired` calls `ReloginStatus()` and **not** `GetStatus()`, deliberately: the closure reads nothing but the relogin map, and `GetStatus`'s browser/registry detection scan would otherwise run on every request to this route for a field it never uses. (This route is *not* polled on a timer — see the paragraph below — but it is fetched on every page load, every WebSocket reconnect, every settings save and every completed cookie setup, once per open tab.)

**There is no cookie-status WebSocket event.** The Web UI's cookie state arrives only in responses the page asked for: this endpoint, plus the two manual triggers, which return the same two blocks. `loadStatus()` is not polled on a timer — it runs on page init, on every WebSocket (re)connect, after a settings save, and after an interactive setup finishes or aborts (both the settings dialog's paths and the first-run wizard's). The two manual triggers do not re-fetch it at all: they assign `cookieStatus` / `twitchAuthStatus` / `autoCookieReloginRequired` straight off their own response bodies and call `updateStatusBar()`. Status and reason therefore always arrive together in one fetch, which is why the header badge may render `youtubeError` / `twitchError` in its tooltip while the push-driven TUI status bar may not (see §Status Bar).

### Monitors

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/monitors/check-now` | Force an immediate poll of all three monitors (feed / DECAPI / Twitch); the monitors coalesce a mid-cycle kick through their `pendingKick` latch, so this never stacks cycles. Debounced to one accepted kick per 30 s — a call inside the window returns 200 with `{"success":false,"debounced":true,"retryAfterMs":N}` (`callDebouncer`, `internal/web/routes/debounce.go`). `503` when no trigger is wired. Web: the clickable `#check-countdown` in the status bar. |

### Backfill

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/backfill/rescan` | Force a feed-history backfill re-scan of every configured YouTube channel (same operation as the TUI `R B` chord). Debounced to one accepted run per 30s — a call inside the window returns 200 with `{"success":false,"debounced":true,"retryAfterMs":N}`. Web: the "Re-scan Feed History" button in the Settings → Channels panel (`rescan-feeds-btn`, `settings.js` `rescanFeedHistory`). |

### Configuration

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/config` | Get current configuration. |
| `PUT` | `/api/config` | Update configuration. Triggers config save and may trigger restart. |
| `POST` | `/api/config/channels` | Add or update (by `id`) a monitored channel. A URL-shaped `id` is resolved to the channel's ID first, under the same rate limiter as `/api/resolve-channel` (a plain ID — every enable/disable toggle — is not limited); one that does not resolve is refused, `400` when it is no channel URL and `422` when the lookup fails. An unknown `platform` or `quality_preference`, or an `archive_window_days` / `archive_slots` outside 1–3650 / 1–100, is a `400` naming the field (`config.ChannelOverrideErrors`, the rules `Validate` applies), as is the same entry in `PUT /api/config`'s `channels[]`. |
| `DELETE` | `/api/config/channels/{id}` | Remove a monitored channel. |
| `PUT` | `/api/config/channels/reorder` | Reorder the monitored-channel list. Body `{ ids: [...] }` naming every configured channel exactly once; `400` on a count mismatch, a duplicate id, or an id that names no channel. The new order is saved under the config lock and rolled back if the save fails, then the channel-change callback re-seeds the monitors. |
| `POST` | `/api/resolve-channel` | Resolve a channel URL or name to a canonical channel identifier. |

### Notifications

| Method | Path | Notes |
|--------|------|-------|
| `POST` | `/api/notifications/test` | Synchronously deliver one test embed to `{ url }` — which may be an UNSAVED value, because both the web add dialog and the TUI editor test a staged webhook before committing it. Single attempt, no retries. `400` for a missing or invalid URL (Discord webhooks only — `ValidateURL`, `internal/notifications/manager.go`), `502` on a delivery failure; the error never echoes the URL. |

### Cookies

`CookieRoutes` in `internal/web/routes/cookies.go`. The mechanisms behind these endpoints — the jar, the in-process `RefreshService`, and the `AutoCookieService`'s browser and profile-import paths — are specified in `data-and-storage.md §Cookies`; this section is what reaches the wire and what each UI does with it.

| Method | Path | Rate limit | Notes |
|--------|------|:----------:|-------|
| `POST` | `/api/cookies/recheck` | — | The in-process Go refresh + check (`RefreshService.CheckNow`, then `GetStatus`). Always 200. |
| `POST` | `/api/cookies/auto-refresh` | shared API | `AutoCookieService.RefreshCookiesDetailed` — headless browser when the gate allows one and `cookies.acquisition` is `auto`, otherwise an immediate browser-profile import. Discriminated error codes below. |
| `POST` | `/api/cookies/import` | shared API | Ingest an operator-supplied Netscape cookie file: `text/plain` body or multipart `cookies` part, 512 KiB cap. Merges into `cookies.txt`, reloads the jar, verifies, and returns `cookieSetupOutcome`'s exact key set. 400 empty/no part, 413 over the cap, 415 wrong content type, 422 for the three refusals and for an unreadable existing file, 409 for a failed write. **No GET.** |
| `POST` | `/api/cookies/auto-setup/start` | shared API | Begin interactive setup. **Loopback only** (below). Body `{ platform }`, defaulting to `"youtube"` on an absent or unparseable body and 400 for any other value. Returns `{ success: true }`. |
| `POST` | `/api/cookies/auto-setup/finish` | shared API | `FinishSetupDetailed` — extract, merge, write, then verify. **Loopback only** (below). |
| `POST` | `/api/cookies/auto-setup/cancel` | shared API | Cancel and close the setup browser. **Loopback only** (below). `{ success: true }`, or 404 when there is nothing to cancel. |
| `POST` | `/api/cookies/auto-setup/abandon` | shared API | The dashboard's unload beacon, and deliberately **not** `/cancel`: a user pressing Cancel consents to the setup window closing, a tab unloading does not. Releases the slot without killing the browser. **Loopback only** (below). Returns `{ success, released }`; 404 when there is no setup. |
| `POST` | `/api/auto-cookies/validate-browser-path` | shared API | Validates a user-supplied browser executable (spawns `--version`). 400 on an undecodable body; otherwise 200 with `{ valid: true }` or `{ valid: false, error }` — a rejected path is a *verdict*, not a transport failure, so it does not get an error status. A successful validation calls `InvalidateBrowserDetection()` so the next status poll sees the new browser instead of riding out the 60s detection TTL. |
| `GET` | `/api/cookies/auto-status` | — | `AutoCookieService.GetStatus()`. |

"shared API" is the single per-IP `apiRL` limiter (20 requests / 60s, `rateLimitAPIPerMinute`), shared with job creation. (The archive import at `/api/import` is NOT on it: it builds its own tighter 5/min limiter.) It wraps the endpoints that spawn or steer a browser — `AutoCookieService` already serialises them, but rate-limiting the request flow stops a caller burning CPU on the fast-fail path — and `POST /api/cookies/import`, which spawns nothing and is on it for a different reason: each request rewrites the credential file and makes up to four live auth round-trips.

**All four `/auto-setup/` endpoints are loopback-gated** (`requireLoopbackForBrowserSetup`, `internal/web/routes/cookies.go`), by owner decision. `/start`, `/finish` and `/cancel` open, finish and close a HEADED BROWSER WINDOW ON THE HOST’S SCREEN, and on a `network_access = "lan"` install there is no authentication at all — so any LAN device could put one on a screen its user cannot see, bounded only by `apiRL`. The gate takes the first-run wizard’s shape (an inline `web.IsLoopbackRequest`, answered before the service guard, reading the DIRECT peer — see [security.md](security.md) § What stays on the direct peer address) and answers **403**, deliberately not the wizard’s 401: `app.js` treats any 401 outside `/api/auth/` as an expired session and reloads the page. The refusal names the remedy that works from anywhere, `POST /api/cookies/import`, which stays ungated for exactly this viewer.

`/abandon` is the fourth, and the owner’s sentence named only the other three because this one opens nothing. What it CLOSES is the point: where `setupBrowserGone` cannot answer — a failed job creation or assign on either OS, a Linux process group that could not be adopted, an unreadable `/proc`, darwin, the fallback build — `AbandonSetup` (`internal/cookies/autocookies_setup.go`) releases the slot, so one unauthenticated LAN POST made `SetupInProgress` false and the host’s own `finish` answer 404 with the browser left orphaned. Gating it costs the beacon nothing: `sendBeacon` fires only from a tab whose `_cookieSetupActive` is set, and only a SUCCESSFUL `/start` sets that — so a page with a setup to abandon is a loopback page by construction, on Linux and in Docker as much as on Windows, and a LAN beacon never had anything of its own to release.

`/auto-refresh`, `/import` and the four `/auto-setup/*` endpoints answer `503 auto-cookie service not configured` when no `AutoCookieService` is wired. `/recheck` needs only the `RefreshService`, `/auto-cookies/validate-browser-path` calls a package-level function and needs neither, and `/auto-status` answers a full zero-value body instead (below).

#### The two auth-status payloads

`CookieStatusPayload` and `TwitchAuthStatusPayload` (`internal/web/routes/cookies.go`) are the **only** two projections of `cookies.AuthStatus` onto the wire. `/api/status`, `/api/cookies/recheck` and `/api/cookies/auto-refresh` all render through them.

| Key | YouTube payload | Twitch payload | Meaning |
|-----|-----------------|----------------|---------|
| `found` | `HasYouTubeCookies` | `HasTwitchCookies` | Was this install ever **configured** for the platform — the loose predicate, not "is the cookie set complete". A Twitch session whose `auth-token` was pruned on expiry is a configured session with no credential, which is a different thing to say than "no cookies". |
| `authenticated` | `YouTubeAuthenticated` | `TwitchAuthenticated` | "Can we do authenticated work right now." Unchanged in meaning on purpose — it is the one key a pre-existing frontend reads — and **false on an inconclusive check**. |
| `verification` | `YouTubeVerification.String()` | `TwitchVerification.String()` | What the check **concluded**: `"ok"`, `"failed"` or `"unknown"`. Only `"failed"` is a conclusive negative and only it may be worded as one. |
| `youtubeError` / `twitchError` | `YouTubeError` | `TwitchError` | **Why** the check could not conclude. Empty whenever it did. |
| `fileError` | `CookieFileError` | `CookieFileError` | `cookies.txt` is **present and could not be read**. Platform-independent — one file holds both platforms’ rows — so both payloads carry the same string and either badge can name it. Empty when the file loaded, and empty when there is no file at all: that is never-configured, which `found` already says. |

`verification` is `cookies.RefreshVerdict` rendered through `String()` — never as an ordinal; the enum is an `int` and its field carries `json:"-"` for exactly that reason. `AuthStatus` has no `lastCheck`: the field existed, was written on every pass, was read by nothing, and was removed rather than wired — a timestamp from a pass that may have concluded nothing does not say "the credentials were valid as of this time".

The reason strings answer the half `verification` cannot carry. Without them the UI could say "could not check" and never say what stopped it, so an install behind a captive portal, one being rate-limited, and one behind an intercepting proxy all rendered identically and none named the thing to fix. They are **safe to render** because of a rule at the producers, not at this projection: every string that can reach them names a status code, a scheme+host, a header *name*, a transport error over a constant URL, or one of two static sentinels, and no branch interpolates a response body.

`verification`, `found` (Twitch), `fileError` and the reason strings are all **additive**: an older frontend ignores them and behaves as before, and the current frontend branches *positively* on the strings, so against an older binary that omits them it degrades to the unqualified copy rather than to the hedged one.

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

`POST /api/cookies/auto-setup/start` keeps the static `no supported browser installed` for `ErrNoBrowserFound`, and correctly — `StartSetup` is never gated on `cookies.auto_enabled`, so there the sentinel means exactly one thing. `ErrSetupInProgress` / `ErrRefreshInProgress` → 409; `ErrServiceStopped` → 503, because that one never clears; `ErrUnsupportedPlatform` → 400 verbatim. That last one is a wrong INPUT: `StartSetup` (`internal/cookies/autocookies_setup.go`) accepts `"youtube"` and `"twitch"` and nothing else, refusing before it claims the setup slot — an absent or empty `platform` still means `"youtube"`, which is what both the dashboard and the first-run wizard send. The rule lives there rather than on the route because the TUI’s `E L` chord and the first-run wizard call it directly; before it existed, an unknown value was driven with the YouTube login URL under the wrong `targetPlatform`, so the Chromium finish skipped `cdpEnsurePageTarget` and the wizard judged both platforms as if `youtube` had been asked.

`cause` is a **short stable token**, never the sentinel's message: prose gets reworded, and a frontend branch keyed on prose breaks silently the first time it is. The wording still rides along as `error`, which is the half a human reads. The two tokens must stay distinct because the operator's next move differs — a blocked ladder is a condition on this machine to change (something is holding or intercepting the debugging port), an unanswered read is the browser side having produced nothing at all. **Nothing branches on `cause` today** — the dashboard renders `error` (directly on `/auto-refresh`, through `serverErrorMessage` on `/auto-setup/finish`) and the TUI never sees these responses at all. It is emitted for the machine reader that does not exist yet, and is pinned by `internal/web/routes/cookies_browserread_test.go`.

**`GET /api/cookies/auto-status`** marshals `cookies.AutoCookieStatus`: `setupInProgress`, `browser`, `availableBrowsers`, `configuredBrowserPath` (`omitempty`), `configuredBrowserType` (`omitempty`), `lastRefresh`, `lastError`, `needsManualRelogin`. There is deliberately no `configured` flag — one existed, computed as `profileDir != ""`, could never be false, and was read by nobody.

When no auto-cookie service is wired the handler answers a hand-built object that must match the real one key for key, or this branch teaches the frontend a field the real service never sends. `availableBrowsers` is `[]` and **not** `null`, because `AvailableBrowsers` has no `omitempty` and `DetectBrowsers` never returns a nil slice — the frontend iterates it unconditionally. `configuredBrowserPath` / `configuredBrowserType` are *omitted* here for the mirror-image reason: both carry `omitempty`, so a zero-value `AutoCookieStatus` omits them too. `needsManualRelogin` always carries both supported platforms.

#### What the Web UI renders

| Surface | Reads | Behavior |
|---------|-------|----------|
| Header platform badges | `/api/status`'s `cookieStatus` / `twitchAuthStatus` / `autoCookieReloginRequired`, plus the in-memory job list | `cookieIndicatorState` in `web/public/modules/utils.js`, called from `updateStatusBar` in `app.js` |
| Header warnings (`YT: Re-login` / `TW: Re-login`) | `autoCookieReloginRequired`, then `/api/cookies/auto-status` on the click | Text on desktop, a collapsed `exclamation-triangle` icon on mobile. Clicking asks `reloginPromptTarget` (`web/public/modules/utils.js`) which remedy is actually available to THIS viewer: interactive setup only for a loopback viewer of a host that has a browser, because the wizard opens a login window ON THE HOST — and the cookies panel's import box for everyone else, including every LAN or tunnelled client, every browserless host, and any install whose status could not be read (that panel holds both controls, so the fallback costs a local operator one click). Since 2026-09-17 the server enforces the same rule (`requireLoopbackForBrowserSetup`, `internal/web/routes/cookies.go`), so this predicate no longer STANDS for the gate — it decides which remedy the viewer is offered, one step before the 403 they would otherwise meet. **Known limit:** an SSH local port-forward makes the browser's `window.location.hostname` the local end of the tunnel while the server sees a loopback `RemoteAddr`, so `reloginPromptTarget` answers "wizard" and the server's gate admits it too — a viewer tunnelling in is offered the wizard and the window opens on the host, by construction of the two checks (`reloginPromptTarget`, `web/public/modules/utils.js`; `isLoopback`, `internal/web/middleware.go`), not observed in the field; the import box beside it is the working path |
| Settings → paste/upload cookies (`cookie-import-text`, `cookie-import-file`, `btn-cookie-import`) | `POST /api/cookies/import` | The textarea wins when it has content; otherwise the chosen file goes up as multipart with no explicit `Content-Type` so the browser sets its own boundary. Accepted platforms toast through `cookieSetupAcceptedToast`, a rejection renders inline through `cookieSetupRejectedMessage`, and a non-200 renders the server's own sentence through `serverErrorMessage`. Both controls are cleared on success |
| Refresh-cookies button | — | Plain click → `recheckCookies()`; **shift+click** → `autoCookieRefresh()` |
| Settings → "Refresh cookies from browser profile" button (`btn-import-browser-profile`) | — | Calls `app.autoCookieRefresh()` — the same method and endpoint as shift+click, existing because a modifier key does not exist on a phone |
| Settings auto-cookie panel | `/api/cookies/auto-status` | Browser selector, `Last refresh: …`, and a `Last cookie error: …` line shown only when `lastError` is non-empty |
| Setup dialog | `/auto-setup/finish` | `cookieSetupAcceptedToast` per accepted platform, `cookieSetupRejectedMessage` inline when neither was accepted |

`cookieIndicatorState` decides one platform's badge in a fixed order, and the order is the contract:

1. **re-login required** → red, `<Platform>: Re-login required`. Not gated on `auto_enabled` — "a human must sign in again" is exactly as true, and exactly as actionable, for an install that maintains `cookies.txt` by hand. Do not reintroduce the gate here or at the call site; the TUI has never had one.
2. **a job parked in `COOKIES?` for this platform** → red, `<Platform>: A download stopped for want of usable credentials`. `parkedCookiePlatforms` is the Web half of the TUI's `parkedCookieJobs`, ported deliberately rather than re-derived, with the same per-platform attribution, the same absent-platform-counts-as-YouTube rule, and the same absence of a `ParkReason` filter.
3. `authenticated` → green, `Authenticated`.
4. `fileError` → red, `<Platform>: cookies.txt could not be read (<cause>)`. The file is on the volume and something — a permission, a wrong mount — stopped the read, which the `!found` arm below used to report as "no cookies": the operator was sent back through a cookie setup they had already done. It ranks UNDER `authenticated` on purpose — a jar still doing authenticated work is not a credential problem, whatever a later reload failed to do — and the interpolated string is path-and-cause only (`CookieFileError`, `internal/cookies/refresh_auth_status.go`), built from the path and the failure class rather than copied out of the error, so it can carry no cookie value.
5. `!found` → the platform's absent state, and the asymmetry mirrors the TUI's yellow/dim split: `YouTube: No cookies` is a warning because almost everything Moombox does with YouTube wants them, `Twitch: Anonymous` is the neutral off dot because that is the ordinary mode.
6. `verification === "unknown"` → warning, `Cookies saved — Moombox could not establish whether they work`, with `(reason)` appended from `youtubeError` / `twitchError` when present. The reason is now appended to the **conclusive-refusal** arm too (`Not authenticated (…)`), because TWO producers write `failed` with a reason: the unsignable-jar sentinel (`ErrAuthCheckNotAttempted`, whose cause the old gate silently dropped) on YouTube, and `NoteTwitchAuthLoss` on Twitch, whose reason is one of five fixed sentences — the four chat-downgrade routes plus the playback-token route — naming which broke; that is the only thing distinguishing a missing `login` cookie from a login Twitch refused. `"ok"` still shows nothing, by construction rather than by convention: `verdictFromCheck` returns OK only for a nil error and the reason string IS that error, so the two cannot co-occur. The gate is therefore on the STRING, not the verdict.
7. otherwise → red, `Not authenticated`.

`authenticated` is tested **before** `found`, and the `"unknown"` comparison is positive rather than `!== "ok"`. Both are the additive contract in the other direction: an older binary sends no `verification` and no Twitch `found`, and either inversion would render a healthy session as broken.

The badge is repainted from five job events — `job_update`, `job_progress` (its status-change branch only), `jobs_update`, `initial_state`, `job_deleted` — through `_syncParkedBadge`, which is **change-gated**: the scan over jobs already in memory runs every time (it is cheap and stops at the second platform) and only the DOM write is conditioned, so a progress tick never repaints. `job_deleted` matters because deleting the last parked job is the one gesture that *clears* the escalation. `updateStatusBar` re-computes `parkedCookiePlatforms` fresh rather than reading the memoised value, so the four pre-existing triggers (config load, status load, manual recheck, manual browser refresh) paint the same badge.

The **recheck toast** is worded by `cookieRecheckToast` from the two `verification` fields, filtered to the active platforms — deliberately not from `success`, which is `youtubeAuthenticated || twitchAuthenticated` and therefore false for a check that never reached the site. Its `message` is reproduced character for character from `cookies.RecheckReport` in Go and pinned by a test that runs both; only the Shoelace `variant` is web-only, and it ranks danger (a conclusive failure) over warning (nothing established) over success.

The **browser-refresh toast** has five branches over `ran`, `verdict` and `renewed`, and both un-concluded arms stop short of asserting failure: `!success && ran === false` → neutral, using `cookies.RefreshDeclinedCauses` verbatim; `!success && verdict === "failed"` → danger; `!success` → warning ("ran but could not establish"); `renewed === false` → warning ("cookies still work — but this pass could not confirm the browser refreshed them"); otherwise success. A 404 or 424 is **not** an error here: it is the bottom rung of the ladder, and the dashboard falls through to `recheckCookies()` after toasting `No browser profile found, running a normal cookie refresh instead...`. It branches on the status code, not on the message.

#### What the TUI renders

The TUI's cookie chords do **not** go through these REST endpoints. `OnRecheckCookies`, `OnAutoCookieLastError` and `OnForceRefreshCookies` — and, behind `E L`, the wizard's `OnStartAutoCookie` / `OnFinishAutoCookie` / `OnCancelAutoCookie` (all supplied at `cmd/moombox/tui_wiring.go` and bound onto the wizard in `internal/tui/app.go`) — call `RefreshService` and `AutoCookieService` in-process, so both surfaces exercise the same services but not the same handlers — which is why every shared sentence here is held by an executed test rather than by the transport. The TUI also has no paste/upload affordance for `POST /api/cookies/import`: Arc 11 skipped TUI parity deliberately (spec R7 left it optional) — a multi-kilobyte credential paste into a terminal is a worse path than the file copy a TUI user already has — so the dashboard's Settings → Cookies panel is the only paste/upload surface.

**`R C` — Recheck Cookies.** Never gated by anything. `recheckCookiesCmd` (`internal/tui/app_actions.go`) collects four values — the two verdicts and the two reason strings — plus `AutoCookieStatus.LastError` from a *different service*, and `cookieRecheckFeedback` (`internal/tui/app_update.go`) composes one line:

```
Cookies: YouTube OK, Twitch — could not establish (Twitch: <reason>) | Last cookie error: <lastError>
```

The verdict clause is `cookies.RecheckReport`, shared with the Web toast. A reason is appended for any *active* platform that has one, whatever its verdict — `RefreshUnknown`, and now `RefreshFailed` as well, which is how two producers reach the operator: the YouTube-side unsignable-jar sentinel, and the Twitch mark's five fixed sentences. An `RefreshOK` platform never has one to append. The `LastError` clause is ungated by any verdict and goes last, so the width clamp eats it before it eats the verdicts: it belongs to the auto-cookie service, not to the check this line reports, and it can be non-empty while both verdicts are OK — a browser refresh that has been failing for days behind a `cookies.txt` the 30-minute session refresh is still renewing. This is the TUI's only surface for that fact; it has no auto-cookie status panel, where the Web UI has the settings panel.

**Severity is stated by the composer, never inferred from the finished sentence.** `cookieRecheckFeedback` returns a `feedbackSeverity` beside the line and `feedbackColor` obeys a stated severity outright. The line is clamped to the pane width by `fitFeedback` *before* the colorizer sees it, so at 40 columns `"… | Last cookie err…"` loses the marker the warning branch matched on and a line announcing a recorded failure rendered **green**; at 30 columns `"Cookies: YouTube not authen…"` lost `not authenticated` and a conclusive refusal rendered green too. Each contributing fact raises the severity independently and none can lower it: `RefreshFailed` → error, `RefreshUnknown` → warning, no configured platforms → warning, a non-empty `LastError` → at least warning (never more — what was recorded is a fact about a *previous* pass). Two limits are accepted as written: the clamp runs at compose time, so a terminal made narrower inside the 3 s window re-wraps the line rather than re-clamping it; and the substring scan remains the arbiter for callers that state no severity — correct today because every such caller composes its own unclamped prose — so any future path that composes FOREIGN prose and passes through the clamp must state a severity.

`not authenticated` is **red** on both `R C` and `R F`. Red is the actionable end — the remedy is to re-export credentials — and yellow is reserved for "we could not check", which asks for nothing. A mixed line, one platform refused and the other unreachable, is red: the conclusive half is the half to act on, which is the same precedence the badge and the dashboard toast apply.

**`R F` — Refresh Cookies from Browser.** Wired unconditionally; do not put an `auto_enabled` gate back, in either shape. A nil `OnForceRefreshCookies` does not make the chord inert, it *deletes* it — `dispatchAction`, `buildMenuItems` and the help overlay all test the field — so on an install with the flag off, an operator told their cookies were dead had no key to press and no entry naming one. It is a three-rung ladder; the rungs are chosen inside `RefreshCookiesDetailed` (see `data-and-storage.md §Auto-Cookie Service`) and the TUI only renders the outcome. Five of the seven lines below open with `<mechanism label>` rather than a fixed subject: `internal/tui/app_update.go`'s `cookieForceRefreshResultMsg` arm computes it once as `cookieRefreshMechanismLabel(msg.Result.Mechanism, a.cookieAcquisitionMode())` (`internal/tui/app_actions.go`), which resolves to `Browser cookie refresh` or `Browser-profile cookie import` depending on which source the pass actually used (H2 R9) — never on which mode was merely configured:

| Outcome | Line |
|---------|------|
| `cookies.IsNoBrowserProfile(err)` (rung 3) | `No browser profile found, running R C instead...` — then it dispatches `R C`'s own command, so the sentence leads a real refresh and is replaced by that refresh's report a moment later |
| any other error | `<mechanism label> failed: <err>` |
| `!Ran` | `<mechanism label> declined to run (<RefreshDeclinedCauses>) — nothing was learned about these cookies` |
| `Overall() == RefreshFailed` | `<mechanism label> ran and auth verification failed` |
| `Overall() == RefreshUnknown` | `<mechanism label> ran but could not establish whether these cookies work` |
| `!Renewed` | `Cookies still work, but this pass could not confirm the browser refreshed them` |
| otherwise | `<mechanism label> successful` |

The rung-3 sentence and its Web twin (`No browser profile found, running a normal cookie refresh instead...`) **diverge by design**: each surface names its own affordance for the in-process refresh, and a dashboard user has no `R C` to press. Both are pinned exactly, and their difference asserted, by `TestRungThreeSentencesDivergeByDesign`.

**`E L` — Cookie Login.** The TUI's entrance to the interactive browser login, and the answer to a `Re-login` badge. It opens `SetupWizardModel` at its cookie step in cookie-only mode (`OpenCookieLogin`, `internal/tui/setup_wizard.go`) — the *same* state machine the first-run wizard drives, not a second one: `OnStartAutoCookie` → `AutoCookieService.StartSetup`, the 300 s countdown (`cookieSetupCountdownSeconds`), `Enter` → `OnFinishAutoCookie` → `FinishSetupDetailed` under `cmd/moombox`'s 60 s ctx, and the same four-arm verdict rendering (`case setupCookieFinishMsg`, `internal/tui/app_update.go`) that distinguishes *accepted* from *verified* — and all four arms render INSIDE the overlay: the error and rejected arms on `errorMsg`, the accepted arm on `successMsg` (`SetupWizardModel`, `internal/tui/setup_wizard.go`), drawn in `SuccessStyle` exactly where `errorMsg` is drawn, in both `viewSimpleCookies` and `viewAdvancedCookies`; the finish arm (`case setupCookieFinishMsg`) writes the pair in one statement so the two are never shown together, and `HandleKey` clears both on the next keypress. The accepted verdict ALSO goes to the App feedback line (`setFeedback`, 3 s), on purpose: `View` (`internal/tui/app_layout.go`) returns the wizard alone while it is visible, so the overlay's own line is what the operator reads while it stands and the feedback line is what they read if they close it at once, and the two are cleared independently (`TestSetupCookieVerdictOutlivesFeedbackLineExpiry`). Before H2 R8 the accepted arm reached only the feedback line, so a terminal operator saw the ✓ on the platform row and nothing else (12a arc-close F4). What cookie-only mode changes is the two exits: `Esc` at the picker and the third list row (`Close`, not `Skip / Next`) close the overlay rather than walking into the first-run channel editor, whose `Tab` would rewrite `config.toml` on a configured install. **Every exit funnels through `closeCookieLogin`, which cancels an in-flight setup first**, so an abandoned overlay releases the acquisition slot instead of leaving it for the server-side reap; `Esc` while the browser is open cancels and returns to the picker rather than closing. The chord is gated on the callback exactly as `R F` is — a nil callback deletes a chord rather than making it inert — and `cmd/moombox` binds it unconditionally, so `E L` exists with `cookies.auto_enabled` off: `StartSetup` is acquisition and is never gated. With no interactive-setup callback at all the chord does not exist: `E L` reports `Invalid Chord: E L` like any unregistered pair and is absent from the menu and from help (the `dispatchAction` nil-guard behind it is defensive and unreachable from the keyboard); `StartSetup`'s own refusals — stopped service, a setup or refresh already running, no supported browser — arrive on the operator's `Enter` and render inline in the wizard, where the dashboard puts them too.

#### Restart-required cookie settings

Four cookie keys are labelled restart-required in **both** settings UIs — `cookie_file`, `refresh_interval`, `auto_enabled`, `browser_profile_dir`. The Web UI inserts a `Restart` badge after the named element (`RESTART_REQUIRED_FIELDS` in `web/public/modules/settings.js`) and offers a restart on save; the TUI colours the change marker yellow instead of green for these keys (`restartRequiredKeys` in `internal/tui/settings.go`, rendered in `settings_view.go`). The two lists are pinned against each other by `TestRestartRequiredListsAgree`. What `auto_enabled` does **not** need a restart for is the manual triggers: `R F` and the dashboard's shift+click read it live. See `data-and-storage.md §[cookies]` for why the four are restart-required at all.

The same two lists carry every other restart-required key — `port`, `network_access`, `https_enabled`, `tls_cert_path`, `tls_key_path`, `database_path`, `log_file_path`, `log_max_file_size`, `log_max_files`, and (since Arc B of the 2026-09-04 improvement chain) `connectivity.probe_targets`, `memory.sidecar_hard_limit_mb`, `bgutils.use_sidecar` — sixteen in all; both restart prompts name the categories: port, network access, connectivity probe targets, database path, log settings, cookie settings, sidecar settings.

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
| `GET` | `/api/files/orphaned` | List orphaned output files — named by no job's row, and not being written by a running finalize or part mux — and staging directories no active job needs; an Error or Cancelled job's staging is listed with that job's title and status. |
| `DELETE` | `/api/files/orphaned` | Body `{"paths": [...]}`. Each path must lie under the staging or output directory (canonical spellings compared on both sides) and is refused while an active job owns it — by its row, by its staging directory, or because a running finalize is still writing it — matched in the configured spelling and the canonical one. Answers `{deleted, errors}`, the errors naming each refused path. |

### History

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/history/orphaned` | List processing-history rows with no matching job (job deleted, or the video was skipped and never jobbed). While the row remains, the monitor treats the video as already-processed and won't re-discover it. Keyed by job ID, so the match is against `jobs.id`. |
| `DELETE` | `/api/history/orphaned` | Remove the given history video IDs (JSON body `{"videoIds":[...]}`), unblocking re-discovery. |

### Updates

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/update/status` | Get current update status (available version, if any). |
| `GET` | `/api/update/release-notes` | Fetch release notes for a version. Query param `version=X.Y.Z` (validated against `^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$` — the pre-release suffix is there because `release.yml` preserves `-rc.N`/`-test.N` into the running version and the Web sends no `version=`, so such a build used to `400` its own notes; anything else is a `400`), defaulting to the current version. Returns sanitized HTML rendered from the GitHub release body via goldmark + bluemonday (download-link section stripped). Used by the Web UI "View Release Notes" button. |
| `POST` | `/api/update/check` | Manually check for updates. Debounced to one accepted check per 30 s — every call spends one of GitHub's 60/h unauthenticated requests — and a call inside the window returns 200 with `{"success":false,"debounced":true,"retryAfterMs":N}`, the same shape `/api/monitors/check-now` and `/api/backfill/rescan` use (`callDebouncer`, `internal/web/routes/debounce.go`). The 30 s window bounds a held key, not a scripted caller (≤ 120 accepted checks/h against GitHub's 60/h); the scheduled auto-check calls the updater in-process and is never gated. |
| `POST` | `/api/update/apply` | Download and apply an available update. Triggers restart. Loopback only: the direct peer must be loopback AND the Origin (if any) must name `localhost` / `127.0.0.1` / `::1` (`updateApplyOriginAllowed`) — the Origin alone is the client's to choose. |
| `POST` | `/api/update/verify` | Verify the Ed25519 signature of the current binary. |
| `POST` | `/api/update/dismiss` | Dismiss the update notification. Body shared with the TUI via `DismissUpdate`. |

### FFmpeg

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/ffmpeg/check` | Check if FFmpeg is on PATH and return version info. |
| `GET` | `/api/ffmpeg/install-suggestion` | Returns the distro-appropriate package manager command for FFmpeg installation (e.g., `apt install ffmpeg`, `dnf install ffmpeg`, `pacman -S ffmpeg`). Linux only; returns empty on Windows. |
| `POST` | `/api/ffmpeg/check` | Validate a specific FFmpeg path (`{ path }`) and, when it answers `-version`, persist it to `paths.ffmpeg_path` **and** re-apply it to the live trim service and download orchestrator through `FFmpegDeps.OnFfmpegPathChange` (`internal/web/routes/ffmpeg.go`). The post-boot FFmpeg overlay saves and resumes with no restart, so without that hot reload every mux and trim would keep the failing boot value. A path `PUT /api/config` would refuse — one carrying a `..` segment — is refused here too, with the same message and before anything is executed: answering `-version` is not a superset of that string rule, and `validateConfigUpdates` has no grandfather clause for paths, so a stored one made every later full-form save `400`. Rate limited. |
| `GET` | `/api/ffmpeg/install-options` | Get available FFmpeg installation options (download sources). |
| `POST` | `/api/ffmpeg/install` | Begin FFmpeg download/installation. Rate limited. |
| `POST` | `/api/ffmpeg/install/confirm` | Confirm FFmpeg installation to a specific location. Rate limited. |
| `POST` | `/api/ffmpeg/install/reject` | Reject/cancel FFmpeg installation. Rate limited. |

### Statistics

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/stats` | Aggregate statistics: job counts by status, total sizes, durations, disk usage. Derivations shared with the TUI via `stats.Build` (`internal/stats`). |

### yt-dlp Plugin

| Method | Path | Notes |
|--------|------|-------|
| `GET` | `/api/ytdlp-plugin/status` | Check if the yt-dlp PO token plugin is installed. The computation lives in `Status` (`internal/ytdlpplugin/ytdlpplugin.go`), a stdlib-only package outside the HTTP layer so the TUI's `E Y` overlay can share the answer without importing `routes`; `YtdlpPluginStatus` (`internal/web/routes/ytdlp.go`) is the shim this route calls, and its `YtdlpPluginInfo` alias's struct tags are the eight-key wire contract `settings.js` `loadYtdlpPluginStatus` reads. `installedPort` is `null` until a plugin is installed, and stays `null` if the installed file's URL line does not parse — that state is the eighth key, `unparseable`, which the dashboard's card renders as an "Unrecognized file" badge plus an alert and the `E Y` overlay as its own row, because a file with no readable URL line is installed but unusable. |
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
| `GET` | `/minter_cache` | Loopback only | List cached minter keys (diagnostic). The keys name internal PO token cache entries, so the route carries the same `LoopbackOnly` gate as the rest of the block (audit Q-23) even though only `/ping` has to stay public for plugin discovery. |
| `GET` | `/pot_stats` | Loopback only | PO token observability counters — session/minter cache hits, minter create/evict/invalidate counts, generate errors, inflight waits, and the current cached-minter count. Monotonic; operators sample and diff externally. |

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
| Re-login required | `YT: Re-login` / `TW: Re-login` in the warnings area, clickable to start setup | Folded into the platform indicator as `YT: Re-login` / `YT!`, and at `tierFull` followed by `(E L)` — the chord that opens the same interactive setup the dashboard's click does |
| Update indicator | New version badge | New version indicator |
| BotGuard sidecar down | Warnings-list item `PO tokens: sidecar down`, from `/api/status`'s `botguardSidecar.healthy === false`. An **alert, not a status**, and action-less: the supervisor is already retrying, so it is a plain span rather than a control (see the keyboard-reachability paragraph below) | Red `SIDECAR DOWN` in the status bar, `POT` from `tierTight` up (`internal/tui/status_bar.go`). Fed by push — `sidecar.CurrentHealth()` seeds it before `Run` and `sidecar.SubscribeHealth` keeps it current (`cmd/moombox/tui_wiring.go`) — so the TUI needs no `/api/status` fetch |

**Keyboard reachability (Web).** The status bar's clickable non-controls — the check countdown, the `YT: Re-login` / `TW: Re-login` warnings, the collapsed warnings icon and the version indicator — plus the log panel's resume-auto-scroll pill are all `role="button" tabindex="0"` and answer `Enter` and `Space` through the same handler their click uses, never a second copy of it; `Space` is `preventDefault()`ed so it activates the control instead of scrolling the page under the user. The glyph-only two — the warnings icon and the version indicator — take their accessible name from the title they already carry, kept in step as the title changes. The action-less `PO tokens: sidecar down` warning is deliberately none of this: it is a statement rather than a button, so it stays a plain span, is not a Tab stop, and leaves `Enter` and `Space` to the browser — and the collapsed warnings icon, which stands in for whichever warning is first, drops its own `role` and `tabindex` for as long as that warning is the action-less one, so it is never a Tab stop that does nothing. Because a control consumes the key by `preventDefault()` rather than by stopping propagation (the warning spans' handler is delegated on their container, so the event has to keep bubbling), the dashboard's global keydown shortcuts return early on an already-defaulted event: `Enter` on a status-bar control activates that control and does not also open the focused job's details dialog.

**Cookie parity, and where it stops.** The two indicators agree on the facts that matter and are held to that by shared code and by tests, not by convention:

| Property | Status |
|----------|--------|
| Escalation order | **Same.** Both rank re-login first and a parked `COOKIES?` job second, above every check-derived state, for the same stated reason: a park is evidence from a real download attempt and outranks a check that merely asked. Below that rank the check-derived states are mutually exclusive by construction, so the two branch orders are not observably different. |
| Parked-job attribution | **Same.** `parkedCookiePlatforms` (`web/public/modules/utils.js`) is a deliberate port of `parkedCookieJobs` (`internal/tui/status_bar.go`), down to the absent-platform rule and the absence of a `ParkReason` filter. The Web side was knowingly divergent before that port — the TUI reflected parked jobs and the Web did not. |
| Re-login gating | **Same, and both ungated.** Neither surface conditions the re-login prompt on `cookies.auto_enabled`. The dashboard used to, in all three places it surfaces the state, and the TUI never has; removing the Web gate is what brought them into step. A manual-cookie install is the audience least able to discover the state any other way. |
| Manual recheck wording | **Same sentence.** `cookies.RecheckReport` is the Go authority and the Web copy is reproduced character for character, pinned by a test that executes both. |
| Manual refresh gesture | **Same gesture, different affordances.** The TUI's `R F` and the dashboard's shift+click (and the Settings page's "Refresh cookies from browser profile" button, which calls the same method) run the same `RefreshCookiesDetailed` ladder — the TUI in-process, the dashboard over `POST /api/cookies/auto-refresh`. Their rung-3 sentences differ **by design** and are pinned apart by `TestRungThreeSentencesDivergeByDesign`. |
| Interactive login | **Same operation, different affordances — and one question only the dashboard has to ask.** Both surfaces drive `StartSetup` / `FinishSetupDetailed` / `CancelSetup`: the TUI in-process through the wizard's three callbacks, the dashboard over the `/auto-setup/*` trio. The dashboard must decide whether *this viewer* may open a browser window on the host (`reloginPromptTarget`, and the import box for everyone else); a TUI session is the host, so `E L` has nothing to route. |
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
| `COOKIES?` | Authentication required but cookies are missing or expired | Warning color (yellow/orange), actionable prompt; the TUI labels it `Auth Required` (`StatusLabel`, `internal/tui/styles.go`), as the Web UI does |

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
| Statistics | `modules/stats.js` | `StatsDialogModel` (`internal/tui/stats_dialog.go`) |
| Zip import | `modules/imports.js` | `import_dialog.go` |
| Cookie import | `modules/settings.js` import panel | `CookieImportDialogModel` (`internal/tui/cookie_import_dialog.go`) |
| Orphaned files | `modules/files.js` | `files_dialog.go` |
| Client tokens | `app.js` (inline) | `client_tokens_dialog.go` |
| yt-dlp plugin | Settings → Integrations card (`settings.js` `loadYtdlpPluginStatus`) | `YtdlpDialogModel` (`internal/tui/ytdlp_dialog.go`) |
| Copy stream URL (job details) | `app.js` details dialog, `streamUrl` in `web/public/modules/utils.js` | `O C` chord (`streamURL`, `internal/tui/app_actions.go`); the browser has a real clipboard API, the terminal has OSC 52 plus a local-Windows `clip.exe` backup, so only the TUI side hedges its wording |
| Filter language | `filter-parser.js` / `filter-engine.js` | `internal/jobfilter` behind the Tasks panel's `/` box |

**Note on video playback:** The TUI cannot play video inline (it is a terminal). The `O W` chord opens the Web UI in the default browser, where the user can access the player. This is the intended design — video playback is a Web UI strength, and the TUI defers to it rather than attempting a degraded experience.

---

## HTTP Server Middleware Stack

The middleware is applied in this exact order (`NewServer` in `server.go`). Order matters — each middleware wraps the next, so the first listed is the outermost. Two housekeeping layers come first: chi's **RequestID** (so recovery and log lines can be correlated to a request) and **DrainMiddleware** (answers 503 once `StartDrain` is called, placed ahead of recovery so a panic in a later middleware cannot disturb the shutdown path). Then:

1. **RecoveryMiddleware** — Catches panics in any handler. Logs the stack trace. Returns 500 to the client. Prevents a single request from crashing the server.
2. **CORSMiddleware** — Handles Cross-Origin Resource Sharing headers based on configuration.
3. **SecurityHeaders** — Sets CSP, X-Content-Type-Options, X-Frame-Options, and other security headers on every response.
4. **CSRFMiddleware** — Validates Origin/Referer headers on state-changing requests (POST, PUT, DELETE). Requests with a valid `X-Internal-Token` header bypass this check (TUI path).
5. **IPGateMiddleware** — IP-based access control. Enforces network access level (loopback only, LAN only, or public).
6. **MaxBodySize** — Default 1 MB body limit. Individual routes (e.g., import at 500 MB) can override.
7. **CompressionMiddleware** — Gzip compression for responses. Skips WebSocket upgrades and responses that should not be compressed (video streams, already-compressed content).
8. **AuthMiddleware** — Added last, by `initServices` in `cmd/moombox/services.go` once the AuthService exists (`s.r.Use(webServer.AuthMiddleware)`). Enforces the session/client-token check for external clients; see security.md for the full chain.

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
- **`internal/tui/`** — All TUI source files (43 non-test files, ~22,700 lines; see §Source Files).
- **`web/public/`** — All static web assets (HTML, JS, CSS).
- **`web/embed.go`** — `go:embed` directive for static assets.
