### Features

- **Real PO tokens via embedded Node sidecar.** Moombox now ships a bundled Node.js v22 + JSDOM + bgutils-js subprocess that runs YouTube's BotGuard challenge under real V8 — producing genuine integrity tokens instead of just websafe-fallback. The sidecar is `go:embed`'d into Moombox.exe (~36 MB binary growth) and extracts on first launch to `%LOCALAPPDATA%/Moombox/sidecar/` (~3-5s, one-time). Pinned to a Windows Job Object so the child dies with the parent. JSON-RPC over stdin/stdout, no extra ports. Falls back to the existing goja-only path on any sidecar error so PO-token generation never goes completely dark. Disable via `[bgutils] use_sidecar = false` in `config.toml`.
- **Connectivity awareness.** Moombox tracks both passive failure signals (HTTP error patterns from active downloads) and active connectivity probes. The download orchestrator pauses cleanly during outages and resumes from the live edge when the network returns. Job lifecycle no longer trips into `Error` from a transient blip.
- **Watch tracking.** Every Finished job has a `watched` flag. Mark watched/unwatched in the TUI (one chord) or web dashboard (one click); resume_position clears on toggle. Batch operations supported.
- **Probe metadata refresh.** When channels change titles, stream times, or other metadata mid-watch, monitors re-probe and update the job record without re-creating it. Stale "Upcoming" jobs no longer block re-detection of a rescheduled stream.
- **Unified filter system.** TUI and web dashboard share a single filter grammar (status, channel, watched, search). Multi-select filtering combines predicates cleanly. Frontend persists filter state across reloads.
- **Sliding-window session renewal.** Web sessions silently extend on activity instead of forcing a re-login at the half-TTL mark. Cookie remains `HttpOnly` + `Secure` (when behind HTTPS) + `SameSite=Lax`; new opt-in `[network] trust_forwarded_proto` for reverse-proxy deployments where TLS terminates upstream.
- **First-run setup hardening.** Both `/api/setup/complete` and the change-password endpoint require loopback when no admin password is set, so a LAN peer can't claim the admin password before the legitimate user on a `network_access=lan` install.
- **Per-user file ACLs on Windows.** Config dir, cookie file's parent dir, and the sidecar extraction dir all get tightened to current-user-only via `icacls` so the password hash, cookies, and auth tokens aren't readable by other non-admin users on a shared host. Runs once per process, fire-and-forget.

### Improvements

