# Sweep fix chain — design (2026-09-15)

Umbrella design for fixing every item of the 2026-09-15 improvement sweep. Source: the sweep ledger
`reports/sweep-2026-09-15.md` (gitignored; 35 numbered items, T1–T4 tiers, every headline item
re-verified in source at `5bbf16e8`). The owner ruled "fix them all" on 2026-09-15.

The chain is seven arcs. Each arc is designed here in full; each gets its own plan in
`docs/superpowers/plans/2026-09-15-sweep-<arc-slug>.md` written from THIS document and the ledger.
Item numbers below (T1-1 … T4-35) refer to the ledger.

## 1. Owner rulings (2026-09-15)

| # | Question | Ruling |
|---|---|---|
| R1 | Attestation challenge plumbing (T4-31) | **Delete the dead path**: remove the unused `challenge` parameters from `fetchWithClient`/`fetchWithEmbedded` and their call sites, and the uncalled `PotTokenProvider.GeneratePlayerPoToken` interface method + implementation. The sidecar JS is untouched. |
| R2 | `-movflags faststart` (T4-35) | **Keep**. No change to `internal/engine/muxer.go`. |
| R3 | TUI `A D` on active jobs (T4-33) | **Hide Delete for active jobs in the TUI**: the Delete menu item gets the Web's status filter (Finished, Error, Cancelled, COOKIES?). |
| — | Everything else | Fix as the ledger proposes, with the design defaults in §3–§9. |

## 2. Execution model

- Arcs run on branches cut from `main` into `.worktrees/<arc-slug>`, at most **two arcs concurrently**
  in separate worktrees, and only when their file sets are disjoint (the table in §10). One
  implementer per working tree. Reviewers and planners may run in parallel freely.
- WORKTREE RECIPE (every arc): `git worktree add -b <branch> .worktrees/<branch> main`, then copy the
  gitignored inputs a fresh worktree lacks: `internal/bgutils/embed/{node-windows-amd64.gz,node-linux-amd64.gz,node-linux-arm64.gz,sidecar.tar.gz}`,
  `internal/cipher/testdata/*.js`, and `cd web/tests && npm ci --no-audit --no-fund`.
  `GOTMPDIR=D:/Git/Moombox/.superpowers/gotmp` on every go command.
- SDD loop per arc: implementer → task review → fix rounds → Fable arc-close review → merge `--no-ff`
  into main WITHOUT asking (standing ruling 2026-09-04) → delete worktree AND branch → post-merge gates
  on main. Nothing pushes; no version bump; the owner controls release.
- Gates per arc (merge candidate): `gofmt -l ./cmd ./internal ./tools ./web` empty, `go vet ./...`,
  `staticcheck ./...` (pinned 2026.2.1) clean, `go build ./...`, `GOOS=linux GOARCH=amd64` and
  `GOOS=linux GOARCH=arm64 go build ./...`, ONE `go test -count=1 ./...` (controller-run, one at a time
  across all worktrees), `node --test web/tests/*.test.mjs` when JS changed. Live gates
  (`MOOMBOX_LIVE_CIPHER_TEST=1`, `MOOMBOX_LIVE_BG_TEST=1`, and the YouTube live extraction gates) run
  where an arc touches extraction clients, goja, the sidecar payload, or the cipher.
- Ledger: `.superpowers/sdd/2026-09-15-sweep-fix-chain/progress.md` for the chain; per-arc SDD ledgers
  beside their plan workspaces. Every ruling is written as `Ruling: <what> — <why> — <cost if wrong>`.
- Plans are deleted in the arc's last commit once implemented (delete-implemented-plans rule). Living
  docs (`SPEC.md`, `docs/spec/*.md`, `CLAUDE.md`, `.claude/skills/*`) are updated instead.

## 3. Global constraints (every task inherits these)

- `go 1.27`, no CGo, pure-Go dependencies; Windows x64 + Linux x64 + Linux arm64 must build.
- LF line endings in every file the chain touches. Commits carry the two trailers
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>` and
  `Claude-Session: https://claude.ai/code/session_01GhTENJov1fPmZFgk43nPRq`.
- The anonymous logger interface (`Debug/Info/Warn/Error(msg string, args ...any)`) stays anonymous
  per struct. Every goroutine has an inline `defer func() { if r := recover(); … }()`.
- `internal/tui` never imports `internal/web/routes` or `internal/bgutils` (compile-time embed check).
- Helpers are defined in the task that first uses them (staticcheck U1000 is a hard gate).
- TDD per task: the failing test is written and run red before the change; every new assertion names
  the mutant that fails it (the reviewer verifies at least one).
- Every JS-touching task gates `go test ./internal/web/routes/` (its tests lift app.js/player.js
  bodies into goja) AND `node --test web/tests/*.test.mjs`. Every task that renames, moves or deletes a
  Go symbol, or edits `docs/spec/*.md`/`SPEC.md`, gates `go test ./internal/docs/` (the citation test
  requires the DECLARING file).
