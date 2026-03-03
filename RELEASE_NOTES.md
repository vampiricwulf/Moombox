### Bug Fixes

- **Fix Firefox "already running" error in auto-cookies** — clean stale `parent.lock` files from the custom profile before launching Firefox, preventing the "Firefox is already running, but it's not responding" dialog after a force-killed session. Mirrors the existing Chromium lock file cleanup pattern.
