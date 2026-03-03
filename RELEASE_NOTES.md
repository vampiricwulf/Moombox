### Features

- **UAC elevation support for FFmpeg install** — when running without admin privileges, the install script is shown for review before requesting elevation via UAC prompt. Users can trust and continue, or distrust and manually install instead.
- **Manual FFmpeg install flow** — new fallback path with download link to gyan.dev and custom path validation for users who decline elevated install.
- **FFmpeg version validation** — warns if detected FFmpeg version is below 4.0 (both TUI and Web UI).

### Improvements

- **Install timeouts** — all package manager commands (choco/winget) now share a 5-minute deadline to prevent indefinite hangs.
- **Truncated error output** — package manager error messages are capped at 500 bytes to avoid flooding the UI with megabytes of log output.
- **FFmpeg check caching** — GET `/api/ffmpeg/check` now uses the cached result (10s TTL) instead of spawning `ffmpeg -version` on every request.
- **TUI callback cleanup** — `OnCheckFFmpeg` now reuses `CheckFFmpegCached` instead of reimplementing bare `exec.Command` with no timeout.
- **API route simplification** — all routes renamed from `/api/v1/` to `/api/` (no v2 planned); removed legacy `APIAliasMiddleware`.
