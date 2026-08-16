# Vision and Purpose

## Scope

This document defines what Moombox is, why it exists, who it is for, and what it does at a high level. It establishes the project's identity, boundaries, and deployment model. Every other specification document assumes the reader has internalized this one first.

## Rules and Constraints

- Moombox is a **YouTube/Twitch live stream archiver**. That is its primary identity. All features serve this purpose.
- Moombox supports **Windows x64, Linux x64, and Linux arm64**. macOS is not supported (deferred). Core functionality is identical across platforms; Windows-specific features (UAC elevation, DPAPI cookie extraction) degrade gracefully on Linux with clear UI messaging.
- Moombox is a **standalone Go program**. It does not shell out to yt-dlp, streamlink, or any other external download tool. The only external runtime dependency is FFmpeg.
- Moombox is a **single-binary application**. There is no installer, no service framework, no daemon manager. The user runs an executable.
- Moombox is a **personal tool built to product-level quality**. The owner/developer's needs drive every decision. Other users benefit from the polish, but the owner is the primary stakeholder.
- Moombox **tracks upstream changes** in yt-dlp, BgUtils, and ejs for awareness of YouTube/Twitch protocol changes, but it reimplements solutions independently in Go. It is not a wrapper, binding, or fork of any upstream project.
- Moombox uses **no CGo**. All dependencies must be pure Go. This is a hard constraint that prioritizes build simplicity over raw performance.

## What Moombox Is

Moombox is a YouTube/Twitch live stream archiver written in Go. It runs as a 24/7 appliance that monitors channels you care about, detects when streams go live, downloads them in real time (video + audio + live chat), muxes everything into final output files using FFmpeg, and provides both a web dashboard and terminal UI for monitoring and management.

It also functions as a lightweight yt-dlp alternative specifically for YouTube and Twitch. Rather than wrapping yt-dlp, Moombox reimplements the relevant extraction and download logic in Go, tracking upstream yt-dlp changes to stay current with YouTube's and Twitch's evolving APIs and anti-bot measures.

The project is a Go rewrite of an earlier TypeScript/Node.js codebase. The rewrite is complete as of early 2026. The original TypeScript implementation is preserved on the `abandoned-nodejs` branch for historical reference but has no bearing on current development.

## Origin and Motivation

Live streams are ephemeral. Once a stream ends, the live experience — including real-time chat — is gone or degraded. VODs may be deleted, made members-only, or lose chat entirely. Moombox exists to solve this problem: it ensures that streams you care about are captured reliably and completely, without requiring you to be present when they happen.

Moombox is fundamentally a personal tool. It was built by one person to solve their own archival needs. However, it is designed with the polish, user experience, and reliability that would let anyone use it. This is not a hacky script — it is a product-quality application with a setup wizard, dual UIs, self-updating, error recovery, and thoughtful defaults.

The motivation can be summarized as: **never miss a stream, never lose an archive, never babysit the process.**

## Target User

### Primary User

The primary user is the owner/developer. Their workflow, preferences, and hardware drive every design decision. When there is a tension between "what would be ideal for a general audience" and "what the owner actually needs," the owner's needs win.

### Secondary Users

Beyond the owner, Moombox is designed for anyone who wants a "set it and forget it" archival solution for YouTube and Twitch streams. The expected secondary user profile:

- **Technical enough** to run a standalone binary, edit a TOML configuration file, and ensure FFmpeg is on their PATH.
- **Not expected** to understand DASH manifests, Innertube API internals, HLS playlist structures, BotGuard challenge flows, or YouTube signature cipher mechanics. These are implementation details that Moombox abstracts away.
- **Motivated by archival** — they want to preserve streams they care about, whether for personal enjoyment, community preservation, or content creation.

### UX Philosophy for Both Audiences

Moombox should be intuitive for non-technical users while providing advanced controls for power users. Sensible defaults mean a new user can get archiving working with minimal configuration. Power users can tune quality preferences, download concurrency, monitoring intervals, cookie management, and more.

## What Moombox Does

### The Full Workflow

This is the end-to-end lifecycle of how Moombox archives a stream:

1. **Channel Monitoring** — Moombox continuously monitors configured YouTube channels via RSS feeds and the DECAPI API, and Twitch channels via GQL polling. It checks for new live streams, upcoming scheduled streams, and new VODs at configurable intervals.

2. **Stream Detection** — When a monitored channel goes live, schedules an upcoming stream, or publishes a new VOD, Moombox creates a job in its SQLite database. For upcoming streams, Moombox waits and begins downloading when the stream actually starts.

