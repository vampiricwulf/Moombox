> **Pre-release for validation.** Production users should stay on [v2.5.2](https://github.com/vampiricwulf/Moombox/releases/tag/v2.5.2); the `/releases/latest` endpoint continues to point at the stable line. Extends `v2.6.0-test.17` with another `internal/engine/` pass.

This build bundles Sprint #1 + Sprint #2 work plus twelve batches from the multi-report audit. All 287 commits since `f3ac3fb` (v2.5.2) build clean and pass `go test -race ./...` plus the frontend JS test suite.

### Manual batch 12 (test.18)

Engine-level correctness + bandwidth-waste fixes:

- **engine #2** — `cipherFailureFired` now uses `CompareAndSwap(false, true)` instead of `Load+Store`. The audio and video downloaders share a `cipherSolver`, so concurrent 403s could both pass the Load check and both fire `OnCipherFailure`, duplicating the `InvalidateSolver` work. CAS guarantees exactly one fire per downloader instance.
- **engine #14** — `probeFileSize` no longer drains the response body when the server ignores the `Range` header. The legacy code unconditionally `io.Copy`'d to `io.Discard` before checking the status code; if a CDN responded 200 OK with the full file, we'd download a multi-GB VOD just to throw it away. Reordered: status check first, drain only on 206 Partial.
- **engine #26** — replaced eight stale "matches TS" / "TypeScript" comments with explanations of the actual reasoning (head probe uses a separate timer to avoid resetting staleness detection, init segment failure is non-fatal because FFmpeg can usually demux without it, etc.). Comments now serve readers who never saw the original TypeScript codebase.

### Manual batch 11 (test.17)

Targeted concurrency hazards in the worker + engine packages — real `-race`-visible bugs and shutdown-correctness fixes:

- **worker #2** — `ChatDownloader.OnProgress` is now an unexported field protected by an `RWMutex` with `SetOnProgress`/`callOnProgress` helpers. The orchestrator overwrites this callback after the chat goroutine has already started polling, so the previous public-field design was a Go memory-model data race on a func value. Three call sites in the worker package updated.
- **worker #7** — Background segment-mux goroutines now run with a 5-minute bounded context instead of `context.Background()`. The `defer segmentMuxWg.Wait()` at the top of the live download loop blocks return until every spawned mux goroutine completes; with an unbounded context, a stuck FFmpeg pinned shutdown indefinitely. User-cancel still preserves partial output (the ctx is detached from parent) but the worst case is bounded.
- **worker #25 + #26** — Catch-up parallel and HLS-VOD parallel pools no longer leak workers on early consumer return. Both pools deadlocked on a write error: consumer returns mid-stream, workers blocked on full results channel, `wg.Wait` never completes, closer goroutine never fires. Added a per-call `done` channel closed by defer at function exit so workers unblock cleanly.
- **worker #19** — Doc fix in `docs/spec/architecture.md`: the terminal-status block claimed `StatusMuxing` was terminal; the actual implementation excludes it (`enqueueExistingJobs` resets interrupted muxes back to `Downloading`). Doc updated to match the canonical code behavior.

### Manual batch 10 (test.16)

Continued cleanup on `internal/cookies/` — focused on real-world correctness (browser detection, leak prevention) plus test coverage of recently shipped fixes:

- **cookies #15** — `FlexDuration.AsDuration(base)` helper plus call-site fixes in `cmd/moombox/main.go` and `internal/monitor/feed.go`. The `time.Duration(d.Minutes()) * time.Minute` pattern was truncating fractional values to int64 nanoseconds before multiplying — a 1.5-minute interval collapsed to 1 minute. Same bug surfaced in two places.
- **cookies #7 + #30 + #31** — added LibreWolf, Zen, Vivaldi, and Thorium to browser auto-detection. Privacy-focused forks now detect ahead of mainstream browsers in their family. Includes Windows registry ProgID matchers and PATH/install-path candidates.
- **cookies #8** — wired Windows Job Objects into both Chromium setup and refresh paths. Previously only the Firefox refresh path used `KILL_ON_JOB_CLOSE`; a Moombox crash would orphan headless chrome.exe processes that held the profile lock and silently broke the next refresh.
- **cookies #52** — dropped the `jobCounter` global. Job Objects are now anonymous (lpName=NULL) instead of named — the per-process counter served only to avoid collisions on naming that nothing outside the package consumed.
- **cookies #59** — locked down `GetCookieHeader`'s deterministic sort (originally fixed in #1) with an explicit ordering test. 50-iteration byte-equality plus alphabetical assertion.
- **cookies #57** — extracted `parseDefaultBrowserProgID` from `detectDefaultBrowserWindows` and added 15-case fixture-driven coverage. Locks in the new ProgID matchers without needing a real Windows registry.

### Manual batch 9 (test.15)

Small cleanup pass on `internal/engine/`:

- **engine F38** — deleted `Muxer.MuxEncode` (CRF re-encode wrapper with zero production callers; `Mux` is the only path).
- **engine F39 + F40 + F41** — unexported `TotalDurationSec`, `TotalSegmentCount`, `SegmentDurationSec`, `EstimateSegmentCount`. All four were test-only; lowercasing them preserves the test coverage of the underlying segment-math (useful if a DASH-VOD progress estimator is ever wired) while shrinking the public API surface by four.

### Manual batch 8 (test.14)

- **config F14 + F21** — `PoToken` and `VisitorData` are now `json:"-"` so they stop leaking via `GET /api/config`. Session-scoped secrets should be treated like `PasswordHash`; the prior tags shipped them to any authenticated remote client.
- **config F26** — hoist `validQualities` into package-level `validQualityPreferences`; no more 15-string map rebuilt on every `validate()` call.
- **config F29** — `ChannelTerms.MarshalTOML` uses `bytes.TrimRight(buf, "\n")` instead of `bytes.TrimSpace` so legitimate leading/trailing whitespace inside a quoted string literal is preserved.
- **config F35** — removed unused `ChannelTerms.IsEmpty` (replaced by `len(Patterns()) == 0` in tests).
- **worker F13** — promoted `ffprobe failed` log from Debug to Warn; a broken FFmpeg install no longer degrades silently through the format-metadata fallback path.
- **worker F56** — removed dead `sanitizeFilename` helper (only referenced by its own tests; production uses `config.ResolveTemplate`).

### Manual batch 7 (test.13)

Again manual rather than agent-dispatched — the Agent-tool worktree-base bug has become reliable enough that we're pattern-matching around it. Net: **14 more commits** across three subsystems.

**goja** (goja.md — 6 commits):

- **C1 + API1** `crypto.getRandomValues` was broken from the moment `RegisterDOMShim` returned — the JS closure looked up `__cryptoRandBytes` on `globalThis` at call time, but the Go cleanup block cleared the global to `undefined` immediately after shim registration. Every subsequent call threw `TypeError: __cryptoRandBytes is not a function` and silently forced callers down the Math.random fallback path. Fixed by capturing the native CSPRNG bridges into closure-local variables (`_randBytes`, `_randomUUID`) *before* the cleanup runs, plus stubbing the common `crypto.subtle.*` methods to return rejected Promises instead of leaving the object empty (API1). This is a **critical** correctness fix that's been silently degrading PO-token quality.
- **C2 + C3 + P3** in DOM shim registration: propagate every `vm.Set` error (seven call sites previously dropped their returns silently), propagate `crypto/rand.Read` errors as JS GoErrors (previously ignored — all-zero "random" bytes would undermine BotGuard assumptions), and add a 65536-byte cap to `getRandomValues` matching the browser WebCrypto spec so a script can't OOM by asking for a giant slice.
- **DD1 + DD2** delete the unused `RunString` / `CallFunction` wrapper helpers — production code used `vm.RunString` / `fn(this, args...)` directly.
- **T1 + P2 + DD5** in `timer.go`: clamp `delayMs` via `clampDelayMs` to prevent `time.Duration` overflow when a script passes a huge value; add per-callback panic recovery inside `DrainCallbacks` so one bad callback can't abort the drain; remove the unused `timerEntry.interval` field; replace two `fmt.Fprintf(os.Stderr, ...)` timer-goroutine panic prints with an optional `SetLogger`-plumbed sink.
- **Q6** name the `__consoleMessages` buffer cap (`__consoleMaxMessages = 100`) so a future change can find and update it without grepping the embedded JS for "< 100".
- **T2** atomic check+enqueue in the interval tick goroutine under `tm.mu`, plus a test reorder (Drain-after-Clear) to match the actual contract. Fixes a race-detector flake exposed by DD5's struct-layout change.

**Cross-cutting lows** (cross-cutting.md — 7 commits):

- **C1** thread an optional logger into `AuthService` via `SetLogger` so the session-cleanup goroutine's panic-recovery no longer swallows errors silently.
- **C2** redact the `/api/webhooks/{id}/{token}` segment when logging rejected Discord webhooks — a near-valid URL carries a real secret, and rejection logs still land in log-collection stores that tail the file.
- **C3** partial — collect `worker.setJobError`'s six magic-string classifications (`login required`, `cookies?`, `age restricted`, `members only`, `max probe errors`, `member-only`) into `isCookiesRequiredError()` / `isNonActionableError()` helpers with the fragments in package-level slices. When the larger sentinel-errors migration lands, these become one-line `errors.Is` wrappers.
- **C5** `EmoteResolver.Resolve`'s inflight-dedup wait now respects `ctx.Done()` via a `select`; a cancelled download no longer blocks on a fetcher stuck on network IO.
- **C6** set a 30s `Timeout` on the TUI's loopback HTTP client (chi's server-side timeouts bounded most failure modes, but a stalled pipe mid-response could hang the TUI indefinitely).
- **C7** `orchestrator_mux.go` writes the `.description` via `writeDescriptionAtomic` (tmp+rename) — a crash mid-write previously left a truncated file with the DB row pointing at it.
- **C9** promote the four database scan-error logs (`getAllJobsUnlocked`/`getGaps`/`getTrimsUnlocked`/`getSegments`) from Debug to Warn — these indicate schema drift or a programmer bug, not expected noise.
- **C10** introduce `isDuplicateColumnErr(err)` helper in `migrations.go`; migrate all six call sites. When modernc/sqlite eventually changes error text (or we move to code-based detection via `*sqlite.Error`), it's a one-place change.