- **TUI overhaul.** Chord system rebuilt around a single `buildMenuItems()` source of truth (24 chords, action menu, hints, and help all stay in lockstep). Action menu uses two-keypress chords (A/R/O/Q prefixes) with a 3-second confirm window for destructive operations. Panel layout, log viewer (`/` search + `n`/`N` nav), filter bar (`F`), settings overlay (`` ` ``), help (`?`). Tab-hide animation pause stops compositor churn when minimized.
- **Web dashboard polish.** Theme bootstrap (light/dark/auto) without flash-of-wrong-theme, persistent restart-required banner when config and runtime drift, OSC 52 stream-URL clipboard copy, AbortController cleanup on every dialog/drag/long-running fetch, WebSocket heartbeat with dead-socket detection.
- **PotProvider single-minter cache.** ONE BotGuard minter per process now serves every content binding for its TTL (was previously one minter per binding — wasted a 2-10s BotGuard run on every new visitor data). Proactive re-mint fires 5min before expiry so user-facing calls never pay the cold-start cost.
- **Cipher solver disk cache.** Player.js extraction results now persist across restarts — fresh starts load preprocessed sig/n functions from disk instead of re-parsing the 2 MB minified player. 14-day TTL.
- **DASH/HLS strategy selection.** Live YouTube streams now prefer DASH manifests (with parallel catch-up + gap-aware re-fetch) over HLS, with explicit fallback when DASH is missing or empty. Trim service captures audio bitrate from the source rather than dropping to FFmpeg's default 128 kbps default.
- **Cookie auto-acquisition.** Browser-managed Chromium and Firefox profiles for cookie refresh now run pinned to a Windows Job Object so a Moombox crash doesn't leak chrome.exe / firefox.exe / msedge.exe processes. Optional DPAPI fallback (off by default per DECISIONS #6) reads cookies directly from the user's real Chromium-family profile when CDP-based refresh fails.
- **Twitch GQL retry hardening.** Per-op rate-limit handling, exponential backoff with `min()` cap, persisted-query hash table tracked against upstream.
- **YouTube `Init` two-tier debounce.** Successful homepage fetch debounces for 1h; failed fetch debounces for 60s (was: 1h on either outcome, so a startup network blip locked the process out of VisitorData refresh for an hour).
- **Notification dispatch panic recovery.** Webhook senders now have explicit defer ordering for `wg.Done` + semaphore release + panic recover so a misbehaving webhook server can't leak the dispatch slot.
- **Rate limiter LRU eviction.** Per-IP rate limiter now evicts the least-recently-used IP when full instead of an arbitrary map entry — adversarial IP churn can no longer push the active user out of the cache.
- **TLS hardening.** ECDHE-only cipher list for TLS 1.2; hot cert reload via `GetCertificate`; cert-SAN-driven WebSocket Origin allowlist.
- **Updater Ed25519 verification.** Self-update now verifies a detached signature on the `.exe` before swapping. Mismatched / missing signatures abort the update with a clear error.

### Bug Fixes

- **PotProvider deadlock.** `cleanupExpired` no longer holds `pp.mu` while running `m.Cleanup()` (which acquires `WebPoMinter.mu` and runs goja shutdown). Concurrent mint-and-cleanup paths can no longer wedge on lock-order inversion.
- **PassiveTracker latch never cleared on idle.** The connectivity-offline latch now self-clears via `pruneOld` once failures age out of the window, instead of staying `true` forever until the next HTTP success fires `ReportSuccess`.
- **Logger Write contract.** Returns `(len(p), nil)` on persistent file-open failure instead of `(0, nil)` — satisfies `io.Writer`'s "must return non-nil error if `n < len(p)`" rule and keeps slog's `MultiWriter` from short-circuiting other sinks.
- **Toast cleanup race.** Removed user-side `sl-after-hide → alert.remove()` listener that raced Shoelace's internal cleanup, throwing `Node.removeChild` errors during dismissal.
- **Cipher Intl regression** (test.42 → test.43). Reverted the Intl-out-of-dom_shim move — preprocessed cache hits no longer fail with `ReferenceError: Intl is not defined`.
- **Local thumbnail handler.** `handleThumbError` no longer adds the square `thumb-avatar` class on fallback for `maxresdefault.jpg`, fixing letterboxed thumbnails on the dashboard. Underlying thumbnail-route relative-path bug also fixed (was doubling `outputDir`).
- **Disk normalize round-trip.** `disk.warn_percent=99` + `disk.critical_percent=99` no longer round-trips through Save → Load with a persistent error every time; the validator now bumps `WarnPercent` down to defaults when the cap doesn't fit.
- **Cookie meta `LastRefresh` JSON.** Switched from `omitempty` (which doesn't work on a `time.Time` struct value) to `omitzero` (Go 1.24+) so a fresh sidecar without a recorded refresh produces clean JSON instead of the misleading `"0001-01-01T00:00:00Z"`.
- **Trim tempdir orphans.** New startup sweep removes `moombox-trim-*` and `moombox-2pass-*` dirs older than 24h from `%TEMP%` — handles the case where a hard process abort bypasses the inline `defer os.RemoveAll`.
- **`Store.Update` rollback.** A failed validation or save now rolls back the in-memory cfg to its pre-`fn` state instead of leaving the bad value in memory while disk stays unchanged.
- **Sidecar HTTP route security.** `/get_pot`, `/invalidate_caches`, and `/invalidate_it` all gate on `LoopbackOnly` AND CSRF-exempt — yt-dlp's bgutil-pot-provider plugin can reach them; nothing outside localhost can.
- **`RateLimiter.Close` non-idempotent.** Internal `sync.Once` so a future double-close path can't panic on close-of-closed-channel.
- **`openBrowserURL` handle leak.** `cmd.Process.Release()` after `Start` matches the symmetric fix in `open-folder` (audit Q-6); no more 1-handle-per-Moombox-lifetime leak.

### Internal

- **Major package splits.** Worker `orchestrator.go` 2055 → 385 lines, `downloader.go` 1430 → 270, TUI `app.go` 2486 → 536, `settings.go` 2143 → 514, worker `jobs.go` 2525 → 1192. No behavior changes; just moves stable APIs into focused files.
- **Routes wiring extracted** to `cmd/moombox/routes_wiring.go`, monitor callbacks to `monitor_callbacks.go`, TUI wiring to `tui_wiring.go`, WS wiring to `ws_wiring.go`. `cmd/moombox/main.go` is now thin.
- **`go fix` sweep across 25 files.** `for range N` (Go 1.22+), `maps.Copy`, `slices.Contains`, `min`/`max` builtins. Toolchain-mechanical only — no behavior changes.
- **Build prerequisites for the sidecar.** `tools/fetch-node` (Go) downloads + SHA-256 verifies + gzips Node v22 LTS Windows x64. `bgutil-sidecar/build.mjs` (Node) tars the production deps + JS source. CI runs both before `go build`. Documented in CLAUDE.md "Build & Test" and `docs/spec/operations.md`.
- **Subprocess JSON-RPC mux** (sidecar) — write-serialized stdin via mutex; readPump matches responses to per-request channels by `reqID`; concurrent mints multiplex cleanly without serialization at the IPC layer.
- **17 web test files** (was 5). httptest round-trips for every API route. Three live-network tests gated on `MOOMBOX_LIVE_BG_TEST=1` exercise real BotGuard against Google's WAA endpoint (sidecar live mint, fallback-on-death, concurrent mints).
- **Ed25519 signing tool** (`cmd/sign`) for release-build signature attachment.
