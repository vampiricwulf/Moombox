### Features

- Add watch tracking for downloaded videos — Plex-like resume and watched status in the Web UI
  - Resume dialog when opening a video with a saved position ("Resume from X:XX" or "Start from beginning")
  - Automatic position save every 10 seconds during playback, on pause, and on tab close
  - Auto-mark as watched when reaching the end (within 30s or 95% of duration)
  - Eyeball icon on job thumbnails: filled green (watched), outlined amber (in progress)
  - Watch status pill in job details with 4 states (unwatched, paused at, watched, watched + paused at)
  - Mark Watched / Mark Unwatched buttons in job details and batch action bar
  - Multi-segment (quality-split) playback support for resume and watched detection

### Improvements

- `UpdateJobFields` now returns the updated job, eliminating redundant database reads in mux notifications and watch endpoints
