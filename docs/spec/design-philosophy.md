# Design Philosophy

## Scope

This document defines the design priorities, platform constraints, code complexity principles, and philosophical stances that govern every decision in the Moombox codebase. When two good things conflict — speed versus correctness, simplicity versus capability, resource usage versus features — this document specifies which one wins. It is the authoritative reference for "why was it built this way."

## Rules and Constraints

These are hard rules. They are not guidelines, suggestions, or aspirations. An AI assisting with Moombox development must follow these without exception:

- **Priority ordering is absolute.** Correctness beats reliability. Reliability beats resource efficiency. The full ordering is defined in the Priority Ordering section below. When two priorities conflict, the higher one wins. There are no exceptions.
- **Cross-platform via build tags.** Windows x64 and Linux x64/arm64 are supported. macOS is deferred. Platform-specific code lives in `_windows.go` / `_unix.go` / `_other.go` files per package. Do not add macOS support unless explicitly requested.
- **No CGo.** Every dependency must be pure Go. No C bindings, no shared libraries, no native extensions. This is non-negotiable — it ensures single-binary distribution with zero system dependencies beyond Go itself (and FFmpeg at runtime).
- **Single binary deployment.** The entire application compiles to one `.exe`. Web assets are embedded via `go:embed`. No installer, no external config files required, no runtime file extraction.
- **Resource efficiency is a design constraint, not an optimization.** Moombox runs 24/7. Every constant allocation, every polling loop, every idle goroutine is a cost the user pays continuously. Design for zero-cost-when-idle from the start.
- **Both UIs are first-class.** Neither the Web UI nor the TUI is a secondary or degraded experience. Feature parity is required, though the specific UX implementation can differ to leverage each platform's strengths.
- **Never crash.** Every goroutine must have panic recovery. A failure in one subsystem must never take down another subsystem or the application as a whole.
- **Never silently fail.** If something goes wrong, the user must know about it. Swallowed errors are treated as bugs with the same severity as crashes.

---

## Priority Ordering

This is the definitive priority list for the entire project. When two design goals conflict, the one that appears higher in this list wins. No amount of benefit at a lower priority level justifies compromising a higher one.

### 1. Correctness

**Never lose a stream. Never corrupt output. Data integrity is paramount.**

A missed stream or a corrupted file is an unrecoverable failure. The content that Moombox archives is ephemeral — live streams exist only once. If the archiver fails to capture it, or captures it but produces a broken file, that content is gone forever. This makes correctness the single highest priority in every design decision.

What this means in practice:
- Segment downloads are verified before being committed to the output pipeline.
- FFmpeg muxing runs in `context.Background()` goroutines so that muxing completes even when the parent context is cancelled. A cancellation should not produce a half-muxed, corrupted output file.
- The verification loop after download completion probes the YouTube API up to 6 times to confirm a stream actually ended before marking the job as finished. This prevents premature termination when streams experience temporary interruptions.
- Quality changes mid-stream trigger a segment split rather than attempting to merge incompatible formats, which would produce a corrupted file.
- Resume state is persisted to `.resume.json` sidecar files so that interrupted downloads can be continued without re-downloading segments or losing progress.

### 2. Reliability

**Runs 24/7 without human intervention. Recovers from errors gracefully.**

Moombox is an archival appliance. The user starts it, configures their channels, and expects it to run indefinitely. The system must survive network hiccups, API changes, temporary authentication failures, rate limiting, unexpected input formats, and every other form of real-world chaos without crashing or requiring manual restart.

What this means in practice:
- Every goroutine has inline `defer func() { if r := recover(); ... }()` panic recovery. HTTP handlers have `RecoveryMiddleware`. Database callbacks use `safeCallJobUpdate` and `safeCallJobsChange` wrappers.
- The launcher/supervisor pattern (parent process respawns child on exit code 42) provides process-level recovery. Even if the application crashes entirely, the launcher brings it back.
- Authentication uses a multi-client fallback chain. If one Innertube client fails, the system tries the next. If cookies expire, the system degrades to unauthenticated access rather than stopping entirely.
- Network errors trigger retries with exponential backoff rather than immediate failure.
- The shutdown sequence uses a 10-second force-exit timer. If graceful shutdown stalls, the process terminates anyway rather than hanging indefinitely.

