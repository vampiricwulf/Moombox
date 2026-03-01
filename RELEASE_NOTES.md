### Features

- **Setup wizard split**: First-run setup now offers Quick Setup (cookies + channels with defaults) or Advanced Setup (full 8-section config walkthrough). Both trigger a clean app restart on completion.
- **Post-setup FFmpeg validation**: After setup, FFmpeg is checked automatically. If missing, an overlay offers one-click install via Chocolatey or winget, custom path entry, or quit. Installs the shared FFmpeg build (`ffmpeg-shared`).

### Improvements

- **Monitors wake on channel add**: Feed, DECAPI, and Twitch monitors now start immediately when channels are added after boot, instead of staying idle until restart.
- **Config validation hardened**: All config defaults are validated on load — 4K default resolution, cookie refresh floored to 10 minutes, invalid values reset to defaults.
- **Twitch check interval**: Minimum lowered from 5 seconds to 1 second.
- **Settings help text**: Corrected DECAPI/Twitch interval ranges and channel filter pattern descriptions (clarifies per-monitor matching behavior).

### Bug Fixes

- **Cookie refresh interval**: Floored to 10 minutes in validation to prevent excessive refresh cycles.
- **yt-dlp plugin HTTPS**: Fixed `NoSupportingHandlers` error when HTTPS is enabled — plugin now uses Python stdlib urllib directly for localhost calls instead of unsupported `ssl_context` extension.
- **Terminal title**: Replaced direct stdout write with `tea.SetWindowTitle` to prevent TUI corruption.
- **Channel editor crash**: Fixed index out-of-bounds panic when pressing Esc then Enter in channel editor with no channels.
- **Windows PATH refresh**: Now merges registry PATH with process-specific entries instead of replacing, and correctly expands `%VAR%` references.
