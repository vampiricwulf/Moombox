> **Pre-release for validation.** Production users should stay on [v2.5.2](https://github.com/vampiricwulf/Moombox/releases/tag/v2.5.2); the `/releases/latest` endpoint continues to point at the stable line. Supersedes the `v2.6.0-test.1` tag, whose CI run failed because `go-winres` rejected the pre-release suffix — now fixed by stripping it before computing the Windows numeric version.

This build bundles Sprint #1 of the multi-report audit: security hardening, database correctness, cipher resilience, and initial frontend test coverage. All 17 commits build clean and pass `go test -race ./...` plus a new JS test suite.

### Security

- **Tightened CSRF** — `POST/PUT/DELETE` now requires an allowed `Origin` header or the internal token in every network-access mode. The prior localhost/LAN bypass let any local process or same-origin tab call `/api/restart`, first-time `set-password`, and `open-folder` without proof of browser context. Modern browsers already send `Origin` on same-origin mutations, so the Web UI is unaffected; the TUI continues to use the internal token.
- **Client-token cookie TTL configurable** — the `moombox_client` "remember me" cookie defaulted to ~10 years. New `network.client_token_ttl_days` setting; default is 365, range 1–3650. Shorter values reduce blast radius of a leaked cookie.
- **PO token no longer logged raw** — the `/get_pot` response log previously included the first 30 chars of the token. Now logs the first 8 hex chars of its SHA-256 plus length.

### YouTube extraction correctness

- **PO token content binding → `visitorData`** — the four strategy call sites previously passed `ChannelID`, diverging from yt-dlp/`bgutil-ytdlp-pot-provider`. Token caching is now per-visitor, which matches upstream and prevents tokens from being rejected as unbound as YouTube tightens enforcement.
- **PO token injected into Innertube player requests** — `WEB`, `WEB_SAFARI`, `WEB_CREATOR`, and `WEB_EMBEDDED` player requests now carry `serviceIntegrityDimensions.poToken`. `PLAYER_PO_TOKEN_POLICY` is non-required upstream today, but supplying a token is future-proof.
- **Cipher solver 3s timeout** — a malformed or malicious `player.js` with an infinite loop used to hang the decrypt path indefinitely while holding `Solvers.mu`, freezing every download. Bounded by `CipherTimeout` (3s, matching `bgutils.DefaultMintTimeout`); the failed call returns an error so the caller can `InvalidateSolver` and retry.
- **Cipher timeout race fix** — refines the above. The initial `time.AfterFunc` pattern had a race where the interrupt callback could fire after `ClearInterrupt()`, poisoning the next call. Replaced with the goroutine+select pattern that `bgutils.botguard` already uses. New test exercises both the timeout bound and the "subsequent call succeeds" invariant.
- **GenerateIT null-integrity-token → error** — when BotGuard fails to produce an integrity token (typically because the goja shims are broken), Moombox previously cached the `websafeFallbackToken` (`"MpQBGAE"`) as a valid PO token for 6h. YouTube doesn't accept it for authenticated player requests, so the silent fallback degraded downloads without signal. Now errors explicitly and points at the likely VM cause.

### Database

- **PRAGMA user_version** — schema versioning migrated from the custom `schema_version` table to SQLite's built-in `PRAGMA user_version` (schema v11). Existing DBs auto-migrate: the legacy value is carried forward and the table is dropped.
- **Drop deprecated `player_prefs`** — superseded in v9 by `jobs.chat_offset`; v10 drops the dead table and removes it from fresh installs.
- **`UpdateJobFields` hardening** — the field→column map was missing six writable columns (`manually_added`, `allow_non_stream`, `selected_video_itag`, `selected_audio_itag`, `start_time`, `end_time`), so those keys were silently dropped by partial-update callers. Map now covers all 45 writable columns, and unknown keys log a Debug warning so typos surface in local testing. Adds `TestFieldToColumnCoverage` that uses reflection over the `Job` struct to prevent drift.
- **Remove unused `UpdateJob` batch path** — the batched async path had no production callers (only tests); the worker uses `UpdateJobFields` throughout. Removes ~140 lines including the `batchUpdateLoop` goroutine, `updateCh` / `batchDone` channels, and the `close+wait` dance in `Close()`.

### Behavior

- **Auto-resume `COOKIES?` jobs on auth recovery** — when the cookie refresh service detects a platform transitioning from not-authenticated to authenticated, jobs parked in `COOKIES?` on that platform are swept back to `Upcoming` (error cleared) and a notification is sent. Previously required manual resume.
- **Pre-stream chat offsets preserved** — YouTube chat messages posted before a stream goes live have legitimate negative `OffsetMs` values. The old code used `OffsetMs == 0` as a sentinel, which collided with real `OffsetMs == 0` messages and overwrote replay-provided negative offsets. New `HasOffset` field disambiguates; old chat files deserialize to the same semantics they had before.
- **Orphan file deletion re-checks active job** — `DeleteOrphanedFile` now re-queries the owning job's status immediately before `os.Remove`. Refuses deletion if the job is in an active state (`Upcoming`/`Live`/`Downloading`/`Muxing`), closing the narrow race where a user restarts a job between scan and delete.

### Internal

- **Frontend JS tests** — seeds baseline coverage via Node's built-in test runner (zero devDependencies). 39 tests across `filter-parser`, `filter-engine`, and `utils`. Run with `node --test web/tests/*.test.mjs`. See `web/tests/README.md`.
- **Release workflow supports pre-releases** — tags containing `-` (e.g. `v2.6.0-test.2`) are marked as pre-release and don't become `/releases/latest`. The workflow also strips the pre-release suffix when building the Windows numeric version (`2.6.0.0`) since `go-winres` requires pure integer components.
- **Shoelace CDN hardened with SRI** — `index.html` and `login.html` now carry `sha384` integrity hashes for the three jsdelivr references. Closes `frontend-html-css.md` S-13. The initial attempt to vendor the full CDN distribution was reverted; SRI pinning is the lighter and more appropriate fix given Moombox is an online tool.