### 3. Resource Efficiency

**Low CPU, RAM, IO, and network footprint. Every wasted cycle and byte matters.**

Moombox runs 24/7 on the user's personal machine, not a server farm. It shares resources with games, browsers, creative tools, and everything else the user runs. Constant overhead — even small amounts — accumulates into a meaningful impact on the user's system over time.

What this means in practice:
- **Signal-driven concurrency over polling.** The database batch update system sleeps until signaled — it performs zero IO when nothing is changing. This is preferred everywhere: don't poll on a timer when you can wait for a signal.
- **Goja VMs auto-evict when idle.** Cipher VMs hold multi-MB JavaScript runtimes in memory. These are expensive to keep around. Cipher VMs use a 3-VM LRU cache, so at most three player.js runtimes exist simultaneously. The fallback BotGuard goja-VM (when the sidecar is unavailable) evicts itself via `time.AfterFunc` when its TTL expires.
- **BotGuard sidecar is one long-running subprocess, not per-request.** The Node + JSDOM sidecar starts once at Moombox launch and serves every PO-token request from the same V8 instance. Per-request subprocess spawning would cost 200-500ms cold-start per token; the long-running model amortises that to a one-time startup cost. The subprocess is pinned to a Windows Job Object so it dies with Moombox even on hard parent crashes.
- **Database batch coalescing.** Updates within a 100ms window are flushed in a single transaction rather than individually. This reduces disk IO by orders of magnitude during active downloads (when many progress updates fire per second) while adding negligible latency.
- **WebSocket broadcasts rely on upstream rate-limiting.** Job update broadcasts are not throttled in the hub — the only high-frequency caller (`OnJobChange` via `ProgressTracker.maybeUpdate`) is already capped to ~60 Hz per job by a 16 ms gate in `internal/worker/progress.go`, and the other callers are event-driven. An earlier per-job throttle in the hub raced against `BroadcastJobDeleted` (which is not throttled) and could resurrect deleted rows.
- **TUI non-blocking sends.** Channel sends to the TUI use non-blocking operations with drop counters. If the TUI's event loop is busy, updates are dropped rather than blocking the sender. The drop counter tracks how many were missed so the next successful send can trigger a full refresh.
- **Log ring buffer.** The in-memory log buffer is bounded at 200 lines. Old entries are evicted as new ones arrive. This prevents unbounded memory growth in long-running sessions.
- **WAL mode SQLite.** Write-ahead logging allows concurrent reads during writes without blocking, and the single-connection configuration eliminates lock contention entirely.

### 4. Simplicity of Deployment and User Experience

**Single binary, sensible defaults, easy to get started.**

Users should not need to understand DASH manifests, Innertube APIs, HLS variant playlists, or BotGuard protocols to use Moombox. The first-run setup wizard guides initial configuration. Default settings work for the vast majority of use cases. The application ships as a single `.exe` with no dependencies beyond FFmpeg (which the setup wizard helps install).

What this means in practice:
- No installer. Download the `.exe`, run it.
- No configuration file required. Sensible defaults are applied. A `config.toml` is created on first run via the setup wizard.
- FFmpeg is the only runtime dependency, and both UIs include a setup flow to help install it.
- Self-updates are built in — the application checks for new versions, verifies Ed25519 signatures, and swaps its own binary.
- Web assets are embedded in the binary via `go:embed`. There are no external static files to manage.

**Important clarification:** This simplicity applies to the **user experience and deployment**, not to the codebase. The internal code is as complex as it needs to be. YouTube's API requires complex handling. BotGuard requires running a JavaScript VM. Cipher solving requires AST parsing. These are genuinely complex problems, and the code reflects that honestly. The goal is to hide that complexity from the user, not to pretend it does not exist.

### 5. User Experience (Polish)

**Polished, intuitive, both UIs feel first-class. Visual details matter.**

Beyond basic usability (covered by priority 4), the UIs should feel polished and professional. Status bars should show relevant information at a glance. Progress indicators should be accurate. Error messages should be clear and actionable. The TUI's chord system should feel responsive. The Web UI should look good on mobile.