- Behaviour that the owner's rulings protect stays: ~60 Hz progress pipeline and DB write cadence
  (make updates cheaper, never rarer); DB layer untouched for perf; `monitors.probe_cooldown` default 0;
  the BotGuard interpreter gate; `/retry` vs `/resume` gates never shared; the Web cookie import stays
  unbounded (a test forbids WithTimeout).
- Reviewers never edit files (they reproduce in `git archive` scratchpad exports); implementers use git
  only for `add`/`commit` on the arc branch; no stashing, no rebasing, no checkout of other branches.
- User-facing strings: match the twin UI's exact output before mirroring it (read the twin's code).

## 4. Arc 1 — Twitch chat correctness (branch `sweep-1-twitch-chat`)

**Packages:** `internal/twitch`, `web/public/modules/player.js` (+ its jsdom pin), `web/public/modules/chat-timeline.js` only if the header merge needs the new scalar, `docs/spec/platform-services.md` and `data-and-storage.md` (chat file format).

**Items:** T1-1, T1-3, T1-11, T2-15, T3-25, T4-34 (chat_irc.go:269 comment).

**Design.**
1. **Emote offsets (T1-1).** Twitch IRC `emotes=<id>:<start>-<end>` offsets index Unicode code points
   (verified on 120 real wire ranges; chatterino7, go-twitch-irc, twitch-irc-rs agree). The emitted
   `TwitchEmoteRef.Start/End` stay UTF-16 code-unit indices because `player.js` slices with
   `String.prototype.substring` and the VOD path (`api.go:975-990`) already emits UTF-16.
   `parseEmoteTags(emotesStr, message string) []TwitchEmoteRef` keeps its signature and:
   - builds `runes := []rune(message)` and `cpToUnit := make([]int, len(runes)+1)` where
     `cpToUnit[i]` is the UTF-16 index of code point `i` and `cpToUnit[len(runes)]` is the UTF-16 length;
   - for each range with `0 <= start <= end < len(runes)`: `Name = string(runes[start:end+1])`,
     `Start = cpToUnit[start]`, `End = cpToUnit[end+1]-1`;
   - out-of-range ranges keep today's behaviour: raw `Start/End` as sent, empty `Name`.
   `TestParseEmoteTagsNonBMP` flips to the true wire value: `parseEmoteTags("25:2-6", "🎉 Kappa")`
   → `{ID:"25", Name:"Kappa", Start:3, End:7}`. ASCII cases and `TestParseEmoteTagsOutOfBounds` are
   unchanged. The wrong-premise comment block above the function and in the test is rewritten.
2. **`/me` messages (T1-1).** `parsePrivmsg` strips a leading `"\x01ACTION "` and trailing `"\x01"`
   from the message text BEFORE emote parsing and sets a new field
   `IsAction bool `json:"isAction,omitempty"`` on `TwitchChatMessage`. Test:
   `parseEmoteTags`-level and `parsePrivmsg`-level cases for `"\x01ACTION Kappa\x01"` with
   `emotes=25:0-4` → `Message:"Kappa"`, `IsAction:true`, emote `Kappa/0/4`. The `raw` field keeps the
   original line.
3. **File marker + replay correction (T1-1).** `TwitchChatData` gains
   `EmoteOffsets string `json:"emoteOffsets,omitempty"`` written as `"utf16"` by every writer of a full
   chat file (IRC and VOD paths). The append path (`utils.AppendChatMessages`, "last `]`" locator), the
   header scalar reader (`chatFileRecordingBaseMs`) and `decodeChatPartFile` must tolerate the new
   scalar; the planner verifies each with a test that reads a file carrying the marker. In `player.js`
   `_appendTwitchMessage` (≈:1782-1815): when the loaded chat file's header lacks
   `emoteOffsets === "utf16"` AND the message carries `raw`, convert each emote's `start`/`end` from
   code points to UTF-16 (same mapping as Go) before slicing; also strip a leading `\x01ACTION ` /
   trailing `\x01` from legacy message text. Add a jsdom pin for `_appendTwitchMessage` covering
   (a) a marked file with UTF-16 offsets after an emoji, (b) an unmarked legacy message with `raw`
   and code-point offsets, (c) an unmarked message without `raw` (VOD, already UTF-16 — untouched).
4. **VOD paging by cursor (T1-3).** `VideoCommentsByOffsetOrCursor` edges each carry `cursor`;
   offsets are integer seconds; a page holds 59 edges. The VOD comment fetcher returns each page's last
   `cursor`; `vod_chat.go` requests the FIRST page (start or resume) by `contentOffsetSeconds` and every
   later page by `cursor`. Persisted-query hash unchanged. Test: a fake GQL server serving three pages
   whose 59+59+10 edges all share one offset second; the downloader must archive all 128 comments and
   stop only on `hasNextPage=false`. Resume (`LastOffsetSeconds`) still starts by offset.
