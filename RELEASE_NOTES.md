## Features

- DASH/HLS/VOD download path now routes sig + n cipher work through the BotGuard sidecar's V8 engine instead of the in-process goja extractor. v2.6.15 plumbed the routed `cipher.Solver` into `PlayerAPI` (the format-parsing path) but the actual download strategies still used the goja resolver directly — so sig stayed broken for live downloads on the cb017549-family of YouTube players. New `RoutedResolveURL` / `RoutedDecryptNInURL` helpers in `internal/cipher/decrypt.go` close the gap.
- Mixed-batch (sig + n in one call) cipher operations now issue a single sidecar JSON-RPC instead of two. Documented trade-off: a transient sidecar failure on a mixed batch loses n entries goja could have served independently — callers that prioritise resilience over round-trips should call `Sig` and `N` separately.

## Improvements

- Cipher solver cache key now hashes the playerID instead of the full player URL. The previous URL-hash split the cache when the routed path's synthetic `en_US` URL and the legacy path's watch-page-supplied URL had different locales, causing two ~44 MB compiled goja Runtimes to live in memory for the same player.
- Sidecar per-player cache cap dropped from 3 to 2 (~30 MB sidecar memory savings; YouTube rotates players slowly enough that 2 is enough headroom for a transition window).
- `WaitForJobExit` switched from 100ms polling to a signal-driven channel close. Up to 50 syscalls per delete → 1 channel select.
- Sidecar stderr filtering replaced its substring-match for the JSDOM canvas warning with structured severity prefixes from the JS side (`[bgutil-sidecar:error]`, `[bgutil-sidecar:warn]`). Real server.js errors now surface at Warn; harmless JSDOM chatter stays silent; unknown unprefixed lines surface at Debug for investigation.
- Goja-side `cipher: solver ready` logs were already demoted to Debug in v2.6.15. The new `[Cipher] sig decrypted via sidecar` log is now per-(playerID, route) deduplicated — operators see the route signal the first time it succeeds for a given player, not 20-50 times per video probe.
- Goja-vs-EJS parity test now iterates `testdata/player_*.js` so any future EJS-vs-goja drift on any tested player surfaces (one subtest per fixture; sidecar startup amortised across all of them).

## Bug Fixes

- `RoutedResolveURL`'s routed-sig-fail-fallback branch now nil-guards the goja resolver before dereferencing. Production wiring always passes both non-nil so it never panicked in practice, but the function's own contract advertised nil-routedSolver support without the matching nil-goja defence.
- `sidecarSolver` retry on double `ErrPlayerNotLoaded` now wraps the second occurrence as a permanent failure (not the same sentinel), so callers can distinguish "transient cache miss → retry-with-JS" from "sidecar permanently rejected the JS." Previous behaviour caused silent total sig failure with confusing log trail when the second attempt also failed.
- `compositeSolver.Batch` and `sidecarSolver.Batch` now validate that every requested challenge has a result on success. Defends against future partial-response bugs that would otherwise slip empty-string "decrypted" values into URL parameters and 403 silently at the CDN.
- `database.UpdateJobFields` ErrNoRows demote (introduced in v2.6.15) now distinguishes real DB errors from delete-during-mux. Only `sql.ErrNoRows` drops to Debug; corruption / schema drift / transient I/O still log at Error.
- Worker orchestrator's row-vanish detection moved from per-call-site `updateOrExit` to a centralised `OnJobDeleted` subscriber. UpdateJobFields now fires `notifyJobDeleted` on its own ErrNoRows path so all 20+ UpdateJobFields call sites benefit, not just the 5 wrapped in v2.6.15. Subscription scope expanded to `processJob` so deletes during pre-execute phases (streamProc, AcquireDownloadSlot) also trigger the cancel.
- `loggedSigRoutes` map is cleared on cipher invalidation via `PlayerAPI.ClearLoggedRoutes(playerID)`, so the "sig recovered after rotation" Info log fires once per recovery cycle, not once per process lifetime.
- `PlayerIDFromURL` fallback to URL-hash now warns (once per URL via slog) so operators see the regression early. Without this signal, a future YouTube URL-shape change would silently route to a bogus playerID with no diagnostic.

## Internal

- `cipher.Solver` plumbed through `DownloadWorkerDeps` → `Orchestrator` → `StrategyDeps` alongside the existing `*GojaResolver` (kept for `GetSts` and goja-internal cache management). Three direct `DownloadDash` callers in `orchestrator_youtube.go` updated to pass both.
- `extractPlayerIDFromURL` helper extracted from `PlayerIDFromURL` so `CacheKey` and `PlayerIDFromURL` share the parser without recursing.
- `JobQueue` gains a per-job `done chan struct{}` map closed when `processJob` returns. `JobQueue.Done(jobID)` returns a pre-closed channel for unknown IDs so callers don't need to pre-check `IsProcessing`.
- New helpers in `internal/cipher/decrypt.go`: `RoutedResolveURL` (manifest URL sig+n decryption with routed/goja fallback) and `RoutedDecryptNInURL` (per-stream n-param decryption).
- New tests:
  - `decrypt_test.go` — six cases covering nil-routed / nil-goja / sig-fail-with-no-fallback / no-sig / no-n / both-nil branches.
  - `solver_composite_test.go` — gained `TestCompositeBatch_*` covering sig-only, n-only, mixed-healthy, mixed-sidecar-fails-entire-batch, and `TestCompositeBatch_MixedSinglesidecarRoundTrip` (counts sidecar Batch invocations).
  - `solver_sidecar_test.go` — `TestSidecarSolverDoubleNotLoadedReturnsWrappedError` and `TestSidecarSolverBatchPartialResponse`.
  - `solver_sidecar_live_test.go` — `TestSidecarSolverGojaParity` iterates `testdata/player_*.js` (replaces the single-fixture `TestSidecarSolverGojaParity74edf1a3`).
  - `worker_test.go` — `TestNewDownloadWorker_AcceptsRoutedCipherSolver` regression-locks the new cipher.Solver field.
- `compositeSolver.Batch` documents the round-trip-vs-resilience trade-off explicitly and notes that the win applies per-Batch-invocation, not per workload (per-element loops in the worker still pay one round-trip each — future batching opportunity).
- Spec and plan docs at `docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md` and the matching plan gained post-implementation appendices recording the deliberate deviations from the original design.