3. **Stream Probing** — Before downloading, Moombox probes the stream to determine its status, available qualities, and required authentication. For YouTube, this involves the Innertube Player API across multiple client types (TV_DOWNGRADED, WEB, WEB_CREATOR, ANDROID_VR) to find the best available formats. For Twitch, this involves GQL queries to get HLS playlist URLs.

4. **Segment Download** — Moombox downloads the stream as it happens:
   - **YouTube DASH** — Sequential segment download, requesting the next segment as soon as the current one completes. Supports catch-up mode (a configurable pool of parallel segment workers, `segment_workers`, 12 by default) when falling behind.
   - **YouTube HLS** — Playlist polling to discover new segments as they appear.
   - **Twitch HLS** — Playlist polling with segment deduplication.
   - **VOD** — Parallel segment download for already-completed videos.

5. **Live Chat Recording** — In parallel with the video download, Moombox polls YouTube's live chat API or connects to Twitch IRC to capture chat messages. Chat is batched and stored alongside the video data.

6. **Quality Monitoring** — During download, Moombox monitors for quality changes (e.g., the streamer changes resolution mid-stream). If the selected quality disappears, Moombox splits the recording into a new segment at the new quality.

7. **Muxing** — Once the stream ends and all segments are downloaded, Moombox invokes FFmpeg to mux the audio track, video track, and chat data into the final output file. Muxing runs on a background context so it completes even if the user cancels the job.

8. **Verification** — After download completes, Moombox probes the YouTube API to confirm the stream has actually ended (up to 6 checks). This prevents premature termination if there is a temporary interruption.

9. **Output** — The final muxed file is written to the configured output directory with metadata (title, channel, date, duration, etc.) stored in the database.

### Ongoing Operations

Beyond the core download workflow:

- **Web Dashboard** — A chi-based HTTP server with WebSocket push serves a single-page application for monitoring jobs, viewing logs, managing channels, playing back archived videos (with niconico-style chat overlay), creating trim clips, viewing statistics, and configuring settings. Served on port 774.
- **Terminal UI** — A bubbletea-based TUI provides the same capabilities as the web dashboard: job monitoring, log viewing, channel management, settings, and all operational controls. The TUI uses a chord-based keyboard system for efficient navigation.
- **Self-Updating** — Moombox checks GitHub releases for new versions, downloads updates, verifies Ed25519 signatures, and performs a three-step binary swap (`.new` -> current -> `.old`). The launcher/supervisor pattern allows graceful restarts after updates.
- **Cookie Management** — Moombox manages browser cookies for YouTube authentication, supporting automatic extraction from Firefox and Chromium browsers, periodic refresh, and manual import.
- **BotGuard/PO Token** — Moombox runs YouTube's BotGuard challenge under an embedded Node.js + JSDOM + bgutils-js sidecar (real V8, real DOM) to generate Proof of Origin tokens. The Node binary and JS payload are embedded into the Moombox.exe via `go:embed` and extracted on first launch — users do not need a Node install. A goja-VM fallback path is retained for environments where the sidecar fails to start; it produces websafe-fallback tokens which work for most YouTube content but not for PO-token-gated formats.
- **Signature Cipher** — Moombox solves YouTube's signature cipher for format URL decryption using AST analysis with regex fallback, maintaining a 3-VM LRU cache keyed by player.js URL.

## What Moombox Is NOT

These boundaries are important for understanding what is in scope and what is not:

- **Not a general-purpose video downloader.** Moombox supports YouTube and Twitch only. It does not download from Niconico, Bilibili, Crunchyroll, or any other platform. Adding new platforms is not a goal.
- **Not a yt-dlp wrapper or binding.** Moombox reimplements extraction and download logic in Go. It does not shell out to yt-dlp, import yt-dlp's Python code, or depend on yt-dlp being installed. It tracks yt-dlp upstream for awareness of protocol changes only.
- **Not cross-platform by default.** Moombox targets Windows. It may work on Linux or macOS incidentally, but platform-specific code (disk space queries, process management, etc.) assumes Windows. Cross-platform support is added only when explicitly requested.
- **Not a hosted or cloud service.** Moombox runs locally on the user's machine. There is no multi-user support, no cloud deployment model, no container image, no Kubernetes manifold. It is a desktop appliance.
- **Not a media server.** Moombox archives files to disk. The web UI includes basic video playback with chat overlay, but Moombox does not transcode, stream to external clients, integrate with Plex/Jellyfin, or serve as a media library. It is an archiver, not a server.
- **Not a chat bot or stream interaction tool.** Moombox reads chat passively for archival. It does not post messages, moderate chat, or interact with streams in any way.

