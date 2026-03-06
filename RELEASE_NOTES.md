### Features

- **Persistent client tokens** — remote users stay logged in across server restarts. Every successful login issues a scrypt-hashed token stored in SQLite; on restart, the middleware transparently creates a fresh session from the persistent cookie. Tokens are manageable from both the TUI (`A K` chord) and Web UI (Settings → Connected Clients). Logout revokes that browser's token; password change/remove clears all tokens.

### Improvements

- Setup wizard: server-side yt-dlp plugin install (runs before restart instead of fire-and-forget client fetch), HTTPS toggle in web setup, consolidated FFmpeg path-check helpers
- Progress reporting now shows real counts on every event instead of artificial increments (Twitch chat was every 100, VOD chat every 500, catch-up segments every 10)
- ProgressTracker coalesce interval reduced from 100ms to 16ms to match TUI 60fps tick rate

### Bug Fixes

- Fix setup wizard passing stale HTTPS flag from config instead of the wizard's own value
- Fix setup wizard cookie extraction freezing the TUI (now runs async)
- Fix setup wizard silently swallowing auto-cookie errors (now surfaces them in the UI)
- Fix re-login accumulating stale client tokens (old token is revoked before issuing a new one)

### Internal

- Database schema bumped to v6 (`client_tokens` table with prefix index)
- Token hash format uses `salt_hex:hash_hex` (no `scrypt:` prefix) to distinguish from password hashes
