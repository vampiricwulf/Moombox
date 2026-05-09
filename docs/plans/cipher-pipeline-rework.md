# Cipher pipeline rework — plan

Status: **DESIGN APPROVED, NOT YET IMPLEMENTED** (as of 2026-05-08, post-v2.6.26)

This plan exists so a future Claude/Wolf session can resume this work
unambiguously after a compact. Every relevant fact, decision, and code
location is captured below — read this top-to-bottom before writing any
code.

---

## 1. The bug

`[PlayerApi] Signature decryption failed error="cipher: sig unavailable
for this player"` warnings flood the log when a player JS file rotates
on YouTube's side faster than our 14-day disk-cache TTL.

**Concrete instance** (2026-05-08 22:14 in `D:\Moombox\moombox.log`):
56 warnings in 5 seconds when adding video `bp4_7T9J6Fg`. The video
uses player `8fb635c2`. The cached JS for that player (file
`f7c8850bfd3ac4c2.js` in `%TEMP%/yt-cipher/player_cache/`) was
**2,465,495 bytes from 2026-05-07 09:50**. The current YouTube-served
JS at the same URL is **2,750,557 bytes** — a 285 KB / 12% growth from
a cipher-algorithm rotation within ~36 hours.

The warning was triggered because:
1. Every stale-cache `parseFormatsWithCipher` call sent the stale JS
   to the sidecar
2. ejs preprocessor recognized the algorithm structure no longer matches
   its expected patterns and threw `"ejs solve sig: no solutions"`
3. The error fell through silently to the goja fallback in `decryptSig`
4. Goja's HasSig returned false (cb017549-family limitation)
5. `ErrSigUnavailable` surfaced as the user-visible warning

**Verified the failure mode end-to-end** with `tools/sidecar-sig-probe`:
- Same player + STALE cached JS → sidecar returns `"ejs solve sig: no solutions"`
- Same player + FRESH JS from YouTube → sidecar returns valid 1311-char sig

So caching is fine; lack of revalidation is the bug.

## 2. What's already shipped (do NOT redo)

| Commit | What |
|---|---|
| `f62f5f9` | diag(youtube): surface sidecar Sig errors before goja fallback |
| (this commit) | `tools/sidecar-sig-probe` standalone reproducer |

**The diagnostic log** is now in `internal/youtube/player_api.go:138-153` —
when sidecar Sig returns an error, a Warn-level dedup'd log fires before
falling through to goja. Dedup key: `<playerID>|sidecar-err|<first 60 chars
of err>`. After a player rotation, the dedup'd log now shows the actual
sidecar error message exactly once, surfacing whatever transient/state
issue is in play.

## 3. Why caching exists (don't propose to remove it)

Player JS is used for three things only:
- **sig decryption** of `signatureCipher` entries
- **n-param decryption** of every videoplayback URL
- **STS extraction** for `/youtubei/v1/player` requests

Three cache layers:

| Layer | Lifetime | Hit when |
|---|---|---|
| `solverData` (in-memory compiled goja Runtime, ~44 MB live per player) | process lifetime | every sig/n call after the first |
| `<key>.preprocessed.js` (disk, sig/n-binding-injected) | 14d (today) | process restart, in-memory miss |
| `<key>.js` (disk, raw player JS) | 14d (today) | process restart, preprocessed miss |
| HTTP fetch from `youtube.com/s/player/<id>/.../base.js` | — | disk miss or stale |

**Steady-state HTTP fetch frequency: 1 per player per process lifetime.**
After that everything's in-memory. The disk cache exists for restart
latency (avoid 5-15s cipher unavailability post-restart) and bandwidth
(2.5 MB × restarts × players adds up over a 24/7 archiver session).

## 4. How often cipher actually fires (correcting an earlier assumption)

User flagged that earlier "sig/n is called a lot" was wrong. Verified
by tracing the code paths:

