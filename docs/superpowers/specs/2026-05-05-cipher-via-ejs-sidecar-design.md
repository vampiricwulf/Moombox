# Cipher solving via ejs in the BotGuard sidecar

**Status:** Draft — pending implementation plan
**Date:** 2026-05-05
**Author:** Moombox / collaborative session
**Related:** `internal/cipher/`, `internal/bgutils/sidecar/`, `bgutil-sidecar/`, `references/ejs/`

## 1. Problem

YouTube's player JS for player ID `cb017549` (and the family of recent players that share its obfuscation pattern) has restructured the signature ("sig") cipher algorithm in a way that defeats Moombox's existing extraction strategies. Concrete log signal:

```
WARN cipher: no sig strategy succeeded (ALR chains + old candidates)
     playerID=cb017549 size=2460644 urlClassName=g.f7
     nArrayCandidates=3 sigOldCandidates=0 alrSigChains=0
INFO cipher: solver ready playerID=cb017549 hasSig=false hasN=true
```

Both existing strategies (`extractor_legacy.go`'s old-candidate regex and `extractor_full_player.go`'s ALR-chain extraction) come up empty. Investigation of the cb017549 player confirms a structural change, not a name rotation:

- Classic helper-object sig (`function(a){a.reverse()}`, `a.splice(0,b)`, `var c=a[0];a[0]=a[b%a.length]` swap) is absent.
- The `set("alr","yes")` ALR marker still appears, but the transform chain Moombox expects after it is gone — replaced by numeric-opcode-dispatched calls (`St(6,1106,Da(2,2438,h))`) reminiscent of BotGuard's interpreter pattern.
- The `"sig"` URL-parameter literal is no longer present anywhere in the player; only `"signature"` appears, and only as an internal state key.

The user reports the algorithm is "constantly being reobfuscated within the code" — the sig logic no longer has a static shape that can be matched by AST or regex patterns alone.

