### Features

- Add watch tracking for downloaded videos — Plex-like resume and watched status in the Web UI
  - Resume dialog when opening a video with a saved position ("Resume from X:XX" or "Start from beginning")
  - Automatic position save every 10 seconds during playback, on pause, and on tab close
  - Auto-mark as watched when reaching the end (within 30s or 95% of duration)
  - Eyeball icon on job thumbnails: filled green (watched), outlined amber (in progress)
  - Watch status pill in job details with 4 states (unwatched, paused at, watched, watched + paused at)
  - Mark Watched / Mark Unwatched buttons in job details and batch action bar
  - Multi-segment (quality-split) playback support for resume and watched detection
- Add `GET /api/jobs/{id}/watch-state` endpoint for mutable player state (watched, resume position, chat offset) — separate from the immutably-cached job endpoint

### Improvements

- `UpdateJobFields` now returns the updated job, eliminating redundant database reads in mux notifications and watch endpoints
- Migrate chat offset from `player_prefs` table to `jobs` table — chat offset is now per-job (not per-video-id), loaded from the watch-state endpoint, and no longer requires a separate API call

### Bug Fixes

- Fix resume dialog not appearing due to browser caching the immutable job response
- Fix watch status pill not updating in real-time during playback