| Call site | Cipher calls per stream |
|---|---|
| `parseFormatsWithCipher` (current — wasteful) | 7-26 (one per format/adaptiveFormat entry) |
| DASH manifest URL sig+n via `RoutedResolveURL` | 0-1 |
| Per-stream BaseURL n-decrypt | 1-2 |
| HLS master URL n-decrypt | 0-1 |
| Quality probe (every 30s) | **0** — uses ANDROID_VR cookieless |
| `OnCipherFailure` recovery | 1-2 (rare) |
| Per-segment fetches | **0** — segment URLs pre-resolved at setup |

So per-stream-lifetime cipher calls: ~30-50 max, **clustered in a single
burst at stream setup**. yt-dlp's "fetch player once at start" model is
the right reference for thinking about validation cost.

## 5. Design

Three coordinated changes that compose:

### Change A — Reactive invalidation on stale-JS sidecar errors

**Problem:** if the player JS rotates between Moombox stream-setup
events and the in-memory `solverData` cache is hot for the old player,
we'll happily ship the stale solver to the sidecar and get
`"ejs solve sig: no solutions"`. Currently swallowed; falls through to
goja; user sees `ErrSigUnavailable` warning.

**Sidecar protocol gap (must fix as part of Stage 2).** The Node sidecar
in `bgutil-sidecar/src/cipher.js:96-105` uses `if (!entry)` semantics:
once a `playerID` is cached, subsequent calls with fresh `playerJS` are
silently ignored. Resending the same playerID with new bytes is a no-op,
so a naive "retry with fresh JS" cannot work. Two options:

1. Add `forceReload: true` field to the `solveCipher` JSON-RPC request.
   When set, sidecar deletes the cached entry before the `if (!entry)`
   check. Smallest protocol delta; preferred.
2. Add a separate `forgetPlayer(playerID)` JSON-RPC method. Cleaner
   semantically but adds a round-trip.

Going with option 1.

**Fix:** in `sidecarSolver.solve`:
- Recognize stale-JS sidecar errors. New sentinel `cipher.ErrPlayerJSStale`.
  Match on substrings `"ejs solve sig: no solutions"` and `"ejs preprocess"`.
- On stale: `playerCache.Remove(playerURL)` (wipes both raw + preprocessed
  files), `gojaResolver.InvalidateSolver(playerURL)` (wipes in-memory
  solverData), `clearPlayerSent(playerID)`, retry `callOnce` once with
  fresh JS attached AND `forceReload=true` so sidecar evicts its V8 cache.
- If retry still fails: surface as `ErrPlayerJSStale` (permanent — likely
  a real Moombox bug, not a transient state issue).

This needs:
- `cipher.PlayerSource` to grow a `RemovePlayerJS(playerID)` method,
  with `GojaResolver` providing the implementation (already has
  `InvalidateSolver` internally).
- `sidecar.SolveCipherRequest` (Go) and the JSON-RPC schema in
  `bgutil-sidecar/src/server.js` + `cipher.js` to grow `forceReload bool`.
- `cipher.js:96` becomes `if (!entry || forceReload) { ... }` with an
  explicit `playerCache.delete(playerID)` before re-loading when
  `forceReload && entry` to drop the old sig/n binding caches too.

### Change B — Conditional-GET revalidation in `PlayerCache.Fetch`

**Problem:** `PlayerCache.Get` returns cached JS if file mtime is < 14d
old. No content validation against YouTube. Player JS rotates within
hours; cache happily serves stale.

**Fix:** in `PlayerCache.Fetch(ctx, playerURL) (js string, changed bool, err error)`:
- If disk cache present:
  - Read companion `<key>.meta.json` (captured below) for last-known
    `Last-Modified` / `ETag` / `Content-Length` from YouTube
  - Send HTTP GET with `If-Modified-Since: <stored Last-Modified>` and
    `If-None-Match: <stored ETag>` if present (replay YouTube's own
    timestamps — file mtime is *our* write time, which is later than
    YouTube's `Last-Modified` and would cause some CDN edges to return
    304 indefinitely under strict comparison)
  - On 304: return `(cached, false, nil)`, refresh file mtime so the
    24h TTL backstop doesn't expire revalidated content
  - On 200: write new file + new `<key>.meta.json`, return `(new, true, nil)`.
    If the response `Content-Length` matches stored size, treat as
    unchanged regardless of `Last-Modified` value (YouTube CDN's
    `Last-Modified` is occasionally split-cached and lies; size is
    reliable). When meta is missing (post-upgrade migration), fall
    back to mtime as `If-Modified-Since` for the first revalidation —
    next 200 response captures real metadata.
