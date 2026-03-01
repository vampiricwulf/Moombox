### Improvements
- Live-update all relative timestamps in web UI (job cards, details panel, file manager) every second instead of only on server push
- Live-update "Starts In" countdown for scheduled streams in details panel
- Manual update check in web UI (Settings > Check Now) now updates the status bar indicator and shows the update dialog
- Manual update check in web UI now broadcasts to all connected web clients and TUI
- Add UU chord in TUI details panel to manually check for updates
- Add "UU to check" / "UU to update" hints in TUI details panel header

### Bug Fixes
- Fix "Last check: 0s ago" on Upcoming jobs never incrementing in web UI
- Fix manual update check in web UI not notifying TUI or other web clients
