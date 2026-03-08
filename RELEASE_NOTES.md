## Bug Fixes

- Fix first-run setup wizard cookie dialogs hidden behind overlay (z-index 10000 vs Shoelace dialog default 800)
- Fix toast notifications invisible during setup (z-index 10000 vs Shoelace toast default 950)
- Fix Escape key during cookie setup orphaning browser process (missing `sl-request-close` handler)
- Fix `PersistPlatforms` prematurely creating config file during first-run setup
- Fix "Localhost Only" network access refusing to save (validation accepted `"local"` but UI sends `"localhost"`)
- Fix inability to clear optional path fields once set (`|| undefined` pattern stripped empty strings)
- Fix notification event toggling silently losing changes (notifications excluded from save payload)
- Fix cookie `Secure` flag mismatch on `clearSessionCookie`/`clearClientCookie` (could prevent proper deletion over HTTPS)
- Fix settings save overwriting channels/downloader fields modified concurrently from TUI

## Improvements

- Add `min`/`max` HTML attributes to all 15 numeric settings inputs for client-side validation
- Add server-side validation for `segment_retry_delay_cap` (1–300), `segment_live_check_retries` (1–100), `feed_check_interval` (1–1440), `hide_finished_age_days` (≥0), `refresh_interval` (≥10)
- Add `ffmpeg_path` path traversal validation (matching all other path fields)
- Add disk threshold cross-field validation (critical must exceed warning)
- Add `feed_check_interval` upper bound (1440 minutes)
- Update help text with accurate ranges and defaults across all settings fields
- Update auto-cookie browser support text to list all 6 supported browsers (Firefox, Chrome, Brave, Edge, Opera, Waterfox)
- Unify `network_access` canonical value to `"localhost"` across middleware, auth, validation, and tests