- If no disk cache: full GET, persist meta, return `(new, true, nil)`.
- **Network-error behavior during revalidation.** If the conditional
  GET fails (DNS, TCP, TLS, timeout) AND a non-expired disk cache exists,
  return `(cached, false, nil)` with a Debug-level log. Mirrors how
  yt-dlp tolerates transient CDN failures and prevents a momentary
  network blip from forcing every concurrent stream-setup to fail.
  If no disk cache exists, the network error is fatal as today.
- When `Fetch` writes a new raw file, it MUST also remove
  `<key>.preprocessed.js` — single ownership of cache invalidation
  inside `PlayerCache`, not split between cache and solver.
- **Singleflight coalescing** keyed on `CacheKey(playerURL)` (NOT raw
  playerURL) so two callers passing the same player via different
  locale URL shapes share one round-trip. The locale-collapse rationale
  is the same one driving the existing in-memory solver cache key.

New `<key>.meta.json` schema:
```json
{ "lastModified": "...", "etag": "...", "contentLength": 2750557 }
```
All fields optional; absence means "no header from YouTube last time."

Use `golang.org/x/sync/singleflight`.

### Change C — Defer cipher decryption to post-selection

**Problem:** `parseFormatsWithCipher` decrypts sig+n on every adaptive
format URL during parsing — 7-26 cipher calls per stream setup — even
though format selection later picks 1-2 and discards the rest. Pure
waste. Compounds the stale-cache problem because more cipher calls = more
chances for the stale JS to surface.

**Fix:** `Format` struct gains:
- `EncryptedSig string` — the `s` field from `signatureCipher` parse, if
  the entry was sigCipher-only
- `SigKey string` — the `sp` field, defaulting to `"signature"`