What this means in practice:
- Status bars in both UIs show connection state, disk space, monitor status, cookie validity, and update availability.
- Progress strings show segment counts for video, audio, and chat (e.g., `V:1234 A:1234 C:5678`).
- Error messages include context about what went wrong and, where possible, what the user can do about it.
- The TUI uses Charmbracelet's styling ecosystem (lipgloss) for consistent, attractive terminal rendering.
- The Web UI uses Shoelace components for a consistent, modern look without a heavy framework.
- Mobile breakpoints at 992px (tablet) and 768px (phone) ensure the Web UI works on all screen sizes.

### 6. Feature Completeness

**Cover the full workflow from monitoring to playback.**

Every step of the archive pipeline should be handled: monitoring channels for new streams, detecting when they go live, downloading video/audio/chat, muxing into final output, organizing files, and playing them back. The user should not need external tools (beyond FFmpeg) to complete any part of the workflow.

What this means in practice:
- Monitors detect streams via RSS feeds (YouTube), DECAPI (Twitch), and Twitch's native API.
- Downloads handle DASH (YouTube), HLS (Twitch), and VOD (both) formats.
- Chat is downloaded alongside video for both platforms.
- FFmpeg muxing produces standard container files.
- The built-in video player (Web UI) supports multi-segment seeking and niconico-style chat overlay.
- Trim functionality allows extracting clips from archived streams.
- Statistics dashboards show archive history and trends.
- Import functionality handles zip archive ingestion.

### 7. Performance

**Fast when it matters, but never at the cost of the above.**

Optimize hot paths — segment downloads, manifest parsing, database queries. But never sacrifice correctness (no skipping verification for speed), reliability (no removing error handling for throughput), or resource efficiency (no caching everything in RAM for lower latency) to achieve it.

What this means in practice:
- Catch-up downloading uses 6 parallel segment fetches when falling behind, but only when needed.
- Cipher solving caches compiled VMs to avoid recompilation, but caps the cache at 3 entries to limit memory.
- Database queries use prepared statements and indexes, but the single-connection model is retained for simplicity and correctness.
- The TUI reduces its progress tick interval from 16ms (active) to 500ms (idle) to avoid unnecessary rendering work.

---

## Code Complexity Principles

These principles govern how complexity is managed in the codebase. They are not about minimizing complexity at all costs — they are about ensuring complexity is justified, contained, and honest.

### Match Solution Complexity, Not Problem Complexity

YouTube's API is complex. BotGuard is complex. Twitch's undocumented GQL API is complex. But the complexity of the problem domain does not automatically justify complex code. If a complex problem has a simple solution, use the simple solution.

The question to ask is: "Does the code need to be this complex to produce the correct result?" If yes, the complexity is justified. If no — if a simpler approach would work just as well — use the simpler approach regardless of how complex the underlying problem appears.

Example: YouTube's cipher uses obfuscated JavaScript with dynamic function names and nested transformations. The problem is extremely complex. But the solution — extract the transformation sequence from the player.js AST, compile it once, cache the VM — is a relatively clean pipeline despite the gnarly domain.

### Complexity Is Justified When the Solution Genuinely Requires It

Some things are inherently complex and cannot be simplified without losing correctness or capability:

- **Concurrent segment downloading** with catch-up parallelism, quality monitoring, and graceful degradation on quality loss.
- **Cipher solving** with AST parsing, regex fallback, disk caching, and a 3-VM LRU memory cache with mutex-serialized compilation to prevent thundering herd.
- **Multi-client auth fallback chains** across 6 Innertube clients with different capabilities, auth levels, and failure modes.
- **BotGuard/PO token generation** involving challenge fetching, JavaScript VM execution, snapshot creation, and triple-layer caching.
- **Download orchestration** managing the lifecycle of video downloaders, audio downloaders, chat downloaders, quality monitors, verification loops, and FFmpeg muxing — all running concurrently with proper cancellation propagation.

In these cases, the code is complex because the solution is genuinely complex. Attempting to simplify it would either break correctness or remove necessary capability. The complexity is honest.

