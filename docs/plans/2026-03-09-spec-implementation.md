# SPEC.md Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Create a comprehensive SPEC.md and 9 supporting deep-dive docs, then reduce CLAUDE.md to control prompts only.

**Architecture:** SPEC.md is a standalone ~800-1500 line AI-first reference document. Each of its 8 sections has a companion deep-dive doc in `docs/spec/`. A metrics appendix holds volatile numbers. CLAUDE.md is then stripped to working-style instructions, build commands, critical patterns, and pointers to SPEC.md.

**Tech Stack:** Markdown documentation only. No code changes.

---

### Task 1: Create `docs/spec/` directory structure

**Files:**
- Create: `docs/spec/` directory

**Step 1: Create directory**

Run: `mkdir -p docs/spec`

**Step 2: Commit**

```bash
git add docs/spec
git commit -m "chore: create docs/spec directory for project specification"
```

---

### Task 2: Write `docs/spec/vision-and-purpose.md`

**Files:**
- Create: `docs/spec/vision-and-purpose.md`

**Content covers:**
- Scope: What Moombox is — a personal tool built to product quality
- Rules: Single binary, Windows-only, standalone (not a wrapper), 24/7 archival appliance
- Body: Origin (personal need for reliable stream archiving), target user (owner first, then anyone wanting set-and-forget archiving), what it does (monitors → detects → downloads → muxes → serves), what it is NOT (not a general downloader, not a yt-dlp wrapper, not multi-platform by default)
- Cross-references: `design-philosophy.md`, `architecture.md`

**Step 1: Write the doc**

Use the frontmatter format: Scope (2-3 sentences), Rules/Constraints (bullets), Body (free-form), Cross-references.

**Step 2: Commit**

```bash
git add docs/spec/vision-and-purpose.md
git commit -m "docs: add vision-and-purpose spec deep-dive"
```

---

### Task 3: Write `docs/spec/design-philosophy.md`

**Files:**
- Create: `docs/spec/design-philosophy.md`

**Content covers:**
- Scope: Design priorities, constraints, and code principles
- Rules: Priority ordering (correctness > reliability > efficiency > simple deployment & UX > polish > completeness > performance), Windows-only, no CGo, resource efficiency
- Body:
  - Priority ordering with explanations for each level
  - Code complexity principles: acceptable when solution demands it, match solution complexity not problem complexity, contain behind clean interfaces, simple solutions to complex problems are ideal
  - Deployment simplicity: single binary, no external dependencies except FFmpeg
  - UX philosophy: intuitive for non-technical users, advanced controls for power users, sensible defaults
  - Dual UI: Web UI + TUI both first-class, parity with platform strengths (TUI gets real-time + keyboard, Web gets rich media + visual dashboards)
  - Resource efficiency: runs 24/7, minimize CPU/IO/network/RAM, Goja VMs auto-evict, signal-driven concurrency over polling
  - Error philosophy: never crash, degrade gracefully, always inform user, silent failures are as bad as crashes
  - Upstream relationship: informed reimplementation, tracks yt-dlp/BgUtils/ejs for awareness, adapts independently
- Cross-references: `vision-and-purpose.md`, `architecture.md`

**Step 1: Write the doc**

**Step 2: Commit**

```bash
git add docs/spec/design-philosophy.md
git commit -m "docs: add design-philosophy spec deep-dive"
```

---

### Task 4: Write `docs/spec/architecture.md`

**Files:**
- Create: `docs/spec/architecture.md`

**Content covers:**
- Scope: Process model, package structure, data flow, concurrency patterns, service wiring
- Rules:
  - Launcher/supervisor pattern via `_MOOMBOX_CHILD` env var (exit code 42 = restart)
  - All goroutines must have panic recovery
  - Logger interface is anonymous per-struct (never extract to named interface)
  - Database partial updates via `UpdateJobFields()` with dynamic SET clauses
  - Callback closures for cross-cutting service wiring