5. **Emote cache (T1-11).** Third-party fetchers return `(emotes []EmoteInfo, ok bool)`; `Resolve`
   caches only when at least one fetcher returned `ok`; cache entries carry a fetch time and expire after
   `emoteCacheTTL = 24 * time.Hour` (expired → refetch on the next `Resolve`, serving the stale set if
   the refetch fails). Tests: all-failed → not cached and refetched next call; one-ok → cached; TTL
   expiry refetches (fake clock or injectable `now`).
6. **Resume sidecar cap (T2-15).** The IRC chat resume sidecar snapshots at most
   `chatResumeIDCap = 1000` newest dedup IDs (share the constant with `vod_chat.go:21` if the values are
   equal). Test: 5000 seen IDs → sidecar carries the newest 1000 in order.
7. **IRC keepalive (T3-25).** The IRC client tracks the last inbound frame time. A ticker
   (`ircKeepaliveCheck = 15 s`) sends `PING :moombox` after `ircKeepaliveIdle = 45 s` without inbound
   frames and expects any inbound frame within `ircKeepalivePongWait = 10 s`; otherwise the read loop
   returns an error that the existing reconnect path handles. Server `PING` handling and the 6-minute
   `ircReadDeadline` outer bound stay. Durations live in an injectable struct (the `delays` pattern of
   Arc E) so the test runs in milliseconds: a fake WebSocket IRC server that goes silent after the
   handshake must observe one `PING :moombox` and see the client reconnect; a server that answers `PONG`
   keeps the connection. The `chat_irc.go:269` comment is rewritten to state the real bounds.
8. **Docs.** `platform-services.md` (Twitch chat: offsets rule, ACTION, keepalive, cursor paging) and
   `data-and-storage.md` (chat file `emoteOffsets` scalar, `isAction`).

**Gates:** `./internal/twitch/...`, `./internal/web/routes/`, node suite, `./internal/docs/`.

## 5. Arc 2 — Engine (branch `sweep-2-engine`)

**Packages:** `internal/engine`, `internal/utils/http.go` (one guard), `docs/spec/architecture.md` if it describes the HLS end verdict.

**Items:** T1-2, T2-14, T2-17, T3-29, T4-31 (HealthUpdate), T4-34 (downloader.go:597 comment), T4-35 (4xx drain; caller-cancel guard).

**Design.**
1. **HLS end verdict (T1-2).** A `CheckStreamStatus` ERROR never finalizes. At the playlist 404/410 site
   (`downloader_hls.go:273-282`) and the >5-consecutive site (`:296-304`) and the third site (`:344-352`):
   `checkErr != nil` logs `"stream status check failed; deferring end verdict"` and does NOT set
   `streamEnded`/return nil. The 404/410 site then falls into the consecutive-error accounting (retry with
   the existing delay); when the consecutive budget is exhausted with the verdict still unknown the loop
   returns the existing `HLS playlist fetch failed after N consecutive errors` error (job → Error, which
   `isRecoverableTwitchError` can recover) — never Finished. `ended==true` and `!ended` keep today's
   behaviour. Tests (fake fetcher + fake status check, `fastDelays()`): (a) 404 + check error → loop does
   not return nil and `streamEnded` stays false; (b) 404 + check error then check `!ended` → `ErrQualityLost`;
   (c) 404 + check `ended` → nil as today. Mirror the DASH comment wording (`downloader_dash.go:447-462`).
2. **Resume save cadence (T2-14).** HLS `saveResume` runs at most once per `hlsResumeSaveInterval = 15 s`
   (a `delays` field so tests scale it) unless the loop is exiting (the deferred final save stays
   unconditional). Test: 20 playlist iterations inside one interval → one write; the deferred save writes
   the final sequence.
3. **Body pre-sizing (T2-17).** `readBody(resp *http.Response, capBytes int64) ([]byte, error)` pre-sizes
   from `resp.ContentLength` when `0 < ContentLength <= capBytes` and falls back to `io.ReadAll`
   otherwise; used at `downloader_fetch.go:180` (segments) and `:582` (VOD chunks, where the exact size is
   `end-start+1`). Test: Content-Length present/absent/oversized → identical bytes; one allocation pin
   via `testing.AllocsPerRun` on the sized path.
4. **Head probe (T3-29).** The fallback probe fires only when the first probe returned a response
   WITHOUT the header, not on transport errors. Test: first probe transport error → exactly one request.
5. **4xx drain (T4-35).** After the 512 B error snippet, drain up to `maxDrainBytes` (the constant the
   206 path already uses at `:584-590`) before `Close`, so the connection returns to the idle pool. Test:
   an `httptest.Server` sending a 2 KB 403 body twice → `ConnState` sees one connection.
