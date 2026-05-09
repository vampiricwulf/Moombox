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

**Fix:** in `sidecarSolver.solve`:
- Recognize stale-JS sidecar errors. New sentinel `cipher.ErrPlayerJSStale`.
  Match on substrings `"ejs solve sig: no solutions"` and `"ejs preprocess"`.
- On stale: `playerCache.Remove(playerURL)` (wipes both raw + preprocessed
  files), `gojaResolver.InvalidateSolver(playerURL)` (wipes in-memory
  solverData), retry `callOnce` once with fresh JS attached.
- If retry still fails: surface as `ErrPlayerJSStale` (permanent — likely
  a real Moombox bug, not a transient state issue).

This needs `cipher.PlayerSource` to grow a `RemovePlayerJS(playerID)`
method, and `GojaResolver` to expose it (already has `InvalidateSolver`).

### Change B — Conditional-GET revalidation in `PlayerCache.Fetch`

**Problem:** `PlayerCache.Get` returns cached JS if file mtime is < 14d
old. No content validation against YouTube. Player JS rotates within
hours; cache happily serves stale.

**Fix:** in `PlayerCache.Fetch(ctx, playerURL) (js string, changed bool, err error)`:
- If disk cache present:
  - Send HTTP GET with `If-Modified-Since: <cached file mtime>`
  - On 304: return `(cached, false, nil)`, touch the file mtime
  - On 200: write new file, return `(new, true, nil)`. Compare
    `Content-Length` against cached size as secondary confirmation —
    YouTube CDN's `Last-Modified` is occasionally split-cached and
    lies; size is reliable.
- If no disk cache: full GET, return `(new, true, nil)`.
- **Singleflight coalescing** by `playerURL` so concurrent
  GetSolvers calls for the same player share one round-trip.

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

Drop the n-fail-as-filter from parsing. Post-selection cipher failures
are surfaced as setup errors; the selector can fall back to next-best
format on retry. (In practice this rarely fires because n-decrypt either
works for all formats from a given player or for none.)

### Composition

After all three:

| | Before | After |
|---|---|---|
| Cipher calls per stream setup | 7-26 | 1-2 |
| HTTP fetches of player JS per stream lifetime | 0-1 | 0 (HEAD only after first) |
| HEAD round-trips per stream lifetime | 0 | 1 (singleflight-coalesced) |
| Stale-cache failure recovery | manual restart | reactive within one cipher call |
| Steady-state download cipher work | 0 | 0 |

A is the safety net that catches what B misses (and what slips past the
solverData TTL eventually). B is the proactive validation. C reduces the
attack surface for both.

## 6. Implementation order

Stage so each commit is independently reviewable and shippable:

### Stage 1 — Conditional-GET in PlayerCache (~80 lines)

Files:
- `internal/cipher/player_cache.go` — modify `Fetch` to return `(string, bool, error)`, add If-Modified-Since logic, add singleflight coalescing
- `internal/cipher/solver.go` — `compileSolver` updated to handle new return signature; on `changed=true`, also remove the `<key>.preprocessed.js` file
- `internal/cipher/player_cache_test.go` — three new tests:
  - 304 path: cached returned, mtime touched, no content rewrite
  - 200 path with different size: cache evicted, new content stored, `changed=true`
  - Singleflight: 5 concurrent fetches for same URL → 1 round-trip
- Drop `playerCacheTTL` from `14 * 24 * time.Hour` to `24 * time.Hour`
  as backstop (TTL only matters when offline; conditional GET handles
  the steady-state case)

### Stage 2 — Reactive invalidation in sidecarSolver (~60 lines)

Files:
- `internal/cipher/errors.go` — new `ErrPlayerJSStale` sentinel
- `internal/cipher/solver_sidecar.go` — modify `solve` to recognize stale-JS error patterns, evict caches, retry once
- `internal/cipher/types.go` — extend `PlayerSource` interface with
  `RemovePlayerJS(playerID string) error`
- `internal/cipher/solver.go` — `GojaResolver.RemovePlayerJS` implementation
  (reuse existing `playerCache.Remove` + `solverData` eviction)
- `internal/cipher/solver_sidecar_test.go` — two new tests:
  - sidecar returns `"ejs solve sig: no solutions"` → cache wiped, retry
    with fresh JS → success
  - sidecar returns same error twice (after fresh JS) → permanent
    `ErrPlayerJSStale`

### Stage 3 — Defer per-format cipher decryption (~150 lines)

Files:
- `internal/youtube/types.go` — `Format` struct gains `EncryptedSig`,
  `SigKey` fields (both omitempty)
- `internal/youtube/player_api_parsing.go` — `parseFormatsWithCipher`
  → `parseFormats`. Remove sig + n decrypt loops. Store sigCipher URL
  component in `Format.URL` and the `s`/`sp` parts in `EncryptedSig`/
  `SigKey`. The "drop on n-fail" filter goes; the
  `cipherNeededButMissing` warn path is dropped.
- `internal/cipher/decrypt.go` — new `ResolveFormatURL` and
  `ResolveFormatRequest`
- `internal/worker/strategy_youtube_dash.go` — call `ResolveFormatURL`
  on the chosen video + audio streams before constructing
  SegmentDownloader
- `internal/worker/strategy_youtube_hls.go` — call `ResolveFormatURL`
  on the master URL only (per-segment URLs come from playlist parsing)
- `internal/worker/strategy_youtube_manifestless_dash.go` — call
  `ResolveFormatURL` on the chosen video + audio adaptive format URLs
  (currently does nothing because parse pre-decrypted; after this
  change, parse leaves URLs raw and strategy resolves them)
- `internal/worker/strategy_youtube_vod.go` — `DownloadVod` resolves
  the chosen format's URL before passing to engine
- Tests:
  - `parseFormats` collects all formats including sigCipher-only,
    populates EncryptedSig + SigKey correctly
  - `ResolveFormatURL` with sigCipher entry produces URL with
    `&signature=DECRYPTED` appended
  - `ResolveFormatURL` with already-resolved URL just runs n-decrypt
  - Strategy build path correctly resolves chosen format(s) only;
    other formats stay raw

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
- **Singleflight key collision.** `playerURL` is the key — fine because
  `playerURLForID` constructs deterministic URLs. If we ever pass two
  different URLs that resolve to the same player, we'd want to coalesce
  on `CacheKey(playerURL)` instead. Not currently a concern.
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

## 10. Open questions to ask if any ambiguity arises

1. **Should the `playerCacheTTL` drop to 24h** as part of Stage 1, or
   leave at 14d and rely on conditional GET? Recommend 24h (defense in
   depth) but it's optional.
2. **Should `ResolveFormatURL` be a method on `cipher.Solver` or a free
   function?** Free function (`cipher.ResolveFormatURL`) is consistent
   with `RoutedDecryptNInURL` / `RoutedResolveURL`. Recommend free function.
3. **Do we need to retain the `cipherNeededButMissing` warning** that
   fires when no cipher is wired but ciphered formats are present? Today
   it fires on goja-disabled startups. Probably keep but move into the
   strategy phase (warn at format-resolution time, not parse).

If anything else unclear during implementation, the user has been very
willing to clarify; ask via AskUserQuestion before guessing.

---

End of plan.