## Standalone Nature

This point deserves emphasis because it is a common source of confusion. Moombox is a **standalone Go program** that reimplements YouTube and Twitch extraction, authentication, and download logic from scratch. It:

- Implements YouTube's Innertube Player API directly (multiple client types, format selection, auth handling).
- Implements YouTube's DASH and HLS manifest parsing and segment download.
- Implements YouTube's BotGuard challenge/response flow primarily via an embedded Node.js + JSDOM sidecar (real V8 engine), with a Goja-VM fallback path.
- Implements YouTube's signature cipher solving using AST analysis of player.js.
- Implements Twitch's GQL API for stream discovery and HLS playlist retrieval.
- Implements Twitch IRC for live chat capture.
- Implements browser cookie extraction for authentication.

It tracks upstream changes in three reference repositories for awareness:

- **yt-dlp** — YouTube/Twitch extractor changes, format selection logic, cipher updates, cookie handling.
- **BgUtils** — BotGuard protocol changes, PO token generation flow.
- **ejs** — External JS cipher solving strategies.

When upstream changes are relevant (e.g., YouTube changes their API, Twitch rotates their GQL schema), Moombox adapts the solutions independently to fit its Go architecture. It does not copy-paste Python or JavaScript code — it understands the change and reimplements it.

## Design Heritage

Moombox was originally written in TypeScript/Node.js. The Go rewrite began in early 2026 and is complete. Key points about the heritage:

- The original TypeScript codebase is preserved on the `abandoned-nodejs` branch (last version: v1.5.16).
- The Go rewrite incorporated all lessons learned from the TypeScript era: authentication handling, CSP headers, rate limiting, batch database updates, chat bounding, gap detection, and more.
- The Go rewrite is not a line-for-line port. It is a reimplementation that takes advantage of Go's concurrency model, static typing, and single-binary deployment.
- `REWRITE_GUIDE.md` in the repository documents the porting strategy for historical reference.

There is no ongoing relationship between the TypeScript codebase and the current Go code. The TypeScript branch exists purely for historical reference.

## Deployment Model

### Binary Distribution

Moombox is distributed as a single Windows executable. There is no installer, no MSI, no setup wizard beyond what the application itself provides on first run. The user downloads the `.exe` and runs it.

### Runtime Dependencies

The only external runtime dependency is **FFmpeg**, which must be available on the system PATH. FFmpeg is used for muxing audio + video + chat into final output files. The first-run setup wizard validates FFmpeg availability and can guide the user through installation.

### First-Run Experience

On first launch, Moombox presents a setup wizard (available in both web UI and TUI) that handles:

1. FFmpeg validation — checks PATH, offers installation guidance if missing.
2. Browser cookie capture — extracts YouTube cookies from installed browsers for authentication.
3. Initial configuration — output directory, monitoring preferences, and basic settings.

### Launcher/Supervisor Pattern

Moombox uses an environment variable (`_MOOMBOX_CHILD`) to implement a launcher/supervisor pattern:

- **Without the variable** — the process acts as a launcher: it spawns itself as a child process and monitors it. If the child exits with code 42, the launcher respawns it (picking up any new binary from self-updates).
- **With the variable** — the process runs the full application stack.

This pattern enables graceful restarts for configuration changes, self-updates, and recovery. All restart triggers (config change, update, setup wizard, API request) exit with code 42 via `triggerRestart()`. A 10-second force-exit timer ensures the process never hangs during shutdown.

### Self-Update Flow

Moombox checks GitHub releases for new versions. When an update is available:

1. Downloads the new binary.
2. Verifies its Ed25519 signature against a known public key.
3. Performs a three-step rename: new binary -> `.new`, current binary -> `.old`, `.new` -> current path.
4. Triggers a restart (exit code 42), and the launcher respawns with the new binary.

### Network and Storage

- **Default port**: 774 (web dashboard and API).
- **Database**: SQLite with WAL mode, stored alongside the executable.
- **Output**: Configurable output directory for archived files.
- **No cloud dependencies**: Everything runs locally. The only network access is to YouTube/Twitch APIs, GitHub (for updates), and Discord (for optional notifications).

## Cross-References

- [Design Philosophy](design-philosophy.md) — Design priorities, constraints, and decision-making principles
- [Architecture](architecture.md) — System structure, package dependency graph, and data flow
- [Operations](operations.md) — Build process, release workflow, and update mechanics
- Source: [`cmd/moombox/main.go`](../../cmd/moombox/main.go) — Entry point, launcher/supervisor, and service initialization