6. **Caller cancellation (T4-35).** `internal/utils/http.go:235-238` and `downloader_fetch.go:153-156`
   skip the connectivity failure record when `ctx.Err() != nil`. Test per site.
7. **Dead code (T4-31).** Delete `HealthUpdate`, `OnHealthUpdate`, `emitHealthUpdate`,
   `transientRetries`, `lastTransientErr`, `startedAt` and the one call site (`downloader_dash.go:272`).
   Gate: build/vet/staticcheck + `./internal/docs/` (if any doc cites them, fix the doc).
8. **Doc drift (T4-34).** `downloader.go:597-607` describes the real linear `5 s × n` retry.

**Gates:** `./internal/engine/...`, `./internal/utils/...`, `./internal/worker/...` (orchestrator consumes the HLS verdicts), `./internal/docs/`.

## 6. Arc 3 — YouTube extraction (branch `sweep-3-youtube`)

**Packages:** `internal/youtube`, `internal/cookies/jar.go` (mtime short-circuit only), `bgutil-sidecar/vendor/ejs/` + `VERSION`, `.claude/skills/moombox-upstream-porting/SKILL.md`, `docs/spec/platform-services.md`.

**Items:** T1-5, T2-16, T2-23, T3-27, T4-31 (challenge, R1), T4-34 (skill doc LRU), ejs pin bump.

**Design.**
1. **Player-response parse (T1-5).** Replace the three lazy `({.+?});` patterns with anchor regexes
   that match only the prefix (`var ytInitialPlayerResponse\s*=\s*`, `window\["ytInitialPlayerResponse"\]\s*=\s*`,
   `ytInitialPlayerResponse\s*=\s*`) followed by `scanBalancedObject` (`watch_page.go:928`) from the
   `{`. Test: a synthetic watch page whose `shortDescription` contains `"};"` (and one with `}` inside a
   string with an escaped quote) parses to the full object; the existing fixtures still parse
   byte-identically (compare the extracted JSON before/after via the fixture set).
2. **Watch-page allocations (T2-16).** Run the extraction regexes/scanners on `[]byte` (no
   `string(body)` copy) and decode the chat continuation from a typed envelope with `json.RawMessage`
   down to `contents.twoColumnWatchNextResults.conversationBar.liveChatRenderer.{isReplay,continuations}`
   (the shape `channel_membership.go:134-151` uses) instead of `map[string]any`. Behaviour pinned by the
   existing continuation tests; add `BenchmarkExtractChatContinuation` and an `AllocsPerRun` ceiling.
3. **Cookie reload (T2-23).** `CookieJar.Load` short-circuits when the file's `(size, mtime)` pair is
   unchanged since the last successful parse (recorded under the jar lock); callers are untouched. Test:
   two `Load`s without a write → one parse (seam counter); a write → re-parse.
4. **Retry budget (T3-27).** `doRetryRequest`'s 1+2+4 s backoff becomes context-deadline-aware: before
   each sleep, if `ctx` has a deadline that the sleep would cross, return the last error immediately.
   Ruling: no concurrent client cascade in this chain (cost if wrong: recovery stays sequential but now
   bounded). Test: a ctx with a 1.5 s deadline and a 503-forever server → returns before 2 s.
5. **Dead challenge path (T4-31, R1).** Remove the `challenge` parameters and arguments from
   `fetchWithClient`/`fetchWithEmbedded` and every call site; remove `GeneratePlayerPoToken` from the
   `PotTokenProvider` interface and its implementation; leave the sidecar protocol and
   `generateAndMint` alone. Gate: build/vet/staticcheck + `./internal/docs/` + the YouTube live gate.
6. **ejs re-vendor.** Follow the skill's "Updating vendored ejs" recipe to upstream `6f8587b`: copy the
   listed files, write the SHA to `bgutil-sidecar/vendor/ejs/VERSION`, keep meriyah/astring pins in
   lockstep, `node build.mjs`, `go test ./internal/cipher/...`, and the live cipher gate. The rebuilt
   `internal/bgutils/embed/sidecar.tar.gz` is gitignored: the controller copies it to main after the
   merge and rebuilds `moombox.exe`.
7. **Doc drift (T4-34).** `.claude/skills/moombox-upstream-porting/SKILL.md` says "3-slot LRU" twice;
   the cap is `solverCacheSize = 10` (`internal/cipher/solver.go:19`). Also `docs/spec/platform-services.md`
   if it repeats the figure.

**Gates:** `./internal/youtube/...`, `./internal/cipher/...`, `./internal/bgutils/...`, `./internal/cookies/...`, `./internal/docs/`, live cipher + YouTube extraction gates.

## 7. Arc 4 — Monitor, notifications, connectivity (branch `sweep-4-monitor`)

**Packages:** `internal/monitor`, `internal/notifications`, `internal/connectivity`, `docs/spec/architecture.md` (monitor cadence prose).

**Items:** T1-10, T2-12, T2-13, T3-28, T3-30, T4-35 (Discord body, feed drain).