### Contain Complexity Behind Clean Interfaces

A package can be gnarly internally. Its public API should be simple. Complexity that leaks across package boundaries is a design defect.

The cipher package is a good example. Internally, it parses JavaScript ASTs, falls back to regex extraction, manages a 3-VM LRU cache with mutex serialization, and handles disk caching with TTL-based eviction. Externally, it exposes a function that takes a player URL and ciphered parameters and returns deciphered parameters. The consumer does not need to know about VMs, ASTs, or caches.

Similarly, the BotGuard package internally manages Goja VMs, challenge endpoints, minter caching, and inflight deduplication. Externally, it provides a method to get a PO token. The YouTube service calls it without caring about the implementation.

This principle applies at every scale: functions should hide their internal complexity, structs should expose only what consumers need, and packages should present clean boundaries to the rest of the codebase.

### Simple Solutions to Complex Problems Are Ideal

Three lines of straightforward code beats a clever abstraction every time — unless that abstraction genuinely simplifies things across the codebase. "Clever" is not a compliment. Code that is easy to read, easy to debug, and easy to modify is better than code that is elegant, minimal, or architecturally pure.

This does not mean avoiding abstractions. Abstractions are essential when they genuinely reduce cognitive load. The database's `UpdateJobFields` method is an abstraction over dynamic SQL generation — it simplifies every call site. The worker's semaphore pattern is an abstraction over concurrency limiting — it simplifies every download. These abstractions earn their existence by making the rest of the code simpler.

The test is: does this abstraction make the code easier to understand, or does it make the code easier to write at the cost of being harder to understand? If the latter, skip the abstraction and write the straightforward version.

---

## Platform Constraints

### Cross-Platform via Build Tags

Moombox supports Windows x64, Linux x64, and Linux arm64. macOS is not supported (deferred). Platform-specific behavior is isolated in per-package `_windows.go` / `_unix.go` files using Go build constraints:

- **Disk space queries** — kernel32 `GetDiskFreeSpaceExW` on Windows (`internal/disk/disk_windows.go`); `statfs` syscall on Linux (`internal/disk/disk_unix.go`).
- **Cookie extraction** — DPAPI decryption and Windows registry-based browser detection on Windows; graceful degradation with UI messaging on Linux (manual cookie file path or browser-selection dropdown).
- **Process creation** — `CREATE_NO_WINDOW` (`0x08000000`) for detached child processes on Windows (`launcher_windows.go`); standard exec on Linux (`launcher_unix.go`).
- **Single-instance locking** — `CreateMutex` on Windows; `flock` on Linux.
- **Self-update cleanup** — the `.exe~` ping-based deferred delete is Windows-only (`launcher_windows.go`); Linux has no equivalent constraint since running binaries can be replaced in-place.
- **Connectivity monitor** — ICMP-free TCP dial on Linux (`monitor_unix.go`); similar approach on Windows.

The core download pipeline, web dashboard, TUI, BotGuard sidecar, and all business logic are identical across platforms. Windows-specific features degrade gracefully on Linux with clear UI messaging rather than silently failing.

### No CGo

Every dependency must be pure Go. This rules out:
- CGo-based SQLite drivers (Moombox uses `modernc.org/sqlite`, a pure-Go SQLite implementation)
- Native crypto libraries
- System notification libraries that use CGo bindings
- Any library that requires a C compiler in the build chain

The benefits of this constraint:
- **Single binary distribution.** No shared libraries to ship, no DLL hell, no "works on my machine."
- **Simple build chain.** `go build` is the only tool needed (plus `go-winres` for the Windows icon/version info, which is optional).
- **Cross-compilation friendly.** The pure-Go constraint enables cross-compilation from any host OS. CI cross-compiles all three platform binaries (Windows x64, Linux x64, Linux arm64) from a single ubuntu-latest runner.
- **Reproducible builds.** No dependency on system-installed C libraries or compiler versions.

The cost is occasionally lower performance compared to CGo alternatives (e.g., `modernc.org/sqlite` is slower than `mattn/go-sqlite3`). This is an acceptable tradeoff given the priority ordering — resource efficiency matters, but deployment simplicity ranks higher, and the performance difference is not meaningful for Moombox's workload.

