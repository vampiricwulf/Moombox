### Bug Fixes

- **Fixed TUI restart freeze** — R P P chord, settings restart, and setup wizard completion no longer freeze the program (deadlock on bubbletea's internal message channel)
- **Fixed signature verification error stutter** — no longer shows "signature verification failed: signature verification failed: signature verification failed"

### Improvements

- **Status bar redesign** — compact 28px bar with vertical dividers for visual grouping, version moved to right side, consistent `Xm Xs` countdown format, tighter typography
- **Disk indicator as warning only** — moved to right side, only appears when disk usage is at warning or critical level with HDD icon
- **Cleaner status bar text** — connection shows icon only when connected (text kept for disconnected alert), removed redundant "Next:" prefix from countdown timers
- **Mobile status bar fixes** — horizontal timers with separators, fixed refresh button spin animation