**Design.**
1. **DECAPI probe budget (T1-10).** `checkChannel` cancels the 15 s request context right after the
   body is read and passes the cycle context (bounded by a new `decapiProbeBudget = 60 s` derived from
   it) into `processResponse` → `ProcessYouTubeVideo`/`ProbeDate`. Test: a probe seam asserts the
   context it receives has a deadline ≥ 30 s from now even when DECAPI took 14 s to answer.
2. **DECAPI terminal memo (T2-12).** `DecapiMonitor` keeps per-channel `{lastVideoID, lastStatus}`;
   `processResponse` skips the probe when the ID is unchanged, `lastStatus ∈ {vod, not_a_stream}` and the
   job/history says processed; a new ID resets the memo. Cadence and `probe_cooldown` untouched. Tests:
   same-ID terminal → no probe on the second cycle; new ID → probe; same-ID `upcoming` → probe.
3. **Membership memo (T2-13).** The feed cycle keeps `nonMemberUntil[channelID] = now + 6 h` when
   `parseMembershipTab` reports no access; channels inside the horizon skip the authenticated fetch —
   except that every cycle fetches AT LEAST ONE channel (the skipped one with the earliest horizon when
   all are memoized) so `ObserveLiveness` still receives a verdict. A member channel is never memoized.
   Tests: non-member skipped for 6 h; member fetched every cycle; all-memoized cycle still fetches one.
4. **Twitch stagger (T3-28).** The inter-chunk stagger runs before `continue` on a whole-batch error.
   Test: two chunks, first batch errors → the second starts after the stagger.
5. **Connectivity boot (T3-30).** `Start` seeds state from one check and starts the loop; it does not
   run a second synchronous `poll()`. Test: offline at boot → `Start` returns within one probe timeout.
6. **Discord 4xx body (T4-35).** Include up to 256 bytes of the response body in the error for 4xx.
   Test: 400 with a JSON body → error text contains it.
7. **Feed drain (T4-35).** Bound the non-200 drain at 4 KB (mirror `decapi.go`). Test.

**Gates:** `./internal/monitor/...`, `./internal/notifications/...`, `./internal/connectivity/...`, `./internal/docs/`.

## 8. Arc 5 — Web server + frontend (branch `sweep-5-web`)

**Packages:** `internal/web` (+ `routes`), `web/public/modules/{log-panel,job-details,stats}.js`, `web/public/app.js` (resync handling), `web/tests`, `cmd/moombox/main.go` (ActualPort readers), `docs/spec/security.md`, `docs/spec/user-interfaces.md`.

**Items:** T1-6, T1-9, T2-18, T2-19, T2-21, T3-26, T4-35 (ActualPort, gzip pool).

**Design.**
1. **Origin policy (T1-6).** In `external`/`public` modes a mutating request's `Origin` (or `Referer`)
   is allowed only when its host:port equals the request's effective host — `r.Host`, or the forwarded
   host when the peer is a trusted proxy (reuse the trusted-proxy helper that `EffectiveClientIP` uses;
   honour `trust_forwarded_proto` for the scheme). CORS reflects an origin under the same rule. `lan` and
   `localhost` keep today's rules. `middleware_test.go:138-147` flips; new tests: external mode +
   `Origin: https://evil.example` → 403 on POST and no `Access-Control-Allow-Origin`; same-host origin →
   allowed; trusted proxy with `X-Forwarded-Host` → allowed. `security.md` documents the rule.
2. **WS drop accounting (T1-9).** `queueOrDrop` never logs per drop. Each client keeps an atomic drop
   counter; the hub logs `"WS client lagging"` at Debug at most once per 30 s per client and once at
   client removal with the total. Test: fill a client's queue and push 100 frames → at most one log call.
3. **needsResync (T3-26).** Any drop sets `client.needsResync`; the next enqueue for that client sends
   the full initial-state snapshot (the exact payload a new connection receives) instead of the frame and
   clears the flag. `app.js` already handles the initial-state message type on connect — verify and pin
   with the jsdom harness that a mid-session snapshot replaces the job list. Test (Go): drop → next frame
   is a snapshot.
4. **Asset validators (T2-18).** A wrapper before `fileServer.ServeHTTP` sets `ETag` to
   `"<commit>"` when the build commit is known, else the hex SHA-256 of the file (computed once per path
   and cached); `net/http` then answers `If-None-Match` with 304. `immutable, max-age=1y` applies only
   to URLs carrying `?v=`; other paths use `no-cache` + ETag. `CompressionMiddleware` commits plain
   (`commitPlain()`) when `Content-Type` starts with `image/` or `video/`, or `Content-Encoding` is set.
   Tests: conditional GET → 304; `/favicon.svg` no `immutable`; a JPEG thumbnail response is not gzipped.