- Body:
  - Process model: launcher vs child, triggerRestart, shutdown (10s force timer), subcommands (add, --version, --headless, --log-level)
  - Service initialization order (all 19 services in sequence)
  - Package dependency graph with descriptions (no line counts — those go in appendix)
  - Key data flow: Monitors → DB jobs → StreamProcessor → DownloadOrchestrator → SegmentDownloader + ChatDownloader → FFmpeg Muxer → Output
  - UI data flow: Database pub/sub → WebSocket broadcasts → Web UI + TUI
  - Concurrency patterns (with parameters and rationale):
    - Worker: 100 lifecycle slots + configurable download semaphore (default 2)
    - WebSocket: 100ms leading/trailing edge throttle
    - Database: 100ms signal-driven batch coalescing
    - TUI: non-blocking channel sends with drop counters, log batching (250ms flush)
    - BotGuard: triple cache (session 6h → minter dynamic TTL → inflight dedup)
    - Cipher: 3-VM LRU keyed by player.js URL
  - Panic recovery patterns: inline defer/recover in goroutines, RecoveryMiddleware for HTTP, safeCall wrappers for DB callbacks
  - Job status lifecycle: Upcoming → Live → Downloading → Muxing → Finished, error paths
  - Download orchestration detail: StreamProcessor (probe/wait/auth upgrade), DownloadOrchestrator (full lifecycle + background mux), SegmentDownloader (DASH sequential, HLS polling, VOD parallel, catch-up at 6 parallel), QualityMonitor (30s probes, split on resolution change), verification loop (up to 6 checks)
- Cross-references: `platform-services.md`, `data-and-storage.md`, `security.md`, source files

**Step 1: Write the doc**

**Step 2: Commit**

```bash
git add docs/spec/architecture.md
git commit -m "docs: add architecture spec deep-dive"
```

---

### Task 5: Write `docs/spec/platform-services.md`

**Files:**
- Create: `docs/spec/platform-services.md`

**Content covers:**
- Scope: YouTube, Twitch, BotGuard, and Cipher subsystem integrations
- Rules:
  - YouTube uses multi-client Innertube fallback (TV_DOWNGRADED → WEB → WEB_CREATOR → ANDROID_VR)
  - Format priority: resolution > FPS > codec > bitrate > auth level
  - Twitch uses GQL API with persisted query hashing
  - BotGuard has triple cache with auto-eviction
  - Cipher has 3-VM LRU with AST + regex fallback
  - All API keys and client configs live in `internal/constants/`
- Body:
  - **YouTube Service:**
    - PlayerAPI multi-client strategy with auth levels (0-5)
    - Watch page parsing (visitor data, player URL, session data)
    - Format selector algorithm (video: resolution > FPS > codec > bitrate > auth; audio: codec > bitrate > auth)
    - Codec scores (video: vp9.2=5, vp9=4, av01=3, avc1=2; audio: opus=4, mp4a.40.5=3, mp4a.40.2=2)
    - N-parameter decryption (throttling countermeasure)
    - Stream status classification (Live, Upcoming, VOD, PostLive, NotAStream)
    - Retry strategy: 4 attempts, exponential backoff (1s, 2s, 4s)
  - **Twitch Service:**
    - GQL API with persisted queries (SHA256 hashing)
    - HLS variant selection (quality preference → resolution → bandwidth)
    - IRC chat (WebSocket to irc-ws.chat.twitch.tv, anonymous nick, capabilities)
    - VOD chat (GQL pagination by offset)
    - Emote resolution (BTTV, FFZ, 7TV) with LRU cache (200 channels)
  - **BotGuard/PO Token:**
    - Flow: challenge → interpreter fetch → Goja VM execution → snapshot → integrity token → mint
    - Triple cache: session (6h) → minter (dynamic TTL, auto-evict via time.AfterFunc) → inflight dedup
    - Fallback endpoints: googleapis.com + youtube.com variants
    - Timeouts: BG load 10s, snapshot 30s, mint 3s
  - **Cipher Solver:**
    - Dual approach: AST parsing + regex fallback
    - Disk cache: ~/.cache/yt-cipher/player_cache/ (14-day TTL, SHA256 key)
    - Memory cache: 3-VM LRU (mutex-serialized compilation to prevent thundering herd)
    - Decrypts both signature (s param) and n-parameter (throttle countermeasure)
  - **Goja Runtime Shims:**
    - Minimal DOM (document, navigator, window, canvas, storage, crypto, performance)
    - TextEncoder/TextDecoder (inline JS, UTF-8)
    - Timer support (setTimeout/setInterval via goroutines, CancelAll on VM exit)
  - **YouTube Live Chat:**
    - Polling endpoint (live_chat/get_live_chat or get_live_chat_replay)
    - Message types: text, super chat, super sticker, membership
    - Continuation token lifecycle (watch page → API responses → stale recovery)
    - All Chat upgrade (Top Chat → Live Chat on first response)
    - Dedup: 5000 recent IDs, deterministic eviction
    - Resume state: sidecar .resume.json
    - Incremental file append (truncate + append pattern)
    - Error limits: live=20, VOD=5 consecutive
  - Caching strategy summary table