### Manual batch 6 (test.12)

Both `internal/bgutils/` and `internal/cipher/` were scheduled as parallel agent dispatches, but both agents detected the worktree was branched from main instead of `test` (a bug in the `Agent` tool's worktree-creation path) and aborted cleanly per explicit instructions. Implementation done directly on `test`. Net: **28 more commits** across the two subsystems.

**bgutils** (bgutils.md — 17 findings across 17 commits):

- **QI-2** `randomAlphaNum` switched from per-byte `crypto/rand.Int` + `big.Int` allocations to a single `rand.Read` + modular reduction
- **CRIT-5** replaced-minter `Cleanup()` now runs *outside* `pp.mu` in `generateAndMint`, mirroring the AfterFunc eviction path; new `safeCleanup(m, reason)` helper shields panics
- **CRIT-6** `InvalidateCaches` and `InvalidateIntegrityTokens` drain the minter map under lock, then iterate + `safeCleanup` after release — the `/invalidate_it` route can't freeze every concurrent `GeneratePoToken` during the sweep
- **PR-1 + PR-3** mint-path panic recovery: the inflight entry's `done` channel always closes and map is always cleaned up, even on Goja VM panic
- **CRIT-4 + TS-2** per-minter `sync.Mutex` on `WebPoMinter`; `Cleanup` now routes through `minter.Shutdown(bgClient.Shutdown)` so VM teardown serializes with any in-flight `Mint`
- **CRIT-8** plumbed a `botguardLogger` (nil-safe) through `NewBotGuardClient`; timer-callback errors go through `logger.Warn` instead of `fmt.Fprintf(os.Stderr)`
- **CRIT-10 + PR-2** `Snapshot(ctx, timeout)` — on cancel, `vm.Interrupt` is fired so an in-flight snapshot unblocks instead of burning the full 30s; default timeout fixed (was mistakenly `DefaultMintTimeout = 3s`, now `SnapshotDefaultTimeout = 30s`); `Shutdown` panic now logged
- **DEAD-4** deleted `SessionData.GetPoToken` (no callers)
- **DEAD-2** deleted `BotGuardClient.syncSnapshot` field + capture block (captured but never invoked)
- **DEAD-3 + DEAD-6** removed `IntegrityTokenData.MintRefreshThreshold` and `DescrambledChallenge.ClientExperimentsBlob` — both parsed but never read
- **DEAD-1** deleted `cold_start.go` + its six tests — zero production callers
- **QI-1** HTTP requests (challenge fetch, interpreter fetch, GenerateIT POST) now use `UserAgentFull`, matching the VM's view; `UserAgentShort` removed
- **QI-6** challenge-array parsing now uses a named const block (`idxMessageID`, `idxProgram`, …) instead of bare numeric indices
- **CRIT-9** timeouts renamed: `BotGuardCallbackTimeout` and `InterpreterExecutionTimeout` replace the descriptively-inverted `BotGuardLoadTimeout` / `SnapshotTimeout`; old names aliased for compat
- **FRESH-1** `BgConfig.SessionTTL` configuration hook (falls back to 6h default when 0), mirroring upstream `TOKEN_TTL`
- **FRESH-4** interpreter JS now cached by URL with an LRU cap of 4; saves the ~1-3 MB download per minter when YouTube hasn't rotated

**cipher** (cipher.md — 17 commits):

- **H1** solver LRU cap raised from 3 to 10 — monitoring 4+ channels no longer causes constant re-compiles
- **H2** LRU now moves on access, not just on insertion
- **H3** disk-cache eviction now runs on a 24h background ticker (with required inline panic recovery per CLAUDE.md), not only at startup
- **M1** documented the ModTime-based TTL's known edge cases (AV touching, manual file replacement)
- **M2** `ResolveURL` skips the `url.Parse` step for n-param replacement — splits at first `?` instead, dropping one brittle error path
- **M3** `StsCache` grew an inflight dedup map mirroring `bgutils.PotProvider` so concurrent `GetSts` calls for the same player share one fetch
- **M5** `Solvers.dead atomic.Bool` + `ErrSolverDead` — after a Goja panic, subsequent `DecryptN`/`DecryptSig` fail fast instead of re-entering the corrupt VM
- **M6** `compileSolver` checks `ctx.Err()` between extract/preprocess/compile steps so a worker cancel short-circuits promptly
- **Q5** `compileSolver` logs which extraction branch succeeded (`full` or `legacy`) at Info — prod triage gains back the diff
- **Q7** `findURLClassName` accepts multi-level dotted identifiers (`a.b.c`, not just `a.b`) — future minifier hardening
- **Q9** `legacySigHelperPattern` moved from `extractor.go` to `extractor_legacy.go` (its only consumer)
- **Q10** `CacheKey` truncated to 16 hex chars (64-bit hash space; negligible collision for this domain) — shorter cache paths, Windows MAX_PATH headroom. Existing 64-char files become unreachable and are harmlessly swept by the new H3 background tick
- **Q11** deleted `OverridePlayerURL` pass-through (no config knob ever wired through it)
- **Q12** eliminated error-chain stutter (`fetch player JS for abc: fetch player JS: ...`)
- **Q13** `Solvers.HasN()` / `HasSig()` accessors so callers can distinguish "no cipher required" from "cipher silently skipped"
- **DU3** deleted `StsRequest` / `StsResponse` types (unused)
- **DU4 + DU2** deleted `Solver.InvalidateCache` and `StsCache.Invalidate` — both all-clear methods had zero callers; keyed invalidation (`InvalidateSolver` + `InvalidateKey`) is the production path

**Agent-tool worktree-base bug** — for the historical record, this was the second batch in a row where `Agent` + `isolation: "worktree"` created the worktree off `main` (v2.5.2 / `f3ac3fb`) instead of the current `test` HEAD. Batch 5 worked around this by rebasing after the fact (10+ conflicts). This batch dodged it by having the agents verify their base on startup and abort if wrong — both did so cleanly, zero wasted work. Manual implementation followed.

### Parallel-agent batch 5 (test.11)

Fifth dispatch targeting residual low-severity findings in `internal/chat/` and `internal/database/`. Both agents branched from v2.5.2 (not `test`), so 37 commits were rebased/cherry-picked onto test with several conflicts resolved. Net: **37 more commits**.

**Chat** (chat.md — 22 findings across 20 commits):

- **C1** encapsulate `writeChatFile` state mutation so `cd.messages` and `cd.flushedToDisk` always reset on success
- **C3** always `saveResume` on cancel when `flushedToDisk`, regardless of in-memory buffer
- **C4** log at debug when `TimestampUsec` ParseInt fails instead of silently yielding `usec=0` (combined with HasOffset guard from test.8)
- **C6** run `cullDedup` on every successful fetch so pinned-announcement loops can't grow the dedup map unbounded
- **C9** type-assert `isCustomEmoji` as `bool` instead of loose equality against an `any`
- **C10** swap `math/rand.Intn` for `math/rand/v2.IntN` in `generateMessageID` — the v1 global is not concurrent-safe
- **C11** accept both `string` and `float64` for `videoOffsetTimeMsec` (YouTube occasionally ships either)
- **C12** log `updateChatFileHeader` IO errors via `OnError` instead of swallowing silently
- **C13** `fsync` before rename in `writeFullChatFile` so a crash mid-write can't leave a zero-byte file overwriting a good archive
- **C15** fix stale-continuation retry sleep ordering — sleep between retries, not before the first
- **R1** only mark `switchedToAllChat = true` when the response actually contained an All Chat continuation, so a subsequent poll can retry the upgrade
- **E1 + E2** handle unknown badge `iconType` values (fallthrough stores the raw lowercase string) and detect MEMBER via `icon.iconType` as well as tooltip heuristics
- **E3** pick the largest emoji thumbnail variant instead of `thumbnails[0]`
- **G1 + G2** wrap panic-recovery `OnError` call in its own deferred recover and include `debug.Stack()` in the panic message
- **Q5** bump watch-page fetch limit to 10 MB (chat-response limit stays at 5 MB)
- **Q6** rename ms-suffixed constants to typed `time.Duration` (`writeInterval`, `maxStaleContinuationAttempts`)
- **Q8** `uint32` keys for `superchatTierColors` (matches YouTube's raw palette) and debug-log unknown bgColors so drift is visible
- **Q12** snapshot resume state under lock, then JSON-marshal and write outside lock — keeps `MessageCount()` / `IsRunning()` unblocked during persistence
- **Q13** log `clearResume`'s `os.Remove` error at warn level so AV/permissions issues don't silently reload stale state
- **U4** remove unused `lastTimestamp` field from resume state

The chat subsystem gained a nil-safe optional `Logger` field on `ChatDownloader` and `ChatAPI` for the parse-level debug diagnostics (C4, C11). Existing callers that don't wire one in continue unchanged.

**Database** (database.md — 16 findings across 17 commits, including one net-new):

- **Q1** fix spec drift in `docs/spec/data-and-storage.md` (`closed bool` → `closeOnce sync.Once`)
- **Q4** `intToBool` helper matches the existing `boolToInt` on the write side
- **U4** unify `scanJob` and `scanJobRows` via a minimal `rowScanner` interface — drops ~45 lines of copy-paste
- **Q5** factor `UpdateResumePosition` and `UpdateChatOffset` into `updateSingleColumnSilent` guarded by a `silentColumns` whitelist
- **U3** reuse the prepared `stmtGetJob` in `UpdateJobFields` read-back instead of re-parsing the SELECT every call
- **Q9** copy the package-level `fieldToColumn` map into a per-instance copy at `Open()` so runtime mutation of the shared map is no longer possible
- **T5** track prepared statements in a `preparedStmts` slice via a `prepareStmt` helper so new additions can't leak on shutdown
- **D1** extract `insertJobExec` helper shared by `AddJob` and `ImportFromJSON` — keeps the 41-column INSERT in one place
- **D4** extract `capLogLines` helper for the `>200 → keep 100` log-buffer retention
- **I1** cache `GetJobStats` results for 5s — the 14-case aggregation is a full-table scan and polled by dashboard tiles
- **I2** `attachTrimsAndGaps` now filters trims/gaps/segments by `WHERE job_id IN (?, ?, ?)` with 500-ID chunking, rather than full-table scanning three times for every `GetAllJobs` call
- **I7** `BatchSetWatched` chunks into batches of 500 inside a single transaction to respect `SQLITE_MAX_VARIABLE_NUMBER`
- **Sub2** top-level panic recovery on the `notifyJobUpdate` / `notifyJobsChange` iteration loops so a subscriber panic can't escape into the caller
- **Sub4** shrink subscriber slices when `cap(s) > 4*len(s)` so subscribe/unsubscribe thrashing can't leak array backing
- **Sub5** fix misleading test comment about trailing-nil trimming (the unsubscribe path uses `append(a[:i], a[i+1:]…)`, which slices out rather than nils)
- **S2** expand the ALTER-TABLE-from-literal-slice comment in `migrations.go` to warn future editors that the pattern is safe only because the input is a compile-time literal
- **M6** v9 `player_prefs → jobs.chat_offset` backfill now counts expected rows up front and logs at ERROR (not WARN) on a row-count mismatch
- **I4** (new — agent didn't reach it before hitting its limit) schema v12 migration adds `idx_history_added_at` on `history(added_at)` — `pruneHistory` ORDERs by it on every `AddToHistory`

**Deferred from this batch** — these agent commits were recognised as obsolete or incompatible with earlier test-branch work and intentionally skipped:

- **Q3** remove half-wired `db.ctx` field — tied up with deleted `database_batch.go` (`UpdateJob` batched path was removed in decision #17); partial application would have created inconsistency. Can be done cleanly as a standalone sweep later.
- **P4** route UpdateJob to sync path when batch loop dies — the batch loop itself was deleted in decision #17, so obsolete.
- **Q10** bound `Close()`'s wait on `batchDone` with a 5s timeout — `batchDone` no longer exists (same reason as P4).
- **M7** `INSERT OR IGNORE` for schema_version seed — the `schema_version` table was dropped by the PRAGMA `user_version` cutover (decision #22); there's no seed to guard anymore.
- **M3** wrap each migration step in a BeginTx — the agent's 200-line refactor was designed for the pre-PRAGMA `UPDATE schema_version` pattern; `PRAGMA user_version` cannot be set inside a transaction, so M3 needs a fresh design. Saved as `reports/drafts/m3-migrations-tx-refactor.diff` for reference.

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

### Parallel-agent batch 4 (test.10)

Fourth dispatch over `internal/twitch/` and `internal/monitor/` — the last two subsystems with a long tail of unaddressed findings. Both agents completed fully, landing **19 more commits**.

**Twitch** (twitch.md — 13 findings across 11 commits):
- `ChatDownloader.Start` now rejects re-entry instead of racing on `seenIDs` re-init (#4)
- VOD thumbnail dimension rewrite skipped when no template matched (#6 — was corrupting legacy URLs with embedded `_640x360.`)
- Twitch channel-login lowercased in GQL paths; mixed-case UI entries no longer silently return empty stream info (#7)
- New `ErrTwitchAuthExpired` sentinel wraps 401/403 on GQL when a token was supplied; callers can `errors.Is` to distinguish token rotation from transient HTTP failure (#8)
- IRC reconnect counter resets after stable uptime so a flaky session doesn't permanently back off (#12)
- IRC session read deadline (6 min, covers two missed pings), clean exit on RECONNECT, proper tag-order handling (#14/#15/#16)
- Resolve mutex released before closing inflight channel to avoid blocking a stalled caller (#17)
- VOD chat pagination no longer terminates on an all-duplicate page while `hasNextPage==true` (#31 — fixes resume on stale-continuation)
- GQL error messages now include operation name for correlation (#34)
- HLS playlist error messages include Usher response body (#35)
- `appendChatMessages` scans the last 256 bytes for the closing `]` instead of the last 10 (#30 — earlier padding widths forced the fallback-merge path unnecessarily)

**Monitor** (monitor.md — 8 commits):
- `waitForRateLimit` now bails when `resetAt` is zero instead of blocking indefinitely (#6)
- `updateRateLimit` rejects past/backward `X-RateLimit-Reset` timestamps (#7)
- `filterUniqueDescriptionLines` dropped from O(N×M) to O(N) via seen-map reuse (#8)
- Swallowed `HasActiveJob` / `HasProcessed` DB errors now logged at Debug instead of silently proceeding with stale assumptions (monitor dead-code items)
- Feed monitor: per-channel stagger between requests to avoid synchronized polls
- Decapi monitor: jitter added to polling interval so multiple instances don't synchronize
- `monitor.SetConnectivityReporter(connMon)` wired up — monitor HTTP failures now feed the passive-connectivity tracker alongside `engine/fetch` and `utils/http` (Critical #2)
- Dead `SetLastVideo` writes removed (obsolete since cf208bc)

Plus a one-line wire-up in `cmd/moombox/main.go` to call `monitor.SetConnectivityReporter(connMon)` alongside the existing engine / utils wires.

### Parallel-agent batch 3 (test.9)

Third dispatch over the remaining untouched subsystems — `web/public/*.js`, `web/public/*.html` + `moombox.css`, `cmd/moombox/`, and the small packages (utils, logger, notifications, connectivity, updater, disk, cmd/sign). All four agents completed fully, landing **52 more commits**.

**Frontend JS** (frontend-js.md — 12 fixes): abort-controller coverage for the resume-dialog / trimmer / player / filter keydown listeners (C5, C11, C14, C15, C18), `loadJobLogs` cancelling before body parse (C9), `batchAction` gating on `_jobActionsInFlight` (C6), stable version-indicator handler (C1), debounced format-fetch cancellation (C2), fetch-401 URL pathname extraction (C4), restart-redirect timer armed before firing (C12), OS-theme change tracking (C20).

**Frontend HTML + CSS** (frontend-html-css.md — 15 fixes): added `.job-progress-bar` colors for `cancelled`/`finished` statuses (Critical), wrapped main tab group in a `<main>` landmark with a visually-hidden `<h1>` (Critical), `100dvh` alongside `100vh` to follow iOS Safari viewport (Critical), inline theme-bootstrap scripts on both index.html and login.html to kill light-theme FOUC (Critical × 2), `<main>` landmark + `autocomplete="current-password"` on login, `aria-label`s on chat offset control, `aria-hidden` on decorative icons, extracted `--status-bar-h` / `--batch-bar-h` CSS custom properties, `:focus-visible` outline restored on `#player-chat-offset`, keyboard-shortcut dialog uses `<dl><dt><dd>`, `role="separator" aria-orientation="vertical"` on status dividers, `.code-snippet` class replaces duplicated inline styles, login.html meta tags aligned with the main page.

**cmd/moombox** (cmd-moombox.md — 10 fixes): winres manifest `long-path-aware: true` so Windows extended paths resolve, `moombox add` bypasses the launcher (direct DB write), `cfgMu` declared before first cfg mutation (closes the race between startup-time `Save` and later readers), `OnSchedule` monitor callback + `OnAuthChange` cookieRefresh callback guarded via `atomic.Pointer` (closes data races with TUI-side callback installation), shutdown cleanup made force-exit-safe via `sync.Once`, `db.Close` made idempotent, web-server panic recovery uses non-blocking send, `cmd/sign -genkey` takes a `-out` flag (stops stdout private-key leak), `addVideo` notifications flush via `Manager.Wait` instead of a 500 ms sleep.

**Small packages** (small-packages.md — 15 fixes): deleted dead `utils/time_format.go` helpers (FormatBytes/FormatDuration/FormatETA), removed unused `DiscordWebhook.Logger`, Discord 429 retry now surfaces the real error on retry failure, documented Discord HTTP client timeout intent and Logger Subscribe/Unsubscribe lifecycle in godocs, broadened twitch reserved-path map (store/tag/drops/turbo/wallet), connectivity Monitor.Start is idempotent and seeds the offline state via a synchronous startup probe, passive-tracker failure slice capped at 256 entries, updater leaves a `.update-broken` marker on rollback failure, hoisted updater HTTP client to package scope, `utils.SetConnectivityReporter` uses `atomic.Pointer`, clarified YouTube/Twitch video-ID regex collision in godoc, added COM0/LPT0/CONIN$/CONOUT$ to Windows reserved-name guard, Logger clamps rotation thresholds to safe minimums, Logger uses `a.Value.Time()` in `ReplaceAttr` instead of `time.Now()`.

### Parallel-agent batch 2 (test.8)

Second dispatch, this one over `internal/tui/`, `internal/web/`, and `internal/worker/`. All three agents ran to the token limit; the **23 commits they had already completed** were fully merged, and the incomplete items were left for a later round. Every batch race-detector-clean.

**TUI** (tui.md — 6 fixes):
- Log buffer backing-array aliasing prevented (a reslice on the ring buffer could let writers stomp reader-held slices)
- Pre-allocated yellow warning styles; 17+ inline `#f1c40f` literals consolidated
- `SpinnerInit` tick-closure deduplicated across dialog models
- Setup-wizard channel editor uses `keyLeft`/`keyRight` constants instead of string literals
- `ffmpeg installOption` list building inlined into `buildInstallForm` (clearer control flow)
- Non-Windows `openBrowser` branches dropped (Moombox is Windows-only per SPEC)

**Web** (web.md — 10 fixes):
- `HSTS` header emitted on TLS connections (max-age=31536000; `includeSubDomains` intentionally omitted)
- WebSocket bound with an idle-read timeout (2× ping interval); obsolete message-size check removed
- `GET /api/config` RLock released before JSON encoding
- `/api/logs` response capped at 500 lines (prevents unbounded response on noisy sessions)
- `open-folder` route releases the explorer.exe handle after Start
- Cookie route failures return real HTTP error codes instead of generic 200/500
- Twitch manual job ID uses nanosecond timestamp (collision risk on rapid adds eliminated)
- Background goroutines in route handlers route panics through the structured logger
- Rate-limiter cleanup goroutine panics are now logged
- Throttle callbacks survive hub Close without a nil-map panic

**Worker** (worker.md — 7 fixes):
- Early chat path uses the correct `pending → downloading` transition
- Cookie-refresh re-probe re-queues as `Upcoming` (not whatever leftover state)
- Muxing→Downloading reset clears stale error field
- `pollForJobs` restart-pause uses ctx-aware sleep (was blocking on `time.Sleep`)
- Refresh-error comments in YouTube live loop clarified
- Still-live manifest refresh replaces the entire result rather than cherry-picking fields
- Live-stream loops now compute the download error BEFORE sending it (was a no-op send race)

### Parallel-agent batch 1 (test.7)

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