5. **Chat endpoint (T2-19).** Serve `chat.json` through `http.ServeContent` with the `*os.File`
   (Range + conditional headers for free); replace whole-file `json.Valid` with a bounded check: the
   first non-space byte is `{` and the last non-space byte is `}` (the writer is atomic; corruption is
   truncation). Keep the 422 contract for a failing check. Tests: truncated file → 422; valid → 200 and a
   `Range` request → 206; conditional → 304.
6. **ActualPort (T4-35).** `atomic.Int32` with a getter; readers in `main.go:373,452` use it.
7. **gzip pool (T4-35).** `sync.Pool` of `*gzip.Writer` (reset per response).
8. **Frontend (T2-21).** `log-panel.js`: queue lines and flush once per animation frame (one fragment
   append, one scroll write). `job-details.js` (`:157-254`, `:283-321`) and `stats.js:174-176`: every
   per-tick write is guarded by `!==` against the current value (the pattern of `app.js:1382`), and
   `toLocaleString` runs only when the source timestamp changed. Tests in the jsdom harnesses: pushing
   the same update twice performs zero DOM writes the second time (spy on `textContent`/`className`
   setters); 100 log lines in one tick → one `appendChild` of a fragment.

**Gates:** `./internal/web/...`, `./cmd/moombox/...`, node suite, `./internal/docs/`.

## 9. Arc 6 — TUI + jobfilter (branch `sweep-6-tui`)

**Packages:** `internal/tui`, `internal/jobfilter`, `docs/spec/user-interfaces.md`, `CLAUDE.md` chord table (if the Delete filter changes its wording).

**Items:** T2-20 (five sub-items), T2-24, T4-33 (help rows; `A D` filter per R3).

**Design.**
1. **Progress-store seeding (T2-20a).** `JobsUpdateMsg` (`app_update.go:213-228`) and
   `handleJobAdded` (`:1195`) skip `Set` for jobs whose status satisfies the terminal predicate that
   `handleJobUpdate` uses at `:1163` (completed/Error/COOKIES?). Test: a full snapshot with one Finished
   job → no progress entry; a `progressTickMsg` with that job selected does not call `SetProgress`.
2. **SetProgress no-op (T2-20b).** `SetProgress` returns early when the pointer equals the last one
   AND `time.Now().Unix()` equals the second of the last build. Test: two calls with the same pointer in
   the same second → one `buildRows` (counter seam); a new pointer → rebuild.
3. **Status summary cache (T2-20c).** `buildStatusSummary` output is computed in `rebuildVirtualList()`
   and unconditionally in `ResweepArchive()`, stored on the model; `renderHeader` reads the string.
   `status_bar.go` counts jobs once per frame into a per-tier array. Test: `View()` called twice without
   a job change → `time.Parse` not invoked on the second call (seam counter or an `AllocsPerRun` pin).
4. **Log level cache (T2-20d).** `rebuildFiltered` stores a parallel `[]color.Color` (or level byte)
   per line; `styleLogLine` reads it. Test: level parsed once per line across two renders.
5. **jobfilter fold (T2-24).** Case-insensitive substring match without allocating: ASCII fast path,
   `unicode.SimpleFold` fallback for non-ASCII; identical results on the 31 pinned node cases (Go twin
   test table) plus a `testing.AllocsPerRun == 0` pin for an ASCII query. The JS twin is unchanged.
6. **Help + Delete filter (T4-33, R3).** `help.go` gains rows for Space (batch select), Esc (clear
   selection/query/chord), Home/End, Ctrl+U/Ctrl+D. The `A D` menu item gets `JobFilter` = Finished,
   Error, Cancelled, COOKIES? (the Web's `DELETE_STATUSES`). Tests: help text contains each row; the
   action menu for a Downloading job lists no Delete item; for a Finished job it does. Docs:
   `user-interfaces.md` chord table + batch-select mention.

**Gates:** `./internal/tui/...`, `./internal/jobfilter/...`, `./internal/docs/`.

## 10. Arc 7 — Core, launcher, config, cookies, plugin, chat (branch `sweep-7-core`)

**Packages:** `cmd/moombox`, `internal/config`, `internal/cookies`, `internal/ytdlpplugin`, `internal/chat`, `internal/web/routes` (plugin status field only), `internal/tui` (plugin overlay line only), `docs/spec/operations.md`, `data-and-storage.md`, `README.md` (config search paths).

**Items:** T1-4, T1-7, T1-8, T2-22, T4-31 (port 0), T4-32, T4-34 (DPAPI message), T4-35 (plugin status, AES-GCM, chat adoption).

**Design.**
1. **Rollback artifact (T1-4).** `handleUpdateRestart` logs the `os.Rename(.old → ~)` failure at Warn
   and still returns true; `rollbackArtifactPath(exePath)` returns `exePath+".old"` when that file exists
   and `exePath+"~"` otherwise. The plan MUST carry the cross-version analysis the update-path rule
   demands: first-update success path byte-identical (rename succeeds, `.old` gone, `~` restored as
   before); second-update path: `.old` survives, rollback restores it; an old launcher with a new child
   sees no new artefact names; `CleanupOldBinary` semantics unchanged. Tests: temp-dir cases for the
   path selection (`.old` present → `.old`; absent → `~`); rename failure logged not swallowed (seam).
2. **Config path (T1-7).** `config.Load` returns the path it actually loaded (or sets
   `cfg.LoadedFrom`); `main.go`/`services.go` build the store on that path; an explicit `-config` path
   that does not exist stays the save target (documented). Tests: load from `./config/config.toml` →
   store saves there; explicit missing path → created there. README/`data-and-storage.md` state the rule.
3. **Cookies lastError (T1-8).** `autocookies_refresh.go:358-360`, `:474-476`, `:479-481` call
   `s.setError(...)` with wording mirroring `FinishSetup`'s (`autocookies_setup.go:287/:333/:351`; the
   write exit keeps the Docker single-file bind-mount hint). Test: fail the `writeCookieFile` seam on the
   refresh path → `GetStatus().LastError` is set; the policy comment's writer list is updated.