**Practical impact today:** Live DASH/HLS downloads succeed (n-param decryption still works in Moombox's goja path), but format URLs that carry an encrypted signature parameter fail downstream auth. The breakage is silent — `solver ready hasSig=false` produces no errors at solve time.

## 2. Goals and non-goals

### Goals

- Restore sig solving on cb017549 and the family of similarly-obfuscated current/future players.
- Future-proof against further sig algorithm changes by treating the algorithm as opaque — never extracting or reimplementing it in our codebase.
- Maintain n-param decryption reliability (it works today via goja; we keep that working *and* add a more durable path).
- Preserve the property that the BotGuard sidecar is optional for downloads — graceful degradation when the sidecar is unavailable.

### Non-goals

- Replacing or rewriting BotGuard. This design adds a sibling responsibility to the existing sidecar, not a replacement.
- Static reimplementation of the sig algorithm in Go or goja. The user has confirmed there is no static shape worth chasing; further investment in pure-Go extraction is wasted work.
- Building a generic JS player evaluator from scratch. We use ejs (`references/ejs/`), the yt-dlp project's own external JS solver, which already implements the URL-passthrough approach actively maintained against new player variants.
- Replacing the existing `internal/cipher/extractor_*.go` extractors. They stay as fallback for n; the sig path through them is dead but its removal is out of scope.

## 3. Architecture

```
Moombox (Go)                      Sidecar (Node + V8)
─────────────                     ────────────────────
cipher.Solver (interface)         vendored ejs bundle
  ├─ goja-based (existing)        (ejs.bundle.js, ~200 KB)
  │    └─ n only (sig dead)         ├─ meriyah (AST parser)
  └─ sidecar-based (new)            ├─ astring (AST generator)
       │                            └─ ejs.main()
       │  JSON-RPC: solveCipher
       │   { playerID,           per-player cache (LRU, N=3):
       ├──► playerJS?,             playerID → preprocessed player JS
       │    sigChallenges,
       │    nChallenges }        per-challenge cache (LRU, ~1k):
       │                           (playerID, type, val) → solved
       │  { sigResults,
       └◄── nResults }
```

The cipher package gains a `cipher.Solver` interface and a sidecar-backed implementation. The existing goja solver stays as a fallback (n only). At call time:

- **sig:** sidecar only. If sidecar errors or unavailable, sig is unavailable (same observable state as today).
- **n:** sidecar primary, goja fallback. n stays available even when the sidecar is down.

Player-JS fetching stays where it is today (`internal/cipher/`); Moombox passes the source bytes to the sidecar on first use of a given player ID, then references the player by ID for subsequent calls.

## 4. Wire protocol

A new JSON-RPC method on the existing sidecar protocol. The protocol's framing, error shape, and ID-multiplexing work unchanged — this is one more method dispatched alongside `ping`, `generatePoToken`, etc.

| Method | Params | Result | Errors |
|---|---|---|---|
| `solveCipher` | `{ playerID: string, playerJS?: string, sigChallenges: string[], nChallenges: string[] }` | `{ sigResults: Record<string,string>, nResults: Record<string,string> }` | `{ error: string }` for "player not loaded" (Moombox retries with `playerJS`), "preprocess failed" (player AST patterns didn't match — likely a new variant ejs hasn't yet handled), "solve failed" (per-challenge issue) |

**`playerJS` is optional** so that warm calls don't re-send 2.7 MB of source. Moombox sends it only on the first call for a given `playerID`, or after a sidecar restart (detected via the "player not loaded" error and one retry).

Result records are keyed by the input challenge string for stable mapping. Empty arrays in `sigChallenges`/`nChallenges` are valid and produce empty result records (callers may want only one of the two cipher types in a given call).

## 5. Sidecar implementation

### 5.1 Vendoring ejs

ejs is licensed Unlicense (public domain) — no compatibility concerns. Vendor the source, do not git-submodule it; updates are deliberate.

**Files vendored** (under `bgutil-sidecar/vendor/ejs/`):

```
src/yt/solver/solvers.ts    — preprocessPlayer, getFromPrepared, getSolutions
src/yt/solver/nsig.ts       — extract (URL builder AST matcher)
src/yt/solver/setup.ts      — minimal global stubs
src/yt/solver/main.ts       — public entry, Input/Output types
src/types.ts                — DeepPartial helper
src/utils.ts                — matchesStructure, generateArrowFunction, isOneOf
LICENSE                     — Unlicense, preserved
VERSION                     — git SHA we vendored from (for traceability)
```

`extract.ts` (CLI helper) and `dynamic.lib.ts` (Deno/Bun runtime imports) are not vendored — they aren't part of the runtime path.

Two npm dependencies are pulled into `bgutil-sidecar/package.json` matching ejs's pins exactly: `meriyah` 6.1.4 and `astring` 1.9.0. Lockstep with ejs avoids the situation where we vendor ejs source against one parser version and resolve a different one transitively.

### 5.2 Build pipeline

Extend `bgutil-sidecar/build.mjs` with an esbuild step that bundles vendor/ejs + its two npm deps into a single ESM file: `bgutil-sidecar/vendor/ejs.bundle.js`. The bundle is gitignored (regenerated on `node build.mjs`). The tarball that goes into `internal/bgutils/embed/sidecar.tar.gz` includes the bundle. Sidecar startup imports the bundle dynamically.

esbuild config (sketch):

```js
await esbuild.build({
    entryPoints: ["vendor/ejs/src/yt/solver/main.ts"],
    bundle: true,
    format: "esm",
    platform: "neutral",
    target: "node20",
    outfile: "vendor/ejs.bundle.js",
    minify: true,
});
```

Existing tarball-build step packages `vendor/ejs.bundle.js` alongside `src/server.js`. Tarball size grows by ~200 KB.

### 5.3 server.js dispatch

Add a `solveCipher` case to the existing JSON-RPC dispatch (`server.js:dispatch`). The handler:

1. If `playerJS` provided OR `playerID` not in cache: preprocess via `ejs.preprocessPlayer(playerJS)`, cache the result keyed by `playerID`. Use `getFromPrepared` once to instantiate `{n, sig}` solver closures, cache those alongside.
2. If `playerJS` not provided AND `playerID` not cached: return `error: "player not loaded"`.
3. For each entry in `sigChallenges`: check the per-challenge cache; if miss, call `sigSolver(challenge)`, cache, return.
4. Same for `nChallenges` with `nSolver`.
5. Return `{sigResults, nResults}`.

Per-player LRU eviction: max 3 players cached. Per-challenge LRU eviction: max ~1000 entries per (playerID, type), flushed on player eviction.

The cipher path runs in a separate Node `vm` context from the BotGuard JSDOM context. ejs's `setup.ts` provides minimal globals (`location`, `document`, `navigator`, `XMLHttpRequest` stub) — no JSDOM needed. This isolates cipher work from BotGuard work and avoids the JSDOM-29 cold-start cost on every cipher call (which would otherwise compound the issue we just fixed in the ready-handshake change).

### 5.4 Memory budget

Per loaded player: ~30 MB (preprocessed JS string ~3-5 MB plus V8 overhead for compiled solver closures). 3-player cache: ~90 MB additional sidecar memory. Stays well within sane limits given current sidecar baseline (~50-80 MB).

## 6. Moombox integration

### 6.1 Cipher solver interface

`internal/cipher/types.go` gains a `Solver` interface:

```go
type Solver interface {
    SolveSig(ctx context.Context, playerID, encryptedSig string) (string, error)
    SolveN(ctx context.Context, playerID, encryptedN string) (string, error)
    // SolveBatch optimizes the common case where a single URL has both a
    // sig and an n parameter, by batching them into one sidecar round-trip.
    SolveBatch(ctx context.Context, playerID string, sigs, ns []string) (sigResults, nResults map[string]string, err error)
}
```

The existing goja-based solver is retrofitted to satisfy this interface (returning `ErrSigUnavailable` on `SolveSig` for new players where extraction fails).

### 6.2 Sidecar solver

`internal/cipher/solver_sidecar.go` implements `Solver` by issuing `solveCipher` JSON-RPC calls to the existing `*sidecar.Sidecar` instance. It needs:

- A reference to the sidecar (passed in from `cmd/moombox/services.go` after the sidecar starts).
- Access to the player JS source through Moombox's existing `internal/cipher/player_cache.go` — that cache already fetches and caches per-`playerID` source bytes for the goja extractors. The sidecar solver reads from the same cache and forwards bytes to the sidecar on first use.
- A small in-memory cache of `(playerID, type, value) → solved` for repeated lookups within a process — critical because segment URLs in a manifest may share an encrypted n value.

Tracks per-`playerID` whether `playerJS` has already been sent to the sidecar in the current sidecar lifetime. On first send OR after a `"player not loaded"` error (sidecar restart between calls), sends `playerJS`; otherwise sends just `playerID`.

### 6.3 Composite solver

`internal/cipher/solver_composite.go` (new) wraps both solvers and implements the policy:

```go
func (c *compositeSolver) SolveSig(...) (string, error) {
    // Sidecar only — goja sig is dead on new players.
    return c.sidecar.SolveSig(...)
}

func (c *compositeSolver) SolveN(...) (string, error) {
    if s, err := c.sidecar.SolveN(...); err == nil { return s, nil }
    return c.goja.SolveN(...)
}
```

This is the type wired into `ytService.PlayerAPI` in place of the current `cipher.NewSolver()` return.

### 6.4 Wiring

`cmd/moombox/services.go` step 7b currently constructs the sidecar and step 8 currently constructs the cipher solver. Reorder so the sidecar is constructed first and passed into the cipher constructor; if the sidecar is disabled or failed to start, fall back to a goja-only solver (downloads continue working modulo sig).

## 7. Failure modes and fallback

| Condition | Behavior |
|---|---|
| Sidecar healthy, ejs preprocessing succeeds | sig + n solved via sidecar; goja idle |
| Sidecar healthy, ejs preprocessing fails (new player variant ejs doesn't know yet) | sig fails; n falls back to goja (works today on most players) |
| Sidecar unhealthy/down | sig fails; n falls back to goja |
| Sidecar disabled in config (`use_sidecar=false`) | composite solver constructed in goja-only mode; same as "sidecar down" |
| Player JS fetch fails | both sig and n unavailable for that player; orthogonal to this design |

The "sig fails" path is observable through the same `solver ready hasSig=false` log line we have today. Downloads that don't need sig keep working; downloads that do need sig get a clean error rather than corrupted output.

## 8. Testing strategy

**Unit tests** — `internal/cipher/solver_sidecar_test.go`:

- Mock sidecar interface so tests don't require the real subprocess.
- Verify the "send playerJS on first call, playerID-only on subsequent" handshake.
- Verify the "player not loaded → retry with playerJS" recovery.
- Verify SolveBatch produces consistent results with separate SolveSig/SolveN calls.

**Integration tests** — `internal/cipher/solver_sidecar_live_test.go`, gated behind `MOOMBOX_LIVE_CIPHER_TEST=1`:

- Start a real sidecar.
- Feed it `testdata/player_cb017549.js` (new fixture, copied from the live download we already have) plus the existing `testdata/player_74edf1a3.js` and `player_latest.js`.
- Verify sig + n solving against known-good challenge/result pairs (collected from the spike).
- Run on every player fixture to catch regressions in ejs's AST patterns when we update the vendored copy.

**Sidecar tests** — `internal/bgutils/sidecar/cipher_test.go`:

- Round-trip JSON-RPC correctness for `solveCipher`.
- Cache eviction at player and challenge level.
- Concurrent solveCipher calls multiplex correctly (existing pattern from sidecar_test.go).

**Spike confirmation already done** — during the brainstorming pass we ran `ejs.main()` against the live cb017549 player JS and confirmed both sig and n produce non-trivial transformations (sig reverses + swaps the input alphabet; n decodes "abcdefghij_12345" → "DdIQSAqCz-DGHq"). This was an ad-hoc TypeScript spike under `references/ejs/`; it is not preserved as a permanent artifact since `references/` is gitignored. The integration tests above subsume what the spike proved.

## 9. Risks and open questions

**Risk: ejs vendor drift.** ejs gets updates for new player variants (recent commits include "Solve new player variants"). If we vendor at one SHA and don't update, we'll regress when YouTube ships another variant. **Mitigation:** track the vendored SHA in `bgutil-sidecar/vendor/ejs/VERSION`; document the update procedure in `bgutil-sidecar/README.md`; consider adding a CI job that periodically checks for ejs updates and opens a PR. Update procedure is mechanical (re-vendor, rebuild bundle, run tests against fixture players).

**Risk: cb017549 is one player; ejs may not match other live players we encounter.** The spike confirmed cb017549 works. Other player IDs in flight (we already have `74edf1a3` and `latest` fixtures) should be verified against ejs as part of integration testing.

**Risk: meriyah parser breaks on a future player.** ejs's AST patterns assume meriyah parses the player. Meriyah is actively maintained but isn't infallible — a syntactic change could break parse before extraction even starts. **Mitigation:** the failure mode is `solveCipher` returning an error, which falls back to goja for n; sig still fails as today. No worse than current state.

**Risk: increased sidecar coupling.** Today the sidecar is a single failure domain for BotGuard alone; this adds cipher to that domain. We just fixed a 21-hour outage from a jsdom 29 cold-start regression. **Mitigation:** the sidecar fix in this same change-set (ready-event handshake, replacing the 5s ping deadline) materially improves sidecar startup reliability. Goja-n fallback means n stays available even when the sidecar is unhealthy.

**Open question: do we eventually retire the goja-based cipher extractors?** Not in this spec. They stay as the n-fallback. Future work could remove them once the sidecar has proven reliable; not now.

**Open question: do we expose ejs's `output_preprocessed` flag externally?** No — Moombox doesn't need preprocessed JS round-tripped to its side. Internal-only optimization in the sidecar.

## 10. Rollout plan

1. **Phase 1 — vendor + bundle.** Vendor ejs into `bgutil-sidecar/vendor/`, add esbuild step to `build.mjs`, confirm `node build.mjs` produces the bundle and the existing sidecar still starts.
2. **Phase 2 — sidecar dispatch.** Add the `solveCipher` JSON-RPC handler to `server.js`. Add cipher tests (unit + sidecar integration). Verify against `player_cb017549.js` fixture.
3. **Phase 3 — Moombox integration.** Add `cipher.Solver` interface, `solver_sidecar.go`, `solver_composite.go`. Wire into `services.go`. Update goja-side to satisfy the new interface and return `ErrSigUnavailable` for sig.
4. **Phase 4 — fixtures and tests.** Save `player_cb017549.js` to `internal/cipher/testdata/`. Add solver-sidecar live tests with known challenge pairs.
5. **Phase 5 — observability.** Update the existing `cipher: solver ready hasSig=...` log line to reflect that sig comes from the sidecar; add a `/api/cipher/stats` endpoint or extend `/api/pot` with cipher counters mirroring the BotGuard sidecar's existing counters (preprocessed players cached, challenges solved, fallbacks taken).

Each phase is independently mergeable. Phase 1 + 2 together are testable in the sidecar without touching Moombox; phase 3 onward is the user-visible change.
