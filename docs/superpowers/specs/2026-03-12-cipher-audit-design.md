# Cipher Solving Audit — Design Spec

**Date:** 2026-03-12
**Scope:** Full-stack surgical hardening of `internal/cipher/` to resist YouTube player changes.

## Context

Commit `5a96980` revealed that hardcoded assumptions in cipher solving (URL class name `g.fb` → `g.sB`) caused DASH n-param extraction to fail, producing infinite 403 retry loops. An audit found additional fragility points and one active bug.

## Critical Bug: ALR Sig Transform False Match

`findAlrTransformChain` uses `strings.Index` to find the FIRST `set("alr","yes");` occurrence, then `FindStringSubmatchIndex` across the entire remaining file to find `(\w+)&&\((\w+)=`. In player `74edf1a3`:

- **4 ALR occurrences** at bytes 697449, 921215, 989089, 1005037
- Only ALR #4 (byte 1005037, function `n8`) is the actual sig function
- The code finds ALR #1, then matches `N&&(N=Z.U` **5,413 chars away** in unrelated code
- The `param == assignParam` guard passes (`N == N`) but doesn't prevent the false match
- Extracts `Z.U` (len=3) as the transform chain — garbage that throws ReferenceError at runtime
- The existing test only checks `solvers.Sig != nil`, never validates the output
- `_multiTry` wraps the garbage function; it exists (non-nil) but always throws when called

**Correct transform at ALR #4:** `JI(49,1919,f1(33,6560,sig))` — found within 14 chars of the ALR marker.

## Changes

### 1. ALR Transform Chain — Proximity-Bounded Iteration (Bug Fix)

**File:** `extractor_full_player.go`

Change `findAlrTransformChain` to `findAlrTransformChains`:
1. Find ALL `set("alr","yes")` occurrences (also check single-quote variant `set('alr','yes')`)
2. For each occurrence, search for the `(\w+)&&\((\w+)=` pattern within 200 chars (measured across player versions; the correct match is typically within 15 chars of the marker)
3. Extract the transform chain from each valid match
4. Return ALL valid chains as a slice (not just one string)
5. All chains become sig generator candidates passed to `_multiTry`

**Signature change:** `findAlrTransformChain(playerJS string) string` → `findAlrTransformChains(playerJS string) []string`

### 2. URL Class Detection — Widened Namespace

**File:** `extractor_full_player.go`

Replace single `urlClassPattern` with ordered pattern list:
1. Dotted identifier: `new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*!0\)\)\.get\("n"\)` — matches `g.sB`, `h.Foo`, `_yt.X`
2. Dotted with `true`: `new\s+([a-zA-Z_$][\w$]*\.[a-zA-Z_$][\w$]*)\([^,]+,\s*true\)\)\.get\("n"\)`
3. Bare identifier: `new\s+([a-zA-Z_$][\w$]*)\([^,]+,\s*(?:!0|true)\)\)\.get\("n"\)`

`findURLClassName` tries each in order, returns first match.

### 3. IIFE Closing Detection — EOF Proximity Check

**File:** `extractor_full_player.go`

After finding `strings.LastIndex(playerJS, "})(")`, verify the match is within the last 200 chars of the file (allows for trailing whitespace, comments, or source maps). If not, try `}).call(`. If neither is near EOF, return error.

### 4. `var window=this;` Stripping — Regex

**File:** `extractor_full_player.go`

Replace `strings.Replace(modified, "var window=this;", "", 1)` with regex `var\s+window\s*=\s*this\s*;` to handle whitespace variations.

### 5. Sig Candidate Validation via _multiTry

**File:** `extractor_full_player.go`

Currently n-param candidates are validated at JS level (nBindingTemplate tests with dummy value). Sig candidates are not validated — `_multiTry` wraps them without testing.

Change: Update `_multiTry` to also check `_r !== _input`, catching identity-function candidates. This mirrors the n-param validation pattern.