4. **DeleteJob prune (T2-22).** The `OnJobDeleted` subscriber calls `s.db.ClearJobLogs(ev.JobID)`
   (exact, O(1)); the full-scan `PruneJobLogs` path is removed from this subscriber. Test: deleting one
   job clears only its buffer; a DB read error cannot wipe others.
5. **Port 0 (T4-31).** Delete the unreachable `configuredPort == 0` auto-pick branch and its comment in
   `main.go:368-393` (validate rewrites 0 → 774). Ruling: delete rather than support (nothing documents
   port 0; cost if wrong: re-add behind a validated `0` later).
6. **Plugin URL (T4-32).** `BUG_REPORT_LOCATION` → `https://github.com/vampiricwulf/Moombox/issues`;
   a test asserts the rendered plugin contains no other GitHub owner.
7. **Plugin status (T4-35).** `ytdlpplugin.Status` reports `Unparseable bool` (JSON `unparseable`)
   when the installed file fails `pluginURLRe`; the Web plugin card and the TUI `R Y` overlay show
   "plugin file not recognized — reinstall" in that state. Test: garbage file → `Installed=true,
   Unparseable=true, PortMismatch=false`.
8. **DPAPI on Linux (T4-34).** `dpapiExtractAsNetscape` (or the refresh gate) short-circuits when
   `runtime.GOOS != "windows"` with ONE Debug line `"DPAPI fallback is Windows-only; cookies.dpapi_fallback is ignored on this host"`
   and returns `dpapi.ErrNotSupported`. Test via a GOOS seam.
9. **AES-GCM once (T4-35).** `ReadChromeCookiesStats` builds the AEAD once per profile and passes it
   to `decryptV10Cookie`. Existing tests pin behaviour.
10. **YouTube chat adoption (T4-35).** `internal/chat/downloader.go:836-887` reads the existing chat
    file's summary with a streaming reader (mirror `readChatPartFileSummary` in `internal/twitch`)
    instead of `os.ReadFile`+`Unmarshal`. Test: a large synthetic file adopts with the same count and
    bounded allocations.

**Gates:** `./cmd/moombox/...`, `./internal/config/...`, `./internal/cookies/...`, `./internal/ytdlpplugin/...`, `./internal/chat/...`, `./internal/web/routes/`, `./internal/tui/...`, `./internal/docs/`.

## 11. Concurrency table (which arcs may run at the same time)

| Pair | Shared surface | Verdict |
|---|---|---|
| Arc 1 ↔ Arc 5 | `web/public/modules/*.js`, `web/tests`, `docs/spec/data-and-storage.md`/`user-interfaces.md` | NOT concurrent |
| Arc 5 ↔ Arc 7 | `cmd/moombox/main.go`, `internal/web/routes`, `internal/tui` | NOT concurrent |
| Arc 3 ↔ Arc 7 | `internal/cookies` (jar.go vs refresh/dpapi files) | concurrent OK (disjoint files; pathspec commits) |
| Arc 1 ↔ Arc 2 / 3 / 4 / 6 | none | concurrent OK |
| Arc 2 ↔ Arc 4 / 6 / 7 | none | concurrent OK |
| Arc 4 ↔ Arc 6 | none | concurrent OK |

Order: **1 + 2** → **3 + 4** → **5 + 6** → **7**. Each arc's branch is cut from the main HEAD at its
plan-commit time; a later arc rebases nothing — merge conflicts in docs are resolved at merge time.

## 12. Definition of done (chain)

Every ledger item T1-1 … T4-35 is either merged with a test, or recorded in the chain ledger as a
ruling (R2 faststart: no change; T3-27 concurrency: not done, deadline-aware retries instead). Memory
notes updated: cipher-cache figure (10 slots), the Twitch emote-offset fact (already written), and the
chain outcome. `RELEASE_NOTES.md` is NOT written (owner controls release).