### Single Binary

Everything compiles into one executable:
- All Go code compiles to a single binary (`Moombox.exe` on Windows, `moombox-linux-amd64` / `moombox-linux-arm64` on Linux).
- All web assets (HTML, CSS, JavaScript, images) are embedded via `go:embed` in `web/embed.go`.
- No external configuration files are required at startup — sensible defaults are applied, and a configuration file is created during the first-run wizard.
- No installer is needed. The user downloads the binary and runs it.
- Self-updates replace the binary in-place (with signature verification).

The only external runtime dependency is FFmpeg, which must be on PATH or configured in the settings. Both UIs include a setup flow to help the user install FFmpeg if it is not found.

---

## Resource Efficiency in Depth

Because Moombox runs continuously on the user's personal machine, resource efficiency is not an afterthought optimization — it is a design constraint that influences architecture from the ground up. The goal is **zero cost when idle**: if nothing is happening, the application should consume negligible CPU, IO, and network.

### Signal-Driven Concurrency

Polling loops are a last resort. Where possible, subsystems sleep until explicitly signaled:

- The **database batch update system** uses a signal channel. When an update is queued, a signal is sent. The flush goroutine wakes, waits 100ms for more updates to accumulate, then flushes everything in a single transaction. If no updates are queued, the goroutine sleeps indefinitely — zero CPU, zero IO.
- The **WebSocket broadcast system** is also signal-driven: a broadcast only happens when `OnJobChange` fires. The hub does no throttling of its own — `ProgressTracker.maybeUpdate` already caps progress writes at ~60 Hz/job upstream, and every other UpdateJobFields caller is event-driven. During idle periods both layers do nothing.

Some subsystems necessarily poll because the external API provides no push mechanism:
- RSS feed monitors poll on a configurable interval (default varies by platform).
- The HLS segment downloader polls the playlist for new segments.
- The quality monitor probes every 30 seconds during active downloads.

In these cases, the polling interval is tuned to balance responsiveness against resource usage, and polling stops entirely when there is nothing to monitor.

### Memory Management for Expensive Objects

JavaScript runtimes are the most expensive objects in the application. Two subsystems run JS:

- **BotGuard** runs primarily under an embedded Node.js + JSDOM sidecar (real V8) — one long-running subprocess for the lifetime of Moombox. The sidecar's V8 heap is the largest single JS allocation in the system but it sits in a separate process so it does not compete with Go's GC for the main heap. PotProvider keeps a triple-layer in-process cache (session tokens 6h TTL, single goja-fallback minter VM with proactive refresh + auto-eviction, inflight dedup) on top of the sidecar's own internal minter cache. When the sidecar is disabled or unhealthy, the goja-VM fallback path keeps token generation working at reduced fidelity (websafe-fallback only).
- **Cipher** uses a 3-VM Goja LRU cache keyed by player.js URL. When a fourth unique player.js is encountered, the least-recently-used VM is evicted. Compilation of new VMs is mutex-serialized to prevent thundering herd (multiple goroutines all trying to compile the same player.js simultaneously).

### Bounded Buffers

All in-memory buffers have explicit bounds:
- Log ring buffer: 200 lines maximum.
- Chat message deduplication: 5,000 recent IDs with deterministic eviction.
- Twitch emote cache: 200 channels (LRU).
- Rate limiter entries: 10,000 maximum per-IP entries.
- Per-job log buffers: in-memory string arrays (bounded by job lifetime).

---

## Dual UI Philosophy

Both the Web UI and TUI are first-class citizens. Neither is a wrapper around the other. Neither is a degraded or simplified version of the other. Both communicate with the same backend via the same APIs (HTTP + WebSocket), and both surface the same capabilities.

However, each UI leans into its platform's strengths rather than trying to be identical:

### TUI Advantages

