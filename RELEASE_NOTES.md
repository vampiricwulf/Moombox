### Features

- **Auto-Update Checker + Self-Updater** — Moombox now checks GitHub Releases for new versions on startup and every 24 hours. When an update is available, the Web UI status bar shows a green version badge with an update dialog (release notes, "Update Now", "Don't ask again"), and the TUI details panel shows an "⬆ Update!" indicator with a U-U chord keybind to apply. Updating downloads the new binary, replaces the running exe, and relaunches the process automatically. Discord notifications fire when an update is detected.

### Improvements

- **Version display** — Both the Web UI status bar and TUI details panel now show the current version (previously the status API returned a hardcoded `1.0.0-go`).
- **Self-replacement on Windows** — Uses rename-swap (`Moombox.exe` → `.old`, download → `Moombox.exe`) with rollback on failure, then `os.StartProcess` to launch the new binary after graceful shutdown.
- **Config toggle** — `auto_check_updates` (default `true`) in a new `[updates]` config section. Clicking "Don't ask again" in the Web UI sets this to `false` and persists it.
