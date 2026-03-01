### Features

- **Updates settings** — New "Updates" section in both Web UI and TUI settings with auto-check toggle and a "Check Now" button (Web UI) to manually trigger an update check.
- **Disk settings** — New "Disk" section in both Web UI and TUI settings to configure warning threshold (default 90%) and critical threshold (default 95%) without editing config.toml.

### Improvements

- **Cookie refresh interval** — Now configurable via the Cookies section in both Web UI and TUI settings (default: 360 minutes / 6 hours).
- **TLS fields in TUI** — TLS certificate and key path fields are now available in the TUI Network settings (previously Web UI only).
- **Disk threshold validation** — Server-side validation for disk warning/critical percentages (must be 1–100).

### Bug Fixes

- **Update restart terminal corruption** — After applying an update, the restarted process now launches in a fresh console window instead of inheriting the old one, fixing broken terminal state (stuck alt screen, partial raw mode) when quitting the TUI.
