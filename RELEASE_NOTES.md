> **Pre-release for validation.** Production users should stay on [v2.5.2](https://github.com/vampiricwulf/Moombox/releases/tag/v2.5.2); the `/releases/latest` endpoint continues to point at the stable line. Extends `v2.6.0-test.4` with TUI config-lock and apiClient fixes and a WebSocket heartbeat fix.

This build bundles Sprint #1 + the first slice of Sprint #2 from the multi-report audit. All 32 commits build clean and pass `go test -race ./...` plus the frontend JS test suite.

### Correctness fixes since test.3

- **Cipher STS cache** — `Solver.InvalidateSolver(playerURL)` now also drops the STS cache entry for that player. Previously the stale `signatureTimestamp` paired with a freshly-compiled solver could trigger a 403 → invalidate → 403 loop. `InvalidateCache()` (all-clear) symmetrically clears both caches now. (audit cipher.md C2)
- **Cipher compile panic recovery** — `getFromPrepared` wraps goja.RunString/NewObject in a deferred recover. A malformed player.js that panics goja (stack overflow, parser edge case) used to kill the download goroutine; now it returns a clean error and the caller invalidates & retries. (cipher.md H5)
- **Engine parallel catch-up resume** — `runParallelCatchUp` was only updating `d.currentSeq` every `ResumeCatchupInterval` (10) segments. A crash in between lost up to 9 segments of work on restart; now updates on every successful write. (engine.md Finding 1, Critical)
- **Engine atomic fields** — `cipherFailureFired`, `lastSegTime`, `lastHeadProbeTime` are now `atomic.Bool` / a new `atomicTime` wrapper. Previously read/written from the download loop AND the parallel-worker result consumer with no synchronization — race-detector-clean now. (engine.md Findings 2, 3)
- **Engine HTTP transport tuning** — shared `engineHTTPClient` now sets `MaxIdleConnsPerHost = ParallelDownloads + 2` (8) so the 6 parallel workers + HEAD probes reuse TCP connections instead of churning handshakes. (engine.md Finding 5)
- **Twitch chat append-failure fallback** — when incremental append fails, the fallback reads the existing file and merges with the current batch before rewriting, instead of overwriting the aggregate with only the last batch. (twitch.md issue #5)

### Correctness fixes added in test.5

- **TUI OnSaveConfig not held under cfgMu** — FFmpeg-path persistence wrote TOML to disk while holding `cfgMu.Lock()`, stalling every reader (job render, settings open, apiClient build) for hundreds of ms. Now snapshots the callback + config under lock, releases, then invokes. (tui.md Finding 2)
- **TUI apiClient rebuilds on HTTPS toggle** — the cached `*http.Client` was never invalidated when the user toggled HTTPS in Settings; subsequent API calls used the wrong transport. Cache now tracks the HTTPSEnabled value it was built against and self-heals on mismatch. (tui.md Finding 3)
- **WebSocket heartbeat timeout hardened** — a transient `ws.send()` throw (e.g. InvalidStateError during a reconnect) was swallowed without advancing any counter, so the 45 s pong timeout could fire on a socket that was fine. Heartbeat now tracks `_lastPingSent` separately and only declares the socket dead when a ping went out AND no pong came back. (frontend-js.md C3)

### Correctness fixes added in test.6

- **Deterministic Cookie header + header-separator sanitisation** — `CookieJar.GetCookieHeader` iterated a `map[string]string`, so successive calls produced different `Cookie:` headers for the same jar (breaking HTTP-level debugging and tripping YouTube endpoints that inspect `__Secure-*` ordering). Now sorted alphabetically. `sanitizeCookieValue` also strips `;` and `,` which terminate cookie pairs at the HTTP header parser — closes a header-injection vector. (cookies.md #1 + #2)
- **PassiveTracker per-tag success** — `ReportSuccess(tag)` previously wiped every failure across every subsystem, letting a flaky single-subsystem success pattern mask a genuine multi-subsystem outage. Now clears only failures for the given tag and keeps the triggered flag stable while the threshold is still met. (small-packages.md question #3)
- **Chat message-ID generator race** — `randomAlphaNum` used `math/rand` globals, which are not concurrent-safe. Multiple parallel chat downloaders could race at `generateMessageID`. Switched to `math/rand/v2`, whose package-level `IntN` is documented as concurrent-safe. (chat.md C10)

### Parallel-agent batch (test.7)

Three agents audited `internal/cookies/`, `internal/youtube/`, and `internal/engine/` in isolated worktrees and committed 34 focused fixes totaling +1,900 / −200 lines. All scoped to their package, all race-detector clean, all audit findings with owner-decision or cross-package implications deferred.

**Cookies** (cookies.md 20 findings):
- Cookie jar preserves state on transient read errors instead of wiping
- SAPISIDHASH restricted to known YouTube origins (known-vector test added)
- `updateCookieFile` honors `Set-Cookie` Domain= attribute (no more guess-from-name drift)
- Chromium setup cleanup, headless anti-automation, navigate-vs-read error distinction, lockfile glob widened to `Singleton*` / `*lockfile*`
- Firefox `cookies.sqlite` opens with `_busy_timeout=2000`; `user.js` disables telemetry rather than bypassing
- Suffix-anchored domain matching everywhere (rejects `.fakegoogle.com.evil.tld`)
- Dead `setupCmd` field removed; refresh error message now names only platforms with cookies
- CDP empty-result falls through all fallbacks and errors if all empty
- Tests for `isRelevantDomain`, `isEssentialCookie`, `deduplicateAndFormat`, `mergeCookieFiles`, and the SAPISIDHASH known-vector

**YouTube** (youtube.md 11 findings):
- `IsAudio` detects audio mime types even when `AudioQuality` is empty
- `collectFormats` no longer mutates caller's slice
- Unknown-metadata sentinels hoisted to named constants
- Formats with failed n-param decryption are dropped rather than emitted with broken URLs
- Innertube requests now retry on truncated body + JSON parse errors (covers transient CDN glitches)
- Upcoming streams classified via `playabilityStatus.liveStreamability` (more accurate than status-string matching)
- `scriptSrc` recognised as a player-URL key; JSON slashes in player URLs are unescaped
- visitor-data regex shared between service.go and watch_page.go
- `adaptiveFormats` preferred over legacy `formats` at the same auth level
- Missing cipher solver now surfaces as a warning, not a silent format drop
- Unused `intPtr` test helper removed

**Engine** (engine.md 14 findings):
- `CalculateSegmentRange` validates time-range inputs (rejects negatives, reversed ranges)
- `ParseDash` uses -1 sentinel for non-numeric representation IDs instead of silently dropping
- `parseMediaPlaylist` falls back to target-duration for zero-duration segments
- `loadResume` rejects empty / corrupt / >7-day-old state
- `Mux` preserves partial output on context cancel (for connectivity-loss resume)
- `connReporter` stored in `atomic.Pointer` — lock-free concurrent reads
- `applyPoTokenQuery` helper extracted; segment-size/error-snippet limits named
- Parallel catch-up stops at first gap instead of writing a hole into the fMP4 stream (closes a real file-corruption risk)
- DASH retry-state constants named instead of inlined literals
- HLS parse failure logs playlist snippet; `fetchSegment` includes error-body snippet in error message
- HLS live loop escalates after repeated same-segment failures (prevents infinite retry)
- 17 new tests across manifest, muxer, resume-state, and connectivity paths

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

### Twitch extraction

- **IRC emote tag indexing** — the IRC emote-tag parser used Go rune indices, but Twitch sends UTF-16 code-unit offsets (same as JavaScript's string indexing). Messages containing characters outside the Basic Multilingual Plane (🎉 emoji, CJK extension ideographs, etc.) before an emote now slice the correct Name. The VOD-chat path already had `utf16Len` — the IRC path now uses matching `unicode/utf16` encoding.
- **GQL batch partial failures tolerated** — Twitch's batched persisted-query endpoint sometimes returns a partial failure (one element errors while others succeed). The previous "return on first error" behaviour failed the whole monitor cycle on a single flaky element. Now errors only when EVERY batch element errored; logs partial failures at Warn so callers' per-element parsing can extract what is available.
- **Monitor duplicate-check fixed** — `TwitchMonitor.checkChannel` called `db.HasActiveJob(jobID)` but the `video_id` column for monitor-created Twitch jobs stores the unprefixed stream ID. The check was dead code surviving only because SQLite's `INSERT OR IGNORE` caught collisions at insert time. Now passes `info.StreamID`.

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
- **`internal/constants` shrunk** — the aspirational "canonical catalog" clashed with reality: ~40 entries had no consumers, and two of the three that DID have local duplicates in consumers (`MaxSegmentRetries`, `SegmentRetryDelayCap`) had different values than the constants package said. Deleted the dead entries; migrated the one exact-value match (`DownloadChunkSize`). File drops from 433 → 214 lines. Consumers now own their own subsystem-specific limits.