- **Real-time monitoring.** Terminal rendering is inherently lower-latency than browser rendering. The TUI feels more immediate for watching download progress and log output.
- **Keyboard-driven workflows.** The chord system (e.g., `AA` to add a video, `RC` to cancel, `QQ` to quit) enables fast, fluid interaction without reaching for the mouse. Power users can manage their entire archive workflow without leaving the keyboard.
- **Immediate feedback.** Status updates, error notifications, and progress changes appear with minimal delay.
- **Cleaner experience.** No browser chrome, no tab management, no notifications competing for attention. The TUI occupies a terminal window and that is its entire world.

### Web UI Advantages

- **Rich media.** The built-in video player with niconico-style chat overlay, visual statistics dashboards, and the trim editor are capabilities that cannot exist in a terminal.
- **Accessibility.** Anyone with a web browser can use it. No terminal knowledge required. More approachable for casual users.
- **Mobile support.** Responsive design with breakpoints at 992px (tablet) and 768px (phone) means the Web UI works on phones and tablets.
- **Broader reach.** Accessible from any device on the network (when configured for LAN or external access), not just the machine running Moombox.

### What Parity Means

Feature parity means both UIs cover the same **capabilities**: manage channels, view and control jobs, configure settings, manage cookies, view logs, handle first-run setup, import archives, and create trim clips. Both UIs receive the same real-time updates via WebSocket.

Feature parity does **not** mean identical UX. The TUI has keyboard chords that have no Web UI equivalent (the Web UI uses buttons and menus instead). The Web UI has a video player, statistics charts, and visual dashboards that have no TUI equivalent (the TUI shows the same data as text). Each UI implements shared capabilities in the way that best fits its platform.

### TUI Design Rule

The TUI is built on Charmbracelet's ecosystem: `bubbletea` for the core framework (Elm architecture), `bubbles` for pre-built components (list, viewport, spinner, key, cursor, paginator), `huh` for form workflows (multi-step wizards, inputs, selects, confirms), and `lipgloss` for styling (colors, borders, padding, layout).

Before building any new TUI component, the developer must first check Charm's repositories (github.com/charmbracelet) for an existing component that solves the same problem. Prefer using or extending Charm's building blocks over rolling custom implementations. This applies to lists, text inputs, forms, file pickers, tables, progress bars, and any UI primitive. Custom implementations are justified only when Charm does not provide a suitable building block and extending an existing one would be more complex than building from scratch.

---

## Error Philosophy

### Never Crash

Every goroutine in the application has panic recovery. The patterns are:

- **Goroutines:** Inline `defer func() { if r := recover(); r != nil { ... } }()` at the top of every goroutine function body. The recovery logs the panic with a stack trace and continues. The goroutine exits but the application survives.
- **HTTP handlers:** `RecoveryMiddleware` in the middleware stack catches panics in any handler and returns a 500 response instead of crashing the server.
- **Database callbacks:** `safeCallJobUpdate` and `safeCallJobsChange` wrap subscriber callbacks so that a panicking subscriber does not crash the database update pipeline.

A single failed stream, a single malformed API response, a single unexpected nil pointer — none of these should affect other downloads, other subsystems, or the application itself.

### Degrade Gracefully

When something fails, the system finds the best available fallback rather than stopping:

- **Missing FFmpeg:** The application starts and can monitor and detect streams, but cannot mux. The error is reported clearly — "FFmpeg not found" — not as a cryptic crash.
- **Expired cookies:** The application continues with unauthenticated access. Membership-only or age-restricted content becomes unavailable, but public content still works. The status bar shows the cookie state so the user knows.
- **Authentication failure on one client:** The multi-client fallback chain tries the next Innertube client. Only if all clients fail does the job enter an error state.
- **Network interruption:** Downloads pause and retry. Monitors continue their polling cycle. The application does not assume that a temporary network failure is permanent.
- **Disk full:** Downloads pause and the status bar shows a disk warning. They do not crash or corrupt partially-written files.

### Always Inform the User

Errors are logged prominently (at Error or Warn level) and surfaced to both UIs via the log stream and status indicators. The user should always be able to answer: "Is something wrong? What is it? What can I do about it?"

Specific notification mechanisms:
- **Log output:** Visible in both the TUI log panel and Web UI log view.
- **Status bar indicators:** Connection state, disk space, cookie validity, and update availability are always visible.
- **Discord webhooks:** Configurable notifications for key events (stream found, going live, download started, finished, errored, auth issues, disk warnings, updates available).
- **Job status:** Each job's status clearly indicates its state. Error jobs show what went wrong. `COOKIES?` status specifically indicates an authentication problem that the user can fix by providing cookies.

