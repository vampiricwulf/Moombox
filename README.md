# Moombox

> **Note:** This project was 99% written by [Claude](https://claude.ai) (Anthropic's AI model) using [Claude Code](https://claude.ai/code). From architecture decisions to implementation details, the vast majority of the codebase — including the YouTube engine, download pipeline, TUI, web dashboard, BotGuard solver, and this README — was generated through AI-assisted development.

YouTube and Twitch live stream archiver with a terminal UI and web dashboard. Monitors channels, detects live streams, and downloads video + live chat automatically.

Written in Go. Single binary, no runtime dependencies beyond FFmpeg.

I kept the Moom because of Nanashi Mumei being my oshi. I might change it to a different name related to a certain orca in time, but for now, it's just Moombox.

## Features

### Core
- **YouTube + Twitch** — Monitors and downloads live streams from both platforms
- **Channel monitoring** — Independent RSS feed polling (YouTube), DECAPI polling (YouTube), and GQL polling (Twitch) with regex filtering on titles and descriptions
- **Live stream archiving** — Downloads DASH/HLS video segments in real-time with automatic parallel catch-up when falling behind
- **Live chat capture** — Archives chat alongside video (YouTube live chat + Twitch IRC), including pre-stream messages from the waiting room
- **VOD downloads** — Download regular videos and post-live DVR recordings with parallel segment fetching
- **Resume on crash** — Periodic state saves allow resuming interrupted downloads without data loss
- **Parallel downloads** — Process multiple streams simultaneously with configurable concurrency
- **Quality monitoring** — Probes stream quality every 30 seconds during live downloads. If the resolution or framerate changes mid-stream, automatically muxes the current segment and starts a new one — no data lost, no mismatched frames

### Advanced Download Options
- **Manual format selection** — Choose specific video and audio formats per download, or select "None" for video-only/audio-only
- **Timestamp selection** — Download a specific time range of a stream (start/end time), with frame-accurate trimming via FFmpeg re-encode
- **Post-download trimming** — Create trimmed clips from finished downloads with CRF-based encoding for optimal quality/size
- **60fps support** — Prefers 60fps streams when available at the same resolution

### Updates
- **Auto-updater** — Checks GitHub for new releases and downloads updates in-place. Apply updates from the web dashboard or TUI — the app restarts automatically with the new version
- **Launcher/supervisor** — A lightweight launcher process manages the app lifecycle. Config changes, updates, and setup wizard restarts are seamless — no terminal flicker, no process chain buildup

### Security
- **HTTPS support** — Auto-generated self-signed certificates with dual-protocol (TLS + plain HTTP) on a single port
- **Password authentication** — Optional password protection for external access (scrypt-hashed, session-based)

### Authentication
- **Member-only support** — Cookie-based authentication for members-only streams (YouTube + Twitch)
- **Automatic cookie refresh** — Keeps sessions alive with periodic background refresh with real API-based validation
- **Auto cookie setup** — Launch a browser from the dashboard or TUI, log in, and cookies are extracted automatically (Firefox recommended, Chromium supported)
- **Auto-reacquisition** — When cookies expire or are deleted, Moombox automatically re-launches the browser to reacquire them

### Interfaces
- **Terminal UI** — Full-screen TUI built with [Charmbracelet's Bubble Tea suite](https://github.com/charmbracelet) ([Bubble Tea](https://github.com/charmbracelet/bubbletea) + [Bubbles](https://github.com/charmbracelet/bubbles) + [Huh](https://github.com/charmbracelet/huh) + [Lip Gloss](https://github.com/charmbracelet/lipgloss)) with mouse support, keyboard navigation, job management, settings editor, and live logs
- **Web dashboard** — Real-time job monitoring at `localhost:774` (HTTPS when external) with video player, synchronized chat replay (Niconico-style flying overlay + sidebar), settings management, and zip import
- **Mobile responsive** — Web dashboard adapts to tablets and phones with reorganized layouts and touch-friendly controls
- **Statistics dashboard** — At-a-glance disk usage, total archive size, platform breakdown (YouTube vs Twitch), job counts, and activity metrics
- **First-run wizard** — Built-in setup wizard in both TUI and web dashboard for initial configuration (quick mode or 8-step advanced walkthrough)
- **Process restart** — Restart Moombox from the TUI or web dashboard when settings require it

### Integration
- **Native PO Token generation** — Built-in BotGuard solver using [Goja](https://github.com/dop251/goja) (pure-Go JavaScript engine, no CGo or V8)
- **yt-dlp compatibility** — Built-in PO Token HTTP endpoint and bundled yt-dlp plugin
- **YouTube cipher decryption** — Native implementation of signature and n-parameter decryption via Goja
- **Webhook notifications** — Notifications for stream events via any webhook-compatible service (Discord, Slack, ntfy, etc.)
- **Single binary** — Compiles to a single executable with embedded web assets, no external runtime dependencies
- **Built-in FFmpeg installer** — Install FFmpeg via Chocolatey or Winget directly from the setup flow, with UAC elevation support and script review for non-admin users

## Requirements

**Running the pre-built executable:**
- Windows (x64)
- [FFmpeg](https://ffmpeg.org/download.html) in your PATH (for muxing video + audio)

**Building from source:**
- [Go](https://go.dev/dl/) 1.25 or later
- [FFmpeg](https://ffmpeg.org/download.html) in your PATH

## Quick Start

1. Download `Moombox.exe` from the [latest release](https://github.com/vampiricwulf/Moombox/releases/latest)
2. Place it in a directory of your choice
3. Run `Moombox.exe`

A built-in setup wizard walks you through first-time configuration on launch. The TUI opens by default — press **W** to open the web dashboard in your browser.

To add a video manually:
```bash
Moombox.exe add <video_url_or_id>
```

To run without the TUI (web dashboard only):
```bash
Moombox.exe --no-tui
```

Other flags:
```bash
Moombox.exe --version          # Show version and exit
Moombox.exe --config path.toml # Use a specific config file
Moombox.exe --log-level DEBUG  # Override log level
```

## Configuration

Moombox includes a built-in first-time setup wizard — no manual configuration is necessary. All settings can be changed at any time from the **Settings** page in the web dashboard or TUI.

For advanced users, a [`config.example.toml`](config.example.toml) reference is included. Moombox looks for `config.toml` in the current directory, `./config/`, or `~/.config/moombox/`.

### Key Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `port` | `774` | Web dashboard port |
| `network_access` | `"localhost"` | `"localhost"`, `"lan"`, or `"external"` |
| `log_level` | `"INFO"` | `"DEBUG"`, `"INFO"`, `"WARN"`, `"ERROR"` |
| `downloader.max_video_resolution` | `1080` | Max resolution (based on max of width/height, handles portrait) |
| `downloader.cookie_file` | `"./cookies.txt"` | Netscape-format cookie file |
| `downloader.download_chat` | `true` | Download live chat alongside streams |
| `downloader.prefer_60fps` | `true` | Prefer 60fps when same resolution available |
| `downloader.num_parallel_downloads` | `2` | Simultaneous download jobs |
| `downloader.output_template` | `"${channel}/${start_date} ${title} [${id}]"` | Output path template |
| `feed_check_interval` | `10` | Minutes between RSS feed checks (also accepts `"10m"`) |
| `twitch_check_interval` | `15` | Seconds between Twitch GQL live-status checks (with jitter) |
| `tasklist.hide_finished_age_days` | `30` | Days before finished jobs move to Archived (also accepts `"30d"`) |

### Channel Monitoring

```toml
# YouTube channel
[[channels]]
id = "UCxxxxxxxxxx"                   # YouTube channel ID
name = "Channel Name"                 # Display name (optional)
terms = "(?i)karaoke|singing"         # Regex filter on title + description (optional)
include_non_live_content = false       # Also download regular uploads (default: false)
enabled = true                        # Toggle monitoring on/off (default: true)

# Twitch channel
[[channels]]
id = "channelname"                    # Twitch login name
name = "Channel Name"
platform = "twitch"                   # Required for Twitch channels
quality_preference = "best"            # "best", "720p", "480p", or "audio_only"
```

Template variables for `output_template`: `${title}`, `${id}`, `${channel}`, `${start_date}`, `${start_time}`

## Building from Source

```bash
git clone https://github.com/vampiricwulf/Moombox.git
cd Moombox
go build -o moombox ./cmd/moombox
./moombox
```

Build with version info (for releases):
```bash
go build -ldflags "-s -w -X main.version=1.0.0 -X main.commit=$(git rev-parse --short HEAD)" -o moombox ./cmd/moombox
```

To run tests:
```bash
go test ./...             # Run all tests
go test -v ./...          # Verbose output
go test -v -run TestName ./internal/package/...  # Single test
go vet ./...              # Static analysis
```

## Terminal UI

The TUI displays a three-panel layout: task list + job details (top) and live log viewer (bottom). Tab switches focus between panels — the focused panel expands to take more space.

### Keyboard Controls

| Key | Action |
|-----|--------|
| Tab | Switch focus between Tasks/Details/Logs |
| Up/Down | Navigate tasks or scroll panels |
| Enter | Expand/collapse archived jobs |
| A | Add video (Tab cycles: URL, Advanced, Import) |
| C | Cancel selected job |
| R | Retry failed job |
| D | Delete job (press twice to confirm) |
| F | Cycle status filter (All/Active/Errors/Finished) |
| T | Trim a finished video |
| O | Open output folder (finished jobs) |
| W | Open web dashboard in browser |
| ` | Open settings panel |
| ? | Toggle help overlay |
| Q | Quit |

Mouse support: click to select tasks, scroll wheel to navigate.

### Add Video Dialog

Press **A** to open the Add Video dialog. By default it's in quick-add mode — paste a URL and press Enter. Press **Tab** to cycle through modes:

- **Quick Add** — Paste a YouTube or Twitch URL and press Enter
- **Advanced** — 5-step wizard: URL, Video Format, Audio Format, Timestamps, Confirm
- **Import** — Upload a `.zip` archive with video + optional chat JSON

## Web Dashboard

Available at `http://localhost:774` (auto-upgrades to HTTPS for external access) with:

- **Tasks tab** — Real-time job list with progress bars, add/cancel/retry/delete actions, and job details with embedded YouTube player
- **Advanced Options** — Format selection (video/audio dropdowns with codec badges) and timestamp range when adding videos
- **Archived tab** — Browse finished jobs older than `hide_finished_age_days`
- **Player tab** — Replay archived videos with synchronized chat:
  - Niconico-style flying chat overlay (togglable)
  - Sidebar chat panel with auto-scroll and search (togglable)
  - Superchat highlighting and emoji support
  - Multi-segment playback with cross-segment seeking for quality-split recordings
- **Imports tab** — Upload `.zip` archives containing video + optional chat JSON for playback in the Player tab
- **Stats tab** — Disk usage, archive size, platform breakdown, job counts, and recent activity
- **Logs tab** — Live log viewer
- **Settings** — General config, downloader settings, channel management (YouTube + Twitch), webhook notifications, password security, yt-dlp plugin installation, and auto-cookie setup

### API

All endpoints are available under `/api/`. Real-time updates are delivered via WebSocket at `/ws`.

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/jobs` | List active jobs (supports pagination) |
| GET | `/api/jobs/archived` | List archived jobs |
| GET | `/api/jobs/{id}` | Get job details |
| GET | `/api/jobs/{id}/video` | Stream video file (Range requests supported) |
| GET | `/api/jobs/{id}/chat` | Get chat data |
| GET | `/api/formats/{videoId}` | Get available formats for a video |
| POST | `/api/jobs` | Add new job |
| POST | `/api/jobs/{id}/cancel` | Cancel job |
| POST | `/api/jobs/{id}/retry` | Retry failed job |
| DELETE | `/api/jobs/{id}` | Delete job |
| POST | `/api/jobs/{id}/trims` | Create trimmed clip |
| POST | `/api/import` | Import zip archive |
| GET | `/api/config` | Get configuration |
| PUT | `/api/config` | Update configuration |
| GET | `/api/status` | Server status |
| POST | `/api/restart` | Restart server (loopback only) |
| POST | `/api/auth/login` | Authenticate with password |
| POST | `/api/auth/set-password` | Set or change password |
| GET | `/api/auth/status` | Check auth status |
| GET | `/api/ffmpeg/check` | Check if FFmpeg is available |
| POST | `/api/ffmpeg/install` | Install FFmpeg via package manager |
| GET | `/api/update/status` | Check for available updates |
| POST | `/api/update/apply` | Download and apply update |
| GET | `/api/stats` | Get download statistics |

WebSocket messages: `initial_state`, `jobs_update`, `job_update`, `check_timers`, `log`, `pong`

## yt-dlp Integration

Moombox includes a built-in PO Token HTTP endpoint compatible with yt-dlp.

### Using the HTTP endpoint directly

```bash
yt-dlp --extractor-args "youtube:player-client=web;po_token=web.gvs+http://127.0.0.1:774/get_pot" <URL>
```

### Using the bundled plugin

The yt-dlp plugin can be installed from the web dashboard's Settings > Integrations page.

### POT Provider Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/get_pot` | Generate PO token (loopback only) |
| POST | `/invalidate_caches` | Clear all caches (loopback only) |
| POST | `/invalidate_it` | Invalidate integrity tokens (loopback only) |
| GET | `/ping` | Health check |

## Cookie Setup

Cookies are needed for member-only content (YouTube) and authenticated access (Twitch). Two options:

### Automatic (recommended)

Enable automatic cookie acquisition in `config.toml`:

```toml
[auto_cookies]
enabled = true
browser_profile_dir = "./browser-profile"
```

Then use the web dashboard Settings page or TUI settings panel to launch a browser, log into YouTube and/or Twitch, and Moombox will manage cookie refresh automatically. Firefox is recommended (cookies are read directly from SQLite). Edge and Chrome are supported as fallback via Chrome DevTools Protocol.

### Manual

Export cookies in Netscape format from your browser and save as `cookies.txt`:

```toml
[downloader]
cookie_file = "./cookies.txt"
```

## Webhook Notifications

Send webhook notifications for stream events:

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN"
events = ["live", "finished", "error"]  # Optional filter (default: all events)
```

Available events: `found`, `added`, `scheduled`, `live`, `downloading`, `muxing`, `finished`, `error`, `auth`, `cancelled`

## Job Status Flow

```
Upcoming -> Live -> Downloading -> Muxing -> Finished
```

Special states: `Error`, `Cancelled`, `COOKIES?` (member content needs cookie refresh)

## Architecture

Moombox is a Go application compiled to a single binary. All code lives under `internal/` with web assets embedded via `go:embed`.

```
Monitors (RSS/DECAPI/Twitch) -> Job Database (SQLite) -> Download Worker -> YouTube/Twitch API
                                                                |
                                 Web Dashboard <- FFmpeg Muxer <- Segment Downloads (DASH/HLS/VOD)
                                 (localhost:774)      |
                                                Chat Downloader (YouTube live chat / Twitch IRC)
```

Key components:
- **YouTube engine** — Multi-client Innertube API strategy with native signature decryption (AST-based cipher solver via [Goja](https://github.com/dop251/goja)) and BotGuard PO Token generation
- **Twitch engine** — GQL-based stream metadata, HLS segment downloading, IRC live chat, and VOD chat replay
- **Download pipeline** — SegmentDownloader with parallel catch-up mode (6 concurrent segments), head sequence tracking, resume state, and gap detection
- **Chat system** — YouTube live chat polling + Twitch IRC with memory bounding, stale continuation recovery, and replay support
- **Database** — SQLite with WAL mode, batch updates, and pub/sub for real-time UI updates
- **Web server** — [chi](https://github.com/go-chi/chi) router with WebSocket real-time updates, CORS, CSP, rate limiting, password auth, and IP-based access control
- **TUI** — Built on [Charmbracelet's full Bubble Tea suite](https://github.com/charmbracelet): [Bubble Tea](https://github.com/charmbracelet/bubbletea) framework, [Bubbles](https://github.com/charmbracelet/bubbles) components, [Huh](https://github.com/charmbracelet/huh) forms, and [Lip Gloss](https://github.com/charmbracelet/lipgloss) styling

See [CLAUDE.md](CLAUDE.md) for comprehensive architecture documentation including initialization order, package dependency graph, code patterns, and design decisions.

## License

[MIT](LICENSE)

Based on [nosoop/moombox](https://github.com/nosoop/moombox).
