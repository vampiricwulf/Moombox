## Features

- Cipher solving (sig + n) now routes through the BotGuard sidecar's V8 engine via the public-domain [yt-dlp/ejs](https://github.com/yt-dlp/ejs) library. Restores sig on the cb017549-family of YouTube players where the in-process goja extractor's static patterns no longer match. Goja stays as the n fallback so downloads keep working when the sidecar is down.
- New `cipher.Solver` interface with composite routing policy: sig is sidecar-only, n prefers sidecar with goja fallback. PlayerAPI migrated to use the routed solver while keeping the goja resolver for signature-timestamp lookup.
- New `[Cipher] sig decrypted via sidecar` and `[Cipher] sig decrypted via goja fallback` log lines surface the actual route taken whenever sig is solved.

## Improvements

- Sidecar startup handshake replaced 5-second ping/pong with a `{"event":"ready"}` notification on stdout. The previous ping had a 5s deadline that started racing jsdom cold-start after the v2.6.14 jsdom 27→29 bump, leaving every PO-token request to fall through to a broken goja path. The new handshake is bounded by a 60s backstop for genuinely wedged sidecars rather than a metronome.
- Deleting an active stream (TUI or web) now auto-cancels the worker, waits up to 5 seconds for graceful drain, then removes the row. Single user action, no more orphaned worker goroutines spamming `UpdateJobFields: failed to read back job` errors.
- Web UI receives a discrete `job_deleted` WebSocket event on delete so rows disappear immediately without waiting for a full-list rebroadcast (previously raced against the preceding `Cancelled` status update and left stale state until manual refresh).
- Sidecar extraction cache stamp now includes a SHA256 of the embedded tarball alongside the Node binary version. Any change to vendored sources (server.js, cipher.js, vendor/ejs.bundle.js, package-lock.json) automatically invalidates the cache and forces a fresh extraction on next launch.
- Goja-side cipher extractor logs (`solver ready`, `no sig strategy succeeded`, `extraction branch`) demoted from Info/Warn to Debug. They reflect internal extraction state, not user-facing failures, and were misleading once the sidecar became the primary sig path.
- JSDOM's canvas-not-implemented stderr is filtered from the log; it fires on every BotGuard run because YouTube probes canvas for fingerprinting and JSDOM ships without a canvas binding by design.
- Per-call `n decrypted via sidecar` debug log dropped to reduce per-segment noise. The DASH batch summary `[Cipher] DASH n-param decryption complete decrypted=N total=N` covers the bulk of n work.

## Bug Fixes

- Sidecar worker defensively exits its goroutine when `UpdateJobFields` returns nil (the row was deleted out from under it). Database read-back-failure log demoted from Error to Debug — it's no longer an exceptional condition once the defensive exit runs.
- Sidecar build prunes devDependencies before tarring and restores them after. The previous build path inadvertently shipped esbuild (~12 MB devDep) inside the runtime tarball, doubling its size.
- Sidecar build switched from in-process esbuild API to subprocess invocation so Windows can release the esbuild.exe file lock in time for the prune step.
- `solveCipher` JSON-RPC dispatch validates that `sigChallenges` and `nChallenges` arrive as arrays; non-array inputs error cleanly instead of producing per-character garbage.
- ejs response shape is guarded before indexing `result.responses[0]` so an unexpected shape produces a meaningful error rather than an opaque `TypeError`.
- Sidecar's per-challenge cache eviction labelled FIFO instead of LRU (the original "Cheap LRU" comment was inaccurate — `Map.get` doesn't reorder iteration; behaviour was always insertion-order FIFO).
- Sidecar `ErrPlayerNotLoaded` detection switched from exact-string equality to `strings.HasSuffix` against a named constant; the prior comparison would silently break if either the sidecar's error body or `call()`'s `"sidecar: "` prefix changed.
- `cmd/moombox/services.go`, `internal/cookies/autocookies.go`, and `internal/cookies/dpapi/dpapi_windows.go` no longer return capitalised error strings (Go style ST1005).
- `connectivity.TestMonitor_StartIsIdempotent` had a dead `&firstCancel == nil` check that was always false. Removed; the non-nil assertion above is the strongest invariant Go function values support.

## Internal

- Vendored yt-dlp/ejs source under `bgutil-sidecar/vendor/ejs/` (Unlicense, public domain) pinned to commit `2231f1f`. Bundled via esbuild to a single ~159 KB ESM file at build time.
- New `meriyah` 6.1.4 and `astring` 1.9.0 npm dependencies (exact pins matching ejs's own pins to avoid AST API drift).
- `*GojaResolver` (renamed from `Solver`) now satisfies the new `cipher.Solver` interface via Sig/N/Batch wrappers. `playerURLForID` helper bridges interface playerIDs to the goja resolver's URL-keyed cache.
- `internal/cipher/solver_sidecar.go` and `solver_composite.go` added with full unit test coverage (mock sidecar; routing policy assertions). `solver_sidecar_live_test.go` validated locally on the cb017549 fixture via the gated
  TestSidecarSolverLive (MOOMBOX_LIVE_CIPHER_TEST=1; not in CI by default).
- `TestSidecarSolverGojaParity74edf1a3` proves byte-for-byte equality between the goja and EJS-via-sidecar outputs on a player goja can statically handle, validating that EJS isn't approximating the sig/n algorithm — it's reproducing the player's authoritative output exactly.
- `cb017549.js` added as a testdata fixture (gitignored per project convention; tests `t.Skip` when missing).
- Removed unused helpers flagged by staticcheck: `SegmentDownloader.recordTransientErr` / `emitProgress`, `filterUniqueDescriptionLines`, and a pair of unused test helpers.
- Project-wide gofmt pass dropped stray blank lines and re-aligned import blocks.
- `.gitattributes` now codifies LF for Go/JS/JSON/TOML/YAML/MD/sh source so Windows checkouts no longer produce CRLF/LF normalisation warnings on every commit.
- `docs/superpowers/specs/2026-05-05-cipher-via-ejs-sidecar-design.md` and the matching implementation plan capture the full design rationale.