- Cross-references: `architecture.md`, `data-and-storage.md`, source files per subsystem

**Step 1: Write the doc**

**Step 2: Commit**

```bash
git add docs/spec/platform-services.md
git commit -m "docs: add platform-services spec deep-dive"
```

---

### Task 6: Write `docs/spec/user-interfaces.md`

**Files:**
- Create: `docs/spec/user-interfaces.md`

**Content covers:**
- Scope: Web UI and TUI architecture, shared patterns, parity rules
- Rules:
  - Both UIs are first-class — neither is secondary
  - Parity with platform strengths (TUI=real-time+keyboard, Web=rich media+dashboards)
  - TUI uses Charm ecosystem (bubbletea + bubbles + huh + lipgloss) — always check Charm repos first
  - Web UI uses Shoelace v2.16 CDN, vanilla JS, go:embed
  - WebSocket is the real-time sync mechanism for both
  - TUI communicates with backend via HTTP + X-Internal-Token
- Body:
  - **Web UI:**
    - SPA architecture: vanilla JS, no framework, Shoelace v2.16 CDN
    - File layout (app.js, modules/*, moombox.css, index.html, login.html)
    - Module loading pattern (setup, player, settings, trimmer, stats, imports, utils)
    - WebSocket client: connects to ws://host/, handles job_update, jobs_update, log, check_timers, initial_state
    - Mobile breakpoints: 992px (tablet), 768px (phone), hover:none (touch)
    - Static assets embedded via go:embed in web/embed.go — changes require go build
  - **TUI:**
    - 3-panel layout: TaskList (~33%), JobDetails (~50%), Logs (~17%)
    - Focus management: Tab/Shift-Tab cycle, mouse click
    - Chord system: buildMenuItems() = single source of truth, dispatchAction() = unified handler
    - Chord prefixes: A (Action), R (Request), O (Open), Q (Quit)
    - Single keys: F (Filter), M (Menu), ` (Settings), ? (Help)
    - Confirm chords: third keypress within 3s for destructive actions
    - Full chord catalog
    - Overlays: Help, ActionMenu, AddVideo, Import, Trim, Files, ClientTokens, Settings, SetupWizard, FFmpegCheck
    - Async message types: JobUpdateMsg, JobsUpdateMsg, LogBatchMsg, CheckTimersMsg, CookieStatusMsg, DiskStatusMsg, UpdateStatusMsg
    - Tick intervals: 1s main, 16ms progress (active), 500ms progress (idle), 250ms log flush, 150ms marquee
    - Non-blocking channel sends with drop counters
    - Backend communication: HTTP to localhost with custom RoundTripper adding X-Internal-Token
  - **Shared patterns:**
    - WebSocket protocol (message types, throttling)
    - API route prefix: /api/ (no version)
    - Status bar elements (connection, disk, monitors, cookies, updates)
    - First-run setup wizard (FFmpeg, cookies) in both UIs
- Cross-references: `architecture.md`, `security.md`, source files

**Step 1: Write the doc**

**Step 2: Commit**

```bash
git add docs/spec/user-interfaces.md
git commit -m "docs: add user-interfaces spec deep-dive"
```

---

### Task 7: Write `docs/spec/data-and-storage.md`

**Files:**
- Create: `docs/spec/data-and-storage.md`

**Content covers:**
- Scope: Database, config, cookies, file output patterns
- Rules:
  - SQLite with WAL mode, 1 connection, 5s busy timeout, foreign keys on
  - Database partial updates via UpdateJobFields() with dynamic SET clauses
  - Job status is `type JobStatus string`, timestamps are ISO 8601 strings, optional numerics use pointers
  - Config migrations are non-destructive (only apply when new section doesn't exist)
  - FlexDuration parses config values as minutes/days (context-dependent)
- Body:
  - **Database:**
    - Connection config: WAL mode, 1 open/1 idle, 5s busy timeout, foreign keys
    - Batch update coalescing: 100ms window, single transaction flush, signal-driven (zero IO when idle)
    - Pub/sub: OnJobUpdate (per-job after batch flush), OnJobsChange (full list), panic-safe wrappers
    - fieldToColumn map: 55 fields tracked for dynamic SET clause generation
    - Migration approach: versioned, idempotent, forward-only
    - Schema design: jobs (with all status/progress/format fields), segments, client_tokens
    - Job log buffers: per-job in-memory string arrays
  - **Config:**
    - TOML format, search order (--config flag, ./config.toml, ./config/, ~/.config/moombox/)
    - Main sections: network, paths, logs, monitors, downloader, cookies, disk, updates, channels
    - FlexDuration: parses int as minutes or days (context-dependent), stores as int
    - Migration system: migrateOldFormat() handles legacy flat→structured, non-destructive
    - Auto-hash: plaintext passwords in config auto-hashed to scrypt on startup
    - Output template: `${channel}/${start_date} ${title} [${id}]`
  - **Cookies:**
    - Netscape format file (cookies.txt)
    - Platform detection: YouTube (ytimg, youtube, youtubeapis), Twitch (twitch.tv, ttvnw.net)
    - Refresh service: periodic validation via API calls (6h default)
    - Auto-cookie: browser-based extraction (Firefox, Chromium)
    - Browser detection: Windows registry + common paths
    - Chromium DPAPI decryption (Windows-specific)
  - **File output:**
    - Staging directory for in-progress downloads
    - Output template for final file placement
    - Resume state: .resume.json sidecar files
    - Chat: JSON format with incremental append pattern
- Cross-references: `architecture.md`, `security.md`, `operations.md`

**Step 1: Write the doc**

**Step 2: Commit**

```bash
git add docs/spec/data-and-storage.md
git commit -m "docs: add data-and-storage spec deep-dive"
```

---

### Task 8: Write `docs/spec/security.md`

**Files:**
- Create: `docs/spec/security.md`

**Content covers:**
- Scope: Authentication, authorization, CSRF, middleware, signing, rate limiting
- Rules:
  - Middleware order matters: Recovery → CORS → SecurityHeaders → CSRF → IPGate → MaxBodySize → Compression → Auth
  - CSRF uses Origin/Referer validation, NOT tokens
  - TUI bypasses CSRF via X-Internal-Token (constant-time comparison)
  - Loopback + private IPs skip auth
  - Ed25519 signature verification before binary swap
  - Never trust X-Forwarded-For (uses RemoteAddr directly)
- Body:
  - **Middleware stack:** Full ordered list with what each does
  - **CSRF protection:** Origin/Referer validation on POST/PUT/DELETE, exemptions (loopback routes, same-process via X-Internal-Token), form submission rejection rules
  - **Authentication:** scrypt (N=16384, r=8, p=1), 24-hour sessions (32-byte random hex), hourly cleanup, persistent client tokens (32-byte, prefix-indexed in DB, rotation on re-login)
  - **Authorization:** IP-based (loopback/private skip auth, external requires session), network_access levels (localhost, lan, external, public)
  - **Rate limiting:** Sliding window per-IP (in-memory), 10,000 max entries, per-route limits (login 5/60s, password 3/60s, POT 30/60s)
  - **Content Security Policy:** Full CSP header with allowed sources
  - **TLS:** Optional, auto-generates self-signed cert if files don't exist, port fallback (774-784)
  - **Updater signing:** Ed25519 embedded public key, .sig file verification, 3-step binary swap (.new → current → .old)
  - **Internal token:** 16-byte random hex per server startup, used by TUI's custom RoundTripper
- Cross-references: `architecture.md`, `operations.md`, source files

**Step 1: Write the doc**

**Step 2: Commit**

```bash
git add docs/spec/security.md
git commit -m "docs: add security spec deep-dive"
```

---

### Task 9: Write `docs/spec/operations.md`

**Files:**
- Create: `docs/spec/operations.md`

**Content covers:**
- Scope: Build, CI, release, updates, launcher, monitoring references
- Rules:
  - Build requires Go 1.25, produces single Windows binary
  - FFmpeg required at runtime (on PATH or configured path)
  - CI builds on tag push, reads RELEASE_NOTES.md
  - Updates use Ed25519 verification before binary swap
  - Exit code 42 triggers launcher respawn
- Body:
  - **Build:** go build commands, Windows resource embedding (go-winres), .syso files
  - **CI:** GitHub Actions workflow (.github/workflows/release.yml), tag-triggered, Windows exe
  - **Release process:** Generate RELEASE_NOTES.md, bump version in main.go, commit+tag+push
  - **Self-update flow:** Check GitHub → download binary + sig → verify Ed25519 → 3-step rename → exit 42 → launcher respawns
  - **Launcher/supervisor:** Parent process spawns child with _MOOMBOX_CHILD=1, respawns on exit code 42, CreateNoWindow flag for detached processes
  - **Shutdown:** Cancel context → stop services in reverse order → 10-second force-exit timer
  - **Reference repos:** references/ directory (gitignored), update-all.sh script, upstream tracking for yt-dlp/BgUtils/ejs/chatterino7/moonarchive
  - **Monitoring:** Discord webhook notifications for events (found, live, downloading, finished, error, auth, disk_warning, update_available)
  - **Signing tool:** cmd/sign/main.go for CI (Ed25519 private key from GitHub Actions secret)
- Cross-references: `architecture.md`, `security.md`

**Step 1: Write the doc**

**Step 2: Commit**

```bash
git add docs/spec/operations.md
git commit -m "docs: add operations spec deep-dive"
```

---

### Task 10: Write `docs/spec/appendix-metrics.md`

**Files:**
- Create: `docs/spec/appendix-metrics.md`

**Content covers:**
- Last verified date
- Go version, module path
- Package line counts (from CLAUDE.md, current as of last count)
- Total file counts
- Database schema version
- Key dependency versions
- main.go line count
- Current version string location

**Step 1: Write the doc**

Gather current metrics from code (schema version, Go version from go.mod, current app version from main.go).

**Step 2: Commit**

```bash
git add docs/spec/appendix-metrics.md
git commit -m "docs: add appendix-metrics with current volatile numbers"
```

---

### Task 11: Write `SPEC.md`

**Files:**
- Create: `SPEC.md`

**Content:** ~800-1500 lines, 8 sections, each 3-10 paragraphs. Standalone — an AI can understand the project without reading deep-dives. Pointers to `docs/spec/*.md` at the end of each section.

**Sections:**
1. Vision & Purpose (~80 lines)
2. Design Philosophy (~120 lines)
3. Architecture (~300 lines) — largest section
4. Platform Services (~200 lines)
5. User Interfaces (~200 lines)
6. Data & Storage (~150 lines)
7. Security (~120 lines)
8. Operations (~80 lines)

Each section: prose explanation sufficient for AI comprehension, key patterns called out, link to deep-dive.

**Step 1: Write the full SPEC.md**

**Step 2: Commit**

```bash
git add SPEC.md
git commit -m "docs: add comprehensive SPEC.md — AI-first project specification"
```

---

### Task 12: Reduce `CLAUDE.md`

**Files:**
- Modify: `CLAUDE.md`

**What stays:**
- "What This Is" — shortened to 2-3 sentences + pointer to SPEC.md
- "Working Style" — unchanged
- "Build & Test" — unchanged (including Windows resource embedding)
- "Critical Patterns" section — logger interface, database partial updates, job status lifecycle, TUI chord system entry points, config migrations, API route prefix
- "References" — unchanged
- "Release Process" — unchanged
- "Maintaining This File" — updated to reference SPEC.md

**What moves to SPEC.md (removed from CLAUDE.md):**
- Design Constraints (→ SPEC.md Design Philosophy)
- Key Dependencies table (→ SPEC.md Architecture)
- TUI design rule (→ SPEC.md User Interfaces)
- Architecture section (process model, init order, package graph, data flow) (→ SPEC.md Architecture)
- Download orchestration details (→ SPEC.md Architecture)
- Concurrency model details (→ SPEC.md Architecture)
- YouTube multi-client auth (→ SPEC.md Platform Services)
- Security details (→ SPEC.md Security)
- Panic recovery patterns (→ SPEC.md Architecture)
- Web UI file table (→ SPEC.md User Interfaces)

**Step 1: Edit CLAUDE.md**

**Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: reduce CLAUDE.md to control prompts — detail moved to SPEC.md"
```

---

### Task 13: Update memory files

**Files:**
- Modify: `C:\Users\Wulf\.claude\projects\D--Git-Moombox\memory\MEMORY.md`

**Update to reflect:**
- SPEC.md exists and is the comprehensive reference
- CLAUDE.md is now lean control prompts
- docs/spec/ directory structure

**Step 1: Edit MEMORY.md**

**Step 2: Verify all files exist**

Run: `ls -la SPEC.md docs/spec/`

---

### Task 14: Final review

**Step 1: Verify SPEC.md line count**

Run: `wc -l SPEC.md`
Expected: 800-1500 lines

**Step 2: Verify all deep-dive docs exist**

Run: `ls docs/spec/`
Expected: 9 files (8 sections + appendix-metrics)

**Step 3: Verify CLAUDE.md is reduced**

Run: `wc -l CLAUDE.md`
Expected: Significantly shorter than before (~80-120 lines vs ~229)

**Step 4: Verify build still works**

Run: `go build ./...`
Expected: Success (no code changes, only docs)

**Step 5: Final commit if any cleanup needed**
