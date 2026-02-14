# Moombox

> **Note:** This project was 99% written by [Claude Opus](https://claude.ai) (Anthropic's AI model) using [Claude Code](https://claude.ai/code). From architecture decisions to implementation details, the vast majority of the codebase — including the YouTube engine, download pipeline, TUI, web dashboard, BotGuard solver, and this README — was generated through AI-assisted development.

YouTube live stream archiver with a terminal UI and web dashboard. Monitors channels via RSS, detects live streams, and downloads video + live chat automatically.

## Features

- **Channel monitoring** — RSS-based feed polling with regex filtering on titles/descriptions
- **Live stream archiving** — Downloads video segments in real-time with automatic catch-up when falling behind
- **Live chat capture** — Archives chat alongside video, including pre-stream messages from the waiting room
- **Member-only support** — Cookie-based authentication for members-only streams, with automatic cookie refresh
- **Parallel downloads** — Process multiple streams simultaneously
- **Resume on crash** — Periodic state saves allow resuming interrupted downloads
- **Terminal UI** — Full-screen TUI built with Ink (React for CLI) with mouse support, job management, and live logs
- **Web dashboard** — Real-time job monitoring, video player with synchronized chat replay (Niconico-style flying overlay), zip import for external archives
- **Native PO Token generation** — Built-in BotGuard solver (no external server or browser needed)
- **yt-dlp integration** — Built-in PO Token HTTP endpoint compatible with yt-dlp, plus a bundled yt-dlp plugin
- **YouTube cipher decryption** — Native JavaScript implementation of signature decryption
- **Discord notifications** — Webhook notifications for stream events (found, live, finished, errors, etc.)
- **Single executable** — Builds into a self-contained `Moombox.exe` (~72MB) with no external dependencies
- **60fps support** — Prefers 60fps streams when available at the same resolution

## Requirements

**Running the pre-built executable:**
- Windows (x64)
- [FFmpeg](https://ffmpeg.org/download.html) in your PATH (for muxing video + audio)

**Building from source:**
- [Node.js](https://nodejs.org/) 20.x or later
- [FFmpeg](https://ffmpeg.org/download.html) in your PATH

## Quick Start

1. Download `Moombox.exe` from the [latest release](https://github.com/Wulf/Moombox/releases/latest)
2. Place it in a directory of your choice
3. Create a `config.toml` next to it (see [Configuration](#configuration))
4. Run `Moombox.exe`

The TUI launches by default. The web dashboard is always available at `http://localhost:774`.

To add a video manually:
```bash
Moombox.exe add <video_url_or_id>
```

To run without the TUI (web dashboard only):
```bash
Moombox.exe --no-tui
```

## Configuration

Copy [`config.example.toml`](config.example.toml) to `config.toml` in the same directory as the executable (or in `./config/` or `~/.config/moombox/`).

### Key Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `port` | `774` | Web dashboard port |
| `network_access` | `"localhost"` | `"localhost"`, `"lan"`, or `"external"` |
| `log_level` | `"INFO"` | `"DEBUG"`, `"INFO"`, `"WARN"`, `"ERROR"` |
| `downloader.max_video_resolution` | `1080` | Max resolution (based on max of width/height) |
| `downloader.cookie_file` | `"./cookies.txt"` | Netscape-format cookie file |
| `downloader.download_chat` | `true` | Download live chat alongside streams |
| `downloader.prefer_60fps` | `true` | Prefer 60fps when same resolution available |
| `downloader.output_template` | `"${channel}/${start_date} ${title} [${id}]"` | Output path template |
| `tasklist.hide_finished_age_days` | `30` | Days before finished jobs move to Archived |

### Channel Monitoring

```toml
[[channels]]
id = "UCxxxxxxxxxx"                   # YouTube channel ID
name = "Channel Name"                 # Display name (optional)
terms = "(?i)karaoke|singing"         # Regex filter on title + description (optional)
include_non_live_content = false       # Also download regular uploads (default: false)
```

Template variables for `output_template`: `${title}`, `${id}`, `${channel}`, `${start_date}`, `${start_time}`

## Building from Source

```bash
git clone https://github.com/Wulf/Moombox.git
cd Moombox
npm install
npm run build    # Compile TypeScript
npm run start    # Start with TUI + web dashboard
```

For development (runs directly from TypeScript):
```bash
npm run dev
```

To build the standalone executable:
```bash
npm run package  # Creates Moombox.exe (~72MB)
```

## TUI Keyboard Controls

| Key | Action |
|-----|--------|
| Tab | Switch focus between Tasks/Details/Logs |
| Up/Down | Navigate tasks or scroll logs |
| Enter | Expand/collapse archived jobs |
| A | Add video by URL or ID |
| C | Cancel selected job |
| R | Retry failed job |
| D | Delete job (press twice to confirm) |
| F | Cycle status filter (All/Active/Errors/Finished) |
| O | Open output folder (finished jobs) |
| W | Open web dashboard in browser |
| ? | Toggle help overlay |
| Q | Quit |

## Web Dashboard

Available at `http://localhost:774` with:
- Real-time job list with progress updates (via WebSocket)
- Add/cancel/retry/delete jobs
- Job details with embedded video player
- **Player tab** — Replay archived videos with synchronized chat (Niconico-style flying overlay + sidebar, independently togglable)
- **Imports tab** — Upload `.zip` archives containing video + optional chat JSON for playback
- **Archived tab** — Browse finished jobs older than `hide_finished_age_days`
- Live log viewer

## yt-dlp Integration

Moombox includes a built-in PO Token HTTP endpoint compatible with yt-dlp.

### Using the HTTP endpoint directly

```bash
yt-dlp --extractor-args "youtube:player-client=web;po_token=web.gvs+http://127.0.0.1:774/get_pot" <URL>
```

### Using the bundled plugin

The yt-dlp plugin is included at `src/pot-plugin/`. Copy the `yt_dlp_plugins` folder to your yt-dlp plugin directory, or point yt-dlp at it:

```bash
yt-dlp --plugin-dirs path/to/src/pot-plugin <URL>
```

The plugin auto-discovers the Moombox server at `127.0.0.1:774`. To use a different address:

```bash
yt-dlp --extractor-args "youtube:getpot_moombox_base_url=http://HOST:PORT" <URL>
```

## Cookie Setup

Cookies are needed for member-only content. Two options:

### Automatic (recommended)

Enable automatic cookie acquisition in `config.toml`:

```toml
[auto_cookies]
enabled = true
browser_profile_dir = "./browser-profile"
```

Then use the web dashboard Settings page to launch a browser, log into YouTube, and Moombox will manage cookie refresh automatically. Firefox is recommended (cookies are read directly from SQLite). Edge and Chrome are supported as fallback via Chrome DevTools Protocol.

### Manual

Export cookies in Netscape format from your browser and save as `cookies.txt`:

```toml
[downloader]
cookie_file = "./cookies.txt"
```

## Discord Notifications

Send webhook notifications for stream events:

```toml
[[notifications]]
url = "https://discord.com/api/webhooks/YOUR_ID/YOUR_TOKEN"
events = ["live", "finished", "error"]  # Optional filter (default: all events)
```

Available events: `found`, `added`, `scheduled`, `live`, `downloading`, `muxing`, `finished`, `error`, `auth`, `cancelled`

## Job Status Flow

```
Upcoming → Live → Downloading → Muxing → Finished
```

Special states: `Error`, `Cancelled`, `COOKIES?` (member content needs cookie refresh)

## Architecture

See [CLAUDE.md](CLAUDE.md) for detailed architecture documentation, API endpoints, and development notes.

## License

[MIT](LICENSE)

Based on [nosoop/moombox](https://github.com/nosoop/moombox).