### Silent Failures Are as Bad as Crashes

If a stream was supposed to be captured and was not, the user must be notified. An error that is logged at Debug level, or handled with an empty catch block, or silently retried until it succeeds without the user knowing there was a problem — these are bugs. They are not acceptable error handling.

The distinction: **expected degradation** (like falling back from one Innertube client to another) can happen silently because the system is working as designed. **Unexpected failures** (like a monitor failing to detect a stream, or a download silently producing an incomplete file) must always be surfaced.

---

## Upstream Relationship

Moombox is an **informed reimplementation**. It is not a wrapper, binding, plugin, or fork of yt-dlp or any other project. It is a standalone Go application that independently implements YouTube and Twitch downloading, monitoring, and archiving.

### What "Informed Reimplementation" Means

Moombox tracks several upstream projects for awareness of protocol changes, API changes, and extraction logic updates:

- **yt-dlp** — YouTube format selection, cipher/signature extraction, Twitch extraction, PO token handling, cookie management
- **BgUtils** — BotGuard/PO token generation protocol
- **ejs** — YouTube cipher solving (external JS for yt-dlp)
- **chatterino7** — Twitch chat (IRC protocol, emotes, badges)
- **moonarchive** — Segment download strategies for live streams

When YouTube or Twitch changes something (a new cipher obfuscation pattern, a new API endpoint, a new authentication requirement), Moombox developers review the upstream fix to understand the change, then adapt the solution to fit Go and Moombox's architecture. The adaptation is independent — it uses Moombox's existing patterns, error handling, caching, and concurrency model rather than mirroring the upstream implementation.

### Where Moombox Diverges

Moombox diverges significantly from upstream projects in several areas:

- **Download strategy.** yt-dlp downloads one format at a time. Moombox downloads segments concurrently (DASH sequential, HLS polling, VOD parallel with catch-up at 6 parallel), monitors quality in real-time, and splits segments on quality changes.
- **Concurrency model.** yt-dlp is single-threaded per download. Moombox orchestrates multiple concurrent downloaders (video, audio, chat) per stream, with a worker pool managing multiple simultaneous streams.
- **Error recovery.** yt-dlp retries at the download level. Moombox has layered recovery: segment-level retry, stream-level retry with auth upgrade, job-level error state with user notification, and process-level restart via the launcher.
- **Caching.** yt-dlp does not cache BotGuard minters or cipher VMs across invocations (it is a CLI tool that exits after each download). Moombox runs continuously and maintains multi-layer caches with TTL-based eviction.
- **Real-time monitoring.** yt-dlp does not monitor channels. Moombox's monitor subsystem continuously watches for new streams via RSS, DECAPI, and Twitch's API.

The upstream projects are references, not authorities. Moombox follows its own design philosophy and makes its own architectural decisions.

---

## Cross-References

- **[vision-and-purpose.md](vision-and-purpose.md)** — What Moombox is, who it is for, and why it exists. Provides the context for why these design principles matter.
- **[architecture.md](architecture.md)** — How these principles manifest in the actual codebase structure. The process model, package graph, data flow, and concurrency patterns are direct consequences of the priorities defined here.
- **[user-interfaces.md](user-interfaces.md)** — Detailed implementation of the dual UI philosophy, including the chord system, WebSocket protocol, and platform-specific UX decisions.
- **Source: `cmd/moombox/main.go`** — The launcher/supervisor pattern, service initialization order, and top-level wiring that embodies these principles.
- **Source: `internal/worker/`** — Download orchestration, where correctness-over-performance tradeoffs are most visible (background mux, verification loops, quality splitting).
- **Source: `internal/bgutils/`** and **`internal/cipher/`** — Examples of justified complexity contained behind clean interfaces.
- **Source: `internal/database/`** — Signal-driven batch coalescing, the primary example of resource-efficient design.
- **Source: `internal/tui/`** — Charm ecosystem usage and the chord system implementation.