`parseFormatsWithCipher` becomes `parseFormats` — pure metadata + raw
URL extraction. For `signatureCipher` entries it stores the URL part
(from sigCipher's `url` query parameter) AND `EncryptedSig` /
`SigKey` separately.

New helper in `internal/cipher/decrypt.go`:
```go
func ResolveFormatURL(ctx context.Context, routed Solver, goja *GojaResolver,
    playerURL string, format ResolveFormatRequest) (string, error)
```
where `ResolveFormatRequest` carries `URL`, `EncryptedSig`, `SigKey`,
and the function:
- If `EncryptedSig != ""`: solve sig via routed, append `&{sigKey}={decrypted}`
- Always run n-decryption via `RoutedDecryptNInURL` on the result

Each strategy (`strategy_youtube_dash.go`, `strategy_youtube_hls.go`,
`strategy_youtube_manifestless_dash.go`, `strategy_youtube_vod.go`) calls
`ResolveFormatURL` for the chosen format(s) right before constructing
the SegmentDownloader.

Drop the n-fail-as-filter from parsing. Today, formats whose n fails
silently disappear before selection; after Stage 3 they'd survive parse,
get selected, and 403 at runtime — strictly worse than today unless
selection retries.

**Required: re-selection fallback.** When `ResolveFormatURL` returns an
error during strategy setup, the strategy must re-run format selection
excluding the failed itag before bubbling up. Use an exclusion set
passed to the selector (NOT mutating Format — `Format` is value-typed
and slices hold copies; mutation wouldn't propagate). Roughly:
```go
exclude := map[int]bool{}
chosen := SelectBestDashStream(formats, exclude, …)
resolved, err := cipher.ResolveFormatURL(ctx, …, chosen)
if err != nil {
    exclude[chosen.Itag] = true
    chosen2 := SelectBestDashStream(formats, exclude, …)
    if chosen2 == nil { return err }
    resolved, err = cipher.ResolveFormatURL(ctx, …, chosen2)
    // bubble err if still failing — bounded to two attempts so a fully
    // broken cipher state surfaces as a real setup error rather than
    // looping through every format
}
```
Bound to two attempts; we don't need full retry-everything because in
practice n-decrypt either works for all formats from a player or none,
and Stage 1+2 catch the "none" case structurally. The shared helper
in `internal/worker/format_selection.go` should encapsulate the
exclusion-set plumbing so each strategy stays clean.

### Composition

After all three:

| | Before | After |
|---|---|---|
| Cipher calls per stream setup | 7-26 | 1-2 |
| Full player JS body downloads | 0-1 per setup | 0 when unchanged, 1 on rotation |
| Conditional GET round-trips per setup | 0 | 1 (singleflight-coalesced; 304 = no body) |
| Stale-cache failure recovery | manual restart | reactive within one cipher call |
| Steady-state download cipher work | 0 | 0 |

A is the safety net that catches what B misses (and what slips past the
solverData TTL eventually). B is the proactive validation. C reduces the
attack surface for both.

## 6. Implementation order

Stage so each commit is independently reviewable and shippable:

### Stage 1 — Conditional-GET in PlayerCache (~110 lines)

Files:
- `internal/cipher/player_cache.go`:
  - Modify `Fetch` to return `(string, bool, error)`
  - Persist `<key>.meta.json` with `Last-Modified` / `ETag` / `Content-Length`
    from each 200 response; load it before each Fetch
  - Replay stored `Last-Modified` / `ETag` as `If-Modified-Since` /
    `If-None-Match` (NOT file mtime — see Change B rationale)
  - On 200, remove `<key>.preprocessed.js` so the solver layer doesn't
    have to know about the staleness; cache owns its invalidation
  - Singleflight keyed on `CacheKey(playerURL)`
- `internal/cipher/solver.go` — `compileSolver` consumes the new
  `changed bool`. On `changed=true`, also drop the in-memory
  `solverData` entry for this CacheKey so the next call recompiles
  from the fresh JS. (The preprocessed.js removal lives in `Fetch`,
  not here.)
- `internal/cipher/player_cache_test.go` — six new tests:
  - 304 path: cached returned, no content rewrite, mtime refreshed,
    meta unchanged
  - 200 with different `Content-Length`: cache evicted, new content
    stored, meta updated, `changed=true`, `<key>.preprocessed.js`
    removed if present
  - 200 with identical `Content-Length`: treated as unchanged
    (Last-Modified flap defense), meta refreshed, `changed=false`
  - **Migration**: cached file present, no `<key>.meta.json` →
    conditional GET uses file mtime as `If-Modified-Since` fallback;
    next 200 captures real headers into meta
  - **Network failure during revalidation**: cached present, server
    unreachable → returns cached with `changed=false`, no error
  - Singleflight: 5 concurrent fetches for same URL → 1 round-trip;
    5 concurrent fetches for two locale variants of the same playerID
    → 1 round-trip
- Drop `playerCacheTTL` from `14 * 24 * time.Hour` to `24 * time.Hour`
  (NOT optional — with conditional-GET in place the TTL only matters
  offline, and 14d offline is useless anyway since the player has
  rotated)

### Stage 2 — Reactive invalidation in sidecarSolver (~100 lines)

Files:
- `bgutil-sidecar/src/cipher.js` — `solveCipher` accepts `forceReload`;
  when truthy AND an entry exists, `playerCache.delete(playerID)` before
  the existing `if (!entry)` load path. Bump sidecar version constant
  if there is one so a Moombox build mismatch is visible.
- `bgutil-sidecar/src/server.js` — surface the new field through
  whatever JSON-RPC dispatcher lives there (passthrough; no logic change)
- `internal/bgutils/sidecar/types.go` (or wherever `SolveCipherRequest`
  is defined Go-side) — add `ForceReload bool` field, JSON tag
- `internal/cipher/errors.go` — new `ErrPlayerJSStale` sentinel
- `internal/cipher/solver_sidecar.go`:
  - Recognize stale-JS error patterns (`"ejs solve sig: no solutions"`,
    `"ejs preprocess"`); promote to `ErrPlayerJSStale` internally
  - On stale: call `src.RemovePlayerJS(playerID)`, `clearPlayerSent`,
    retry `callOnce` once with `includeJS=true` AND `forceReload=true`
  - If retry still fails: return `ErrPlayerJSStale` to the caller
  - Order: stale-JS handling sits alongside the existing
    `ErrPlayerNotLoaded` retry — both are "evict + retry once" patterns
    on different error classes; keep them as parallel `if errors.Is(...)`
    branches in `solve`, not nested
- `internal/cipher/types.go` — extend `PlayerSource` interface with
  `RemovePlayerJS(playerID string) error`
- `internal/cipher/solver.go` — `GojaResolver.RemovePlayerJS`:
  - Calls `pc.Remove(playerURLForID(playerID))` to wipe disk
  - Drops the in-memory `solverData[CacheKey]` entry
- `internal/cipher/solver_sidecar_test.go` — three new tests:
  - sidecar returns `"ejs solve sig: no solutions"` → cache wiped,
    retry with `forceReload=true` + fresh JS → success
  - sidecar returns same error twice (after fresh JS) → permanent
    `ErrPlayerJSStale`
  - **`forceReload` round-trip**: when set, the fake sidecar drops its
    in-memory entry; without it, the second call hits the cached
    (broken) JS again. Pins the protocol contract.

### Stage 3 — Defer per-format cipher decryption (~180 lines)

Files:
- `internal/youtube/types.go` — `Format` struct gains `EncryptedSig`,
  `SigKey` fields (both `,omitempty`). Document that `Format.URL` is now
  "may be ciphered; resolve via `cipher.ResolveFormatURL` before use."
- **Pre-task: `Format.URL` reader sweep.** Before changing parse,
  `grep -rn 'Format' internal/ web/ cmd/` for callers reading `.URL`
  outside the strategy/resolver path. Known suspects:
    - Engine quality probe / dashboard JSON (display only — fine to
      show "ciphered" placeholder, but verify it doesn't try to fetch)
    - Any test fixtures that hard-code resolved URLs (update or accept
      that fixture URLs will need cipher resolution too)
  Findings drive whether additional consumer-side changes are needed.
- `internal/youtube/player_api_parsing.go` — `parseFormatsWithCipher`
  → `parseFormats`. Remove sig + n decrypt loops. For sigCipher-only
  entries, store the URL part (sigCipher's `url` query param) in
  `Format.URL`, and `s` / `sp` parts in `EncryptedSig` / `SigKey`.
  Direct-URL entries leave `EncryptedSig` empty.
- `internal/cipher/decrypt.go` — new free function (consistent with
  `RoutedDecryptNInURL` / `RoutedResolveURL`):
  ```go
  func ResolveFormatURL(ctx context.Context, routed Solver,
      goja *GojaResolver, playerURL string,
      req ResolveFormatRequest) (string, error)
  ```
  Behaviour: solve sig (if `EncryptedSig != ""`), append
  `&{SigKey or "signature"}={decrypted}`, then run n-decryption on
  the result.
- Strategy call sites (each adds ~5-10 lines for resolve + fallback):
  - `internal/worker/strategy_youtube_dash.go` — resolve chosen video
    + audio streams pre-SegmentDownloader; on failure mark broken,
    re-select once
  - `internal/worker/strategy_youtube_hls.go` — resolve master URL
    only (segment URLs come from playlist parsing); same fallback
  - `internal/worker/strategy_youtube_manifestless_dash.go` — resolve
    chosen adaptiveFormat URLs; same fallback. Currently a no-op
    because parse pre-decrypted; after this change parse leaves URLs
    raw and strategy resolves them
  - `internal/worker/strategy_youtube_vod.go` — `DownloadVod` resolves
    chosen format pre-engine
  - The re-selection helper (~15 lines) lives in
    `internal/worker/format_selection.go` (new file or extend existing
    selector) so all four strategies share one fallback impl
- `cipherNeededButMissing` warn — relocated to strategy phase. Each
  strategy that detects `EncryptedSig != ""` on the chosen format AND
  no cipher wired emits a single Warn per process (sync.Once-style),
  same observability as today but at the right semantic point.
- Tests:
  - `parseFormats` collects all formats including sigCipher-only,
    populates `EncryptedSig` + `SigKey` correctly
  - `ResolveFormatURL` with sigCipher entry produces URL with
    `&signature=DECRYPTED` appended
  - `ResolveFormatURL` with already-resolved URL just runs n-decrypt
  - `ResolveFormatURL` failure on chosen format → strategy re-selects
    excluding it → second resolve succeeds (DASH and manifestless DASH
    each)
  - `ResolveFormatURL` failure twice → strategy bubbles error (bound
    holds)
  - Strategy build path correctly resolves chosen format(s) only;
    other formats stay raw in the `[]Format` slice the dashboard sees

## 7. Tests + verification

After each stage, run:
```
go build ./... && go vet ./... && go test ./internal/cipher/... \
    ./internal/youtube/... ./internal/worker/... ./internal/engine/...
```

Live verification against `bp4_7T9J6Fg` (or another video using
`8fb635c2`):
1. Manually delete `%TEMP%/yt-cipher/player_cache/f7c8850b*.js` to
   simulate fresh-state
2. Run Moombox, add the video
3. Expect: ~1 HEAD round-trip (or 1 full GET if cache empty), zero
   `cipher: sig unavailable` warnings, download completes

Also run `go run ./tools/sidecar-sig-probe -wp /d/Moombox/_wp.html`
after refreshing the watch page — should print "*** SUCCESS ***" with
a non-empty decrypted sig regardless of cache state.

## 8. Risks + edge cases

- **YouTube returns 304 but content actually rotated** (split CDN race).
  Handled by Stage 2's reactive path catching the resulting "no solutions"
  on the next sig solve.
- **YouTube returns 200 with same content** (Last-Modified flap). Stage 1's
  Content-Length comparison catches this — if size matches, treat as
  unchanged regardless of `Last-Modified` value.
- **Singleflight key.** Resolved in design: key on `CacheKey(playerURL)`
  so locale-variant URLs collapse, matching the existing solver-cache key.
- **Stage 3 changes the `Format` JSON shape** (new EncryptedSig/SigKey
  fields). Add `omitempty` so existing JSON consumers don't see noise on
  resolved formats.
- **In-flight downloads during Stage 3 deploy.** `Format` parses come from
  the response, so on next stream-setup post-upgrade, things just work.
  No persistence of Format in DB.
- **Stage 2 ErrPlayerJSStale shouldn't fire for actual player JS bugs**
  (where we've shipped wrong code, not where YouTube rotated). The retry
  logic ensures we eat one false-positive but bubble up if persistent.

## 9. Status snapshot — where we are now

- Diagnostic log shipped (`f62f5f9`); will surface root cause on next
  reproduction
- Probe tool shipped (`tools/sidecar-sig-probe`); reproduces the
  exact failure end-to-end
- Stale cached file `f7c8850bfd3ac4c2.js` already manually evicted
  during this session, so the user's next attempt will fetch fresh
- v2.6.26 is the latest tag; this work would be v2.6.27 or v2.6.28
  (one tag per stage, or a single rollup tag — ship's call)
- All three stages have been designed, no implementation started

## 10. Decisions baked in (no longer open)

These were open questions in earlier drafts; resolved here so a
post-compact session doesn't relitigate them:

1. **TTL drop to 24h** — required, not optional. With conditional-GET
   in place, TTL only matters offline; 14d offline is meaningless because
   the player has rotated.
2. **`ResolveFormatURL` shape** — free function `cipher.ResolveFormatURL`,
   matching `RoutedDecryptNInURL` / `RoutedResolveURL` conventions.
3. **`cipherNeededButMissing` warning** — keep, relocate to strategy
   phase. Sync.Once-style emission per strategy when a chosen format has
   `EncryptedSig != ""` AND no cipher is wired.
4. **Sidecar protocol change** — `forceReload` flag added to
   `solveCipher` JSON-RPC. Smallest delta vs adding a separate
   `forgetPlayer` method.
5. **Conditional-GET headers source** — replay YouTube's own
   `Last-Modified` / `ETag` from `<key>.meta.json` (NOT file mtime).

If something genuinely new comes up during implementation, ask via
AskUserQuestion before guessing — Wolf has been responsive about
mid-implementation clarifications.

---

End of plan.
