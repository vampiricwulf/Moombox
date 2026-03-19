### Features

- **Shutdown vs Cancel redesign** — shutdown-interrupted jobs auto-resume on restart; user-cancelled jobs get granular follow-up actions instead of a single Retry
- **Resume action** (A R) — continue a cancelled/errored YouTube download from where it left off, preserving progress and staging files
- **Reinitialize action** (A I) — fresh restart that clears all progress and deletes staging (replaces old Retry)
- **Mux action** (A M) — force-mux whatever segment files exist in staging for cancelled/errored jobs
- New API endpoints: `POST /resume`, `/reinitialize`, `/mux` with staging file detection
- Import chord moved from A I to A Z

### Bug Fixes

- Fix chat `messageCount` header being stale after interruptions — now updates on every flush
- Fix jobs stuck in Muxing status after shutdown — reset to Downloading on startup
- Fix MuxJob leaving job stuck in Muxing if DB lookup fails
- Guard `download_started_at` to preserve original timestamp on resume
