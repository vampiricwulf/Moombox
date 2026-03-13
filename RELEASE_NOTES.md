### Bug Fixes

- **Fixed DASH segment 403 errors** — YouTube renamed the n-param URL class (`g.fb` → `g.sB`); cipher extractor now dynamically detects the class name instead of hardcoding it
- **Fixed infinite retry loop on failed downloads** — deferred progress events and catch-up bookend events no longer falsely reset the safety counters (`consecutiveLiveChecks`, `lastSegmentTime`), so stalled downloads now properly time out

### Improvements

- **Added Logger to all segment downloaders** — DASH, HLS, and Twitch segment download errors are now visible in the log (previously went to a no-op logger)