With the ALR proximity fix (Change 1), the false `Z.U` transform wouldn't be found in the first place. The `_multiTry` identity check is defense-in-depth: even if a garbage candidate passes extraction, it would be skipped at call time if it returns the input unchanged.

### 6. Cache Invalidation on Runtime Failure

**Files:** `solver.go`, `player_cache.go`

Add `InvalidateSolver(playerURL string)` to `Solver`:
- Acquire `solverMu` for thread safety (must not deadlock with in-progress compilations via `compileMu`)
- Evict from in-memory solver LRU cache (`solverData` map + `solverOrder` list)
- Call `PlayerCache.Remove(playerURL)` to delete the cached .js file

Add `Remove(playerURL string)` to `PlayerCache` (distinct from existing bulk `Evict()` method):
- Delete the specific cached file by `CacheKey(playerURL)`
- Handle `os.Remove` errors gracefully (log, don't fail) — on Windows, file may be held by another reader

**Wiring:** The 403 detection for cipher failure happens in the engine package (`downloader_dash.go`'s `handleGoneError`), not the orchestrator. The orchestrator interprets download results. Wire invalidation via a callback: add `OnCipherFailure func()` to `SegmentDownloader` options, called by `handleGoneError` on 403. The orchestrator sets this callback to call `InvalidateSolver(playerURL)` when constructing the downloader in `strategy_youtube_dash.go`.

**Files additionally modified:** `internal/engine/downloader_dash.go` (callback field + invocation), `internal/worker/strategy_youtube_dash.go` (wire callback).

### 7. Diagnostic Logging

**File:** `solver.go` (in `compileSolver`, which already logs — consistent with current pattern)

Extraction functions (`preprocessPlayerFull`, `findAlrTransformChains`, etc.) remain pure. Logging stays in the calling `compileSolver` method, which already logs candidates at lines 109-114.

Extend `compileSolver` logging at Debug level:
- URL class name detected or not found
- Number of ALR chains found, n-array candidates, sig candidates
- Which extraction approach succeeded (full player vs legacy fallback)

Log at Warn level when ALL strategies for a component fail:
- `"cipher: no n-param strategy succeeded (URL class + array candidates)"`
- `"cipher: no sig strategy succeeded (ALR chains + old candidates)"`

### 8. Test Improvements

**Files:** `extractor_test.go`, `solver_test.go`

In `extractor_test.go`:
- `TestPreprocessPlayer74edf1a3`: Add actual sig solver invocation and validate output differs from input
- `TestFindAlrTransformChains`: Test with multiple ALR markers, verify only proximity matches are returned
- `TestFindURLClassName`: Test with widened patterns (dotted, bare)
- End-to-end: Verify player_74edf1a3.js produces working N AND Sig solvers

In `solver_test.go`:
- `TestSolverInvalidation`: Test that `InvalidateSolver` evicts from both in-memory and disk caches

## Files Modified

| File | Changes |
|------|---------|
| `internal/cipher/extractor_full_player.go` | ALR iteration, URL class patterns, IIFE check, window regex, sig validation |
| `internal/cipher/solver.go` | `InvalidateSolver` method, extended diagnostic logging |
| `internal/cipher/player_cache.go` | `Remove(playerURL)` single-key eviction method |
| `internal/cipher/extractor_test.go` | Enhanced extraction tests |
| `internal/cipher/solver_test.go` | `TestSolverInvalidation` |
| `internal/engine/downloader_dash.go` | `OnCipherFailure` callback field, invocation on 403 |
| `internal/worker/strategy_youtube_dash.go` | Wire cipher failure callback |

## Non-Changes

- **Browser stubs:** Goja lacks ES6 Proxy; existing stubs + panic recovery are sufficient
- **Architecture:** No restructuring — the existing full-player + legacy fallback chain is sound
- **Legacy extractor:** Kept as-is (fallback for pre-modern player formats)
- **STS extraction:** Pattern is stable, no changes needed
