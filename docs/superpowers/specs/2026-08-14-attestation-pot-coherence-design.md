# Attestation-Challenge POT Coherence (moonarchive parity)

**Date:** 2026-08-14
**Status:** Approved design, pending implementation plan
**Trigger:** Premiere `LcYRqZxJEko` (2026-08-14) — every GVS segment request 403'd
for the full ~7-minute broadcast (1,543 attempts, seqs 0–47, zero bytes written),
on both the original ANDROID_VR-sourced URLs and freshly re-resolved ones. Regular
live streams through the identical pipeline work. Full investigation in session
logs; summary below.

## 1. Problem

GVS PO tokens for segment downloads are currently minted by a BotGuard minter
whose challenge the sidecar fetches itself from `/att/get` — anonymously
(`ENGAGEMENT_TYPE_UNBOUND`, hardcoded WEB client context, bgutils-js UA, no
cookies). The resulting token has no tie to the session that extracted the
video. Live broadcasts do not enforce GVS POT validation, so this incoherence
is invisible on regular streams. Premiere broadcasts (`source=yt_premiere_broadcast`)
evidently do enforce it (yt-dlp `_base.py` on android_vr: "Since 2026.07,
intermittent/selective POT enforcement has been observed for non-HLS formats"),
and every segment fetch is rejected with 403.

moonarchive (the live-archiver reference Moombox ports segment strategies from)
hit and fixed this class of rejection in commits dated 2026-07-10 → 2026-08-05:

- `96344fe` extracts the attestation challenge YouTube embeds in the watch page
  (`window.ytAtN({...})` → `R` → `bgChallenge`) and passes it to the PO-token
  minter, so the token is minted from the session's own challenge.
- The same commit sets `bypass_cache=true` on every mint: "the server only keys
  this on source address, meaning it might use a minter from a different session."
- moonarchive binds GVS tokens to the **video ID** and mints once per recording
  session with the page challenge captured at startup.

The upstream bgutil-ytdlp-pot-provider server already supports an externally
supplied webpage challenge (`session_manager.ts` "Using challenge from the
webpage" branch); our sidecar is the stripped variant that never receives one.

## 2. Goal

GVS (segment-URL) PO tokens are minted by a fresh BotGuard minter built from the
job's own watch-page attestation challenge, bound to the video ID — full
moonarchive parity — with mint-provenance logging rich enough that a failed
trial months from now still identifies which variable was in play.

### Strategy note: why all variables at once

Premieres on monitored channels are rare; the next trial may be months out. With
scarce trials the goal flips from maximizing information per trial to maximizing
the probability the one trial succeeds, so we adopt the complete known-working
moonarchive configuration in one step rather than isolating variables across
rare test windows. The regression-risk half (videoID binding on regular live)
self-verifies within days because live streams are frequent; the payoff half
(premieres) waits for the next premiere. Diagnostic power is recovered through
provenance logging (§4.6) instead of change restraint.

## 3. Non-goals (deferred, tracked separately)

- Blanket-403 failover to another format source/strategy (e.g. web_safari HLS)
  when every segment including the head 403s.
- Zero-byte pre-mux guard (live job finishing with 0 bytes should fail
  truthfully instead of running ffmpeg on empty files).
- ANDROID_VR-wins-dedup preference revisit for premieres, and the stale
  "cookied formats win" comment at `player_api_strategy.go:168`.
- Datasync-ID binding for cookie-authenticated GVS fetches (next suspect if
  this change doesn't fix premieres).
- Goja-fallback (internal/bgutils) challenge support — sidecar only.
- Mid-job POT rotation (see §7 open question).
- moonarchive `567b565` missing-streamingData robustness port.

## 4. Design

### 4.1 `js_to_json` utility — `internal/utils/jsjson.go`

Faithful Go port of yt-dlp's `js_to_json` (`yt_dlp/utils/_utils.py`), keeping
signature parity: `JSToJSON(code string, vars map[string]string, strict bool)
(string, error)`. Owner chose the full port over a targeted extractor for
reusability and drift tolerance.

RE2 constraints: yt-dlp's `(?<![0-9])[eE]` and `(?<!\.)0+[0-7]+` lookbehinds
must be reworked (leading capture group + manual boundary check in the
replacement callback). Inline `(?s)` flags are supported. The port must handle
the constructs the main regex dispatches on: string quotes (`'`, `"`, ``` ` ```
with `${}` template substitution), comments, trailing commas, `void 0`,
`undefined`, hex/octal integers, `!!`-prefixed values, `new Date(...)`,
`Array(...)`, `new Map(...)`, `parseInt(...)`, and IIFE unwrapping.

Tests: port yt-dlp's `js_to_json` test cases from its test suite so behavior is
pinned against upstream.

### 4.2 Watch-page extraction — `internal/youtube/watch_page.go`

`extractAttestationChallenge(html string) string`:

1. Regex capture (moonarchive's pattern, RE2-safe):
   `window\.ytAtN\(\s*(\{[\s\S]*?\})\s*\)`.
2. `JSToJSON` on the captured object literal → `json.Unmarshal`.
3. The `R` key holds a JSON **string**; unmarshal it, take `bgChallenge`.
4. Re-marshal `bgChallenge` compact; that JSON string is the challenge payload.

Failure semantics: absent blob, `JSToJSON` error, missing `R`/`bgChallenge` —
all return `""` with a Debug log. Never an error; the sidecar's `/att/get`
fallback preserves today's behavior exactly.

Plumbing: new `WatchPageResult.AttestationChallenge string`, carried onto a new
`VideoInfo.AttestationChallenge string` in **both** `GetVideoInfoAuthenticated`
and `GetVideoInfoPublic`, on **all** return paths. Mechanism: explicit
assignment on the returned `*VideoInfo` at every return site via a small helper
— NOT via `mergeWatchPageMetadata` (several early returns — web_embedded,
web_creator, android_vr, watch-page fallback — skip it or call it with a nil
source). The live refresh loop re-extracts every few minutes, so re-mints
after a downloader restart use the freshest extraction's challenge.

### 4.3 Provider — `PotProvider.GenerateGvsPoToken`

New method, used only by segment-download strategies:

```go
func (pp *PotProvider) GenerateGvsPoToken(ctx context.Context, binding, challenge string) (string, error)
```

Semantics (owner's "fresh minter per GVS mint" choice):

- Bypasses the session cache entirely — no read, no write. Inflight dedup keyed
  on binding still applies so concurrent same-binding GVS calls share one mint.
- Sidecar path: RPC with `freshMinter: true` and the challenge (when non-empty).
- Sidecar unhealthy / errors: falls through to the existing goja flow with the
  challenge ignored — degrades to exactly today's behavior (log this: §4.6).
- New counters in `PotStats`: `gvs_mints` (attempts through this method),
  `gvs_mints_challenge` (mints where a page challenge was supplied).

The existing `GeneratePoToken` / `GeneratePoTokenString` remain untouched for
player-API POT injection (`fetchWithClient`, `fetchWithEmbedded`).

### 4.4 Sidecar — RPC + `generateMinter(challenge)`

`generatePoToken` params gain two optional fields:

```
{ "binding": str, "challenge": str|undefined, "freshMinter": bool|undefined }
```

- `generateMinter(challenge)` accepts a parsed challenge object; only when
  absent does it run its own `/att/get` fetch (mirrors upstream
  `session_manager.getDescrambledChallenge`). The page `bgChallenge` has the
  identical shape (`program`, `globalName`, `interpreterUrl.privateDoNot...`).
- `freshMinter: true` forces a regeneration even when `cachedMinter` is valid.
  The result **replaces** `cachedMinter`, so subsequent player-API mints
  passively use the most session-coherent minter. The existing `minterPromise`
  in-flight dedup is kept: two jobs starting simultaneously share the first
  request's regen (and its challenge) — same session, acceptable; documented in
  a comment. The existing stale-`globalName` VM cleanup applies unchanged.
- RPC result gains provenance so the Go side logs truth, not assumption:
  `{ poToken, binding, expiresAt, minterSource: "challenge"|"att_get",
  minterFresh: bool }`.
- Protocol compatibility: the embedded sidecar ships in lockstep with the
  binary, so no cross-version concern; unknown params are ignored by JSON
  dispatch regardless.
- Build step: sidecar JS changes require `cd bgutil-sidecar && node build.mjs`
  to regenerate the embedded tarball before `go build`.

### 4.5 Binding switch — videoID for all GVS mints

- `manifestlessPotBinding` collapses: the manifestless path always binds to
  `job.Job.VideoID` (moonarchive parity). The experiment-detection branch
  (`DashManifestURL == ""` → videoID) becomes vacuous and is removed with a
  comment pointing here.
- HLS and DASH-manifest strategies' GVS mints also switch to videoID and route
  through `GenerateGvsPoToken` with the job's challenge — one token discipline
  for every segment download.
- Player-API POT injection keeps visitorData binding (different token class;
  yt-dlp binds player POTs to visitor data).
- Explicit risk: the videoID switch applies to regular live streams, which work
  today with visitorData. First live capture after deploy is the regression
  check; revert is one line per strategy.

### 4.6 Logging & observability

Two purposes: (a) if the next premiere still fails, the logs alone must say
which configuration was in play (variable identification without re-trial);
(b) every line involved in a download is attributable to a job (the 2026-08-14
investigation was hampered by engine 403 lines carrying no job ID).

1. **Mint provenance line** — one INFO line per GVS mint attempt, e.g.:

   ```
   [POT] GVS mint  jobID=… binding=videoID challenge=page|none
   minterSource=challenge|att_get|goja-fallback minterFresh=true|false
   sidecar=true|false tokenLength=…
   ```

   `minterSource`/`minterFresh` come from the sidecar RPC result; the
   goja-fallback path logs `minterSource=goja-fallback`. Mint failures log the
   same fields at Warn with the error.

2. **Job-scoped engine logging** — new small wrapper in `internal/worker`
   implementing the standard anonymous 4-method logger interface, appending
   fixed args to every call:

   ```go
   type scopedLogger struct { inner logger; args []any }
   ```

   Applied to `DownloaderOptions.Logger` for both segment downloaders, scoped
   with `jobID` **and** `stream=video|audio` (the investigation's interleaved
   "catch-up segment permanently gone" lines were ambiguous between the two
   downloaders). Engine code is unchanged — scoping is purely at construction.

3. **403-invalidation attribution** — `invalidate403Caches` and the
   `OnCipherFailure` closures include `jobID` in their log lines (today the
   "[Cipher] … 403 signal" lines carry only the player URL).

   A broader "scope `JobContext.Logger` itself at job creation" refactor is
   noted as a possible follow-up but out of scope — it would double-tag the
   many existing call sites that pass `jobID` manually.

### 4.7 Data flow (end state)

```
FetchWatchPage (cookies, session UA)
  └─ extractAttestationChallenge(html) ──► WatchPageResult.AttestationChallenge
GetVideoInfo{Authenticated,Public}
  └─ VideoInfo.AttestationChallenge
Download{ManifestlessDash,Dash,Hls}
  └─ PotProvider.GenerateGvsPoToken(ctx, videoID, challenge)
       ├─ sidecar healthy ── RPC {binding, challenge, freshMinter:true}
       │    └─ generateMinter(challenge)      [/att/get only if challenge==""]
       │         └─ fresh minter replaces cachedMinter → mint(binding)
       └─ sidecar down ──── goja flow, challenge ignored (today's behavior)
Player-API POT injection: unchanged (GeneratePoToken, visitorData, cached minter)
```

### 4.8 Cost

Fresh mint adds one BotGuard run (2–10 s) per GVS mint: at download start and
on any downloader-restart re-mint. Accepted by owner; early live segments are
covered by catch-up fetching. The FRESH-2 proactive-refresh optimization still
serves the player-API path via the (replaced) cached minter.

## 5. Error handling / degradation matrix

| Condition | Behavior |
|---|---|
| `ytAtN` absent / parse fails | challenge `""` → sidecar `/att/get` flow (today's behavior), Debug log |
| Sidecar down at GVS mint | goja fallback, challenge ignored, provenance logged as `goja-fallback` |
| Fresh-minter regen fails in sidecar | RPC error → PotProvider falls through to goja (existing path) |
| Concurrent GVS mints, cold sidecar | shared regen via `minterPromise` (first challenge wins) |
| Challenge stale (job extracted hours ago) | used as-is; moonarchive reuses one challenge per run, freshness not required. Refresh loop re-extracts anyway |

## 6. Testing

- `internal/utils/jsjson_test.go`: ported yt-dlp `js_to_json` cases.
- `internal/youtube/watch_page_test.go`: ytAtN fixtures — present, absent,
  malformed JS, missing `R`, missing `bgChallenge`.
- `internal/bgutils/pot_provider_test.go`: `GenerateGvsPoToken` bypasses the
  session cache (no read, no write), nil-sidecar fallback, counters.
- Sidecar smoke tests: `generatePoToken` with `challenge`/`freshMinter` params;
  provenance fields in the result. Env-gated live test
  (`MOOMBOX_LIVE_BG_TEST=1`) minting from a real page challenge.
- Worker: `scopedLogger` arg-append behavior; downloader construction passes
  scoped loggers.

## 7. Open question (verify during implementation)

Whether the mid-job 403-invalidation chain re-mints the segment POT or only
swaps the URL (`DownloaderOptions.PoToken` appears static after construction).
If it does not re-mint, do **not** add rotation here — note it in the deferred
blanket-403 failover work. If it does, that path must use `GenerateGvsPoToken`
with the job's challenge.

## 8. Rollout & verification

1. Ship; watch the first regular live capture (days): confirm segments flow
   with `binding=videoID` + challenge-sourced minter (regression gate).
2. Next premiere (possibly months): the actual fix trial. If it still 403s,
   the provenance line tells us the exact configuration; datasync-ID binding
   (§3) is the next suspect.
3. Doc update: `docs/spec/platform-services.md` BotGuard/POT section.
4. Revert points: binding switch is one line per strategy; challenge plumbing
   degrades to `/att/get` automatically if extraction breaks.