## Errata (chain close, 2026-09-15)

Corrections recorded at chain close. The design sections above are left as written — they are the
record of what was PLANNED. These entries are what the implementation proved, and they win wherever
they disagree with the text above.

1. **§5.1 — "job → Error, which `isRecoverableTwitchError` can recover" is false.**
   `isRecoverableTwitchError` (`internal/monitor/twitch_recover.go`) has exactly one caller: the Twitch
   MONITOR, when it sights a live channel whose existing job is already in `Error`
   (`internal/monitor/twitch.go:405`). It is not a hand-off the HLS loop reaches, and the YouTube
   orchestrators finalize an errored job themselves. So the gain from deferring the end verdict is not
   "the job gets recovered": it is that an unverifiable playlist failure RETRIES instead of truncating
   the recording as Finished, and that the resume sidecar survives for a later `/resume`. The design's
   behaviour is unchanged and correct; only that clause of its justification was wrong.

2. **§5.5 — the 4xx drain was DROPPED, not implemented.** Go 1.27's `net/http` `Transport` already
   drains an unread response body of up to 256 KiB when the caller closes early (`maybeDrainBody`), so
   the brief's RED did not reproduce and the specified 64 KiB drain could not rescue a connection the
   stdlib does not already keep (verified with a 300 KiB body: red without the fix, still red with it).
   Arc 2 Task 5 was dropped by ruling and committed nothing. A body larger than 256 KiB is beyond any
   sane drain cap, so nothing is owed here.

3. **§7.3 — "so `ObserveLiveness` still receives a verdict" overstates the floor.** What
   `armMembershipLiveness` guarantees is one membership fetch per cycle that RETURNS, and only while
   some memoized channel has not recently errored; when every candidate has errored, the retry is
   bounded by `membershipLivenessMaxTries` and the cycle may legitimately end with nothing returned. A
   fetch that returns is also not the same thing as a verdict: `routeLivenessVerdict` forwards only
   `LoggedIn`/`LoggedOut`, so a page carrying no login marker observes nothing even after a successful
   fetch. Tier-2 `FallbackLiveness` remains the backstop for BOTH gaps — it is not a redundancy the
   memo made optional.

4. **§6.7 — "3-slot LRU" is the figure being CORRECTED, not a current one.** The sentence is written in
   the present tense against a state Arc 3 has since fixed; read it as past tense. The cipher solver
   cache holds at most 10 compiled VMs (`solverCacheSize`, `internal/cipher/solver.go`) — it was 3
   until 2026-04, when multi-channel monitoring routinely holding 4+ active player URLs made the
   smaller cap re-compile constantly. `.claude/skills/moombox-upstream-porting/SKILL.md` and
   `docs/spec/platform-services.md` both state 10 as of Arc 3.

5. **§10.1 — the case-(b) premise was wrong and was corrected inside Arc 7.** The design assumed that a
   second update in one launcher lifetime fails `os.Rename(.old → ~)` because a `~` file is already
   there. It does not: Go's `os.Rename` on Windows is `MoveFileEx` with `MOVEFILE_REPLACE_EXISTING`,
   which replaces a plain `~` file. The rename fails only when something denies delete-sharing on that
   name — in the field the launcher's own mapped image (which is also why the child's
   `CleanupOldBinary` could not delete it), or a directory at that path. The shipped test
   (`TestHandleUpdateRestartReportsARenameItCouldNotDo`, `cmd/moombox/launcher_windows_test.go`) pins
   the corrected premise and reproduces the failure with an ordinary open `*os.File` handle, which
   denies exactly the same sharing mode. The FIX (report the failure, prefer a surviving `.old` as the
   rollback artifact) is unaffected — it is right for the real failure mode too.

6. **Arcs 5 and 6 — the node-suite test counts in those plans were written against a 159 baseline.**
   Arc 1 raised `node --test web/tests/*.test.mjs` to 169 before either arc ran, so every absolute
   count quoted in their task steps was stale by 10 on arrival. Ruling taken at the time: the counts
   are informational, and implementers report the count they actually observe. The gate is the suite
   passing, never a number. Those plans are deleted, so this entry is the surviving record.

7. **Arc 6 — commit `fedf98c5` carries TWO tasks.** A pathspec-less `git commit` swept Task 5's
   already-staged `internal/jobfilter` files into Task 4's commit, whose subject names only T2-20d. The
   commit contains T2-20d (the TUI log-level cache) AND T2-24 (the jobfilter case-insensitive fold);
   both halves were reviewed separately, by pathspec-scoped diffs. Ruling: no history rewrite — the six
   hashes are cited across the ledgers and review packages, and the merge commit body records the
   pairing. The process rule this produced is now in the global constraints: implementers commit with
   `git commit -m … -- <their files>`, pathspec on the commit too.
