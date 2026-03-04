### Features

- Shift+click the cookie refresh button in the web UI to trigger a full browser-based cookie refresh (requires auto-cookie acquisition enabled)

### Improvements

- Removed deprecated `-no-remote` flag from Firefox commands — unnecessary with separate profiles since Firefox 67, officially removed in Firefox 131
- Removed explicit `--headless` from Firefox screenshot command to avoid known hang bug when combined with `--screenshot` (which already implies headless)
- Standardized Firefox flags to double-dash format (`--profile` instead of `-profile`)
